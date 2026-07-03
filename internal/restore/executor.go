// Package restore implementa o executor de restore: dado um RestoreItem,
// baixa o objeto do bucket (GetObject com VersionId), valida sha256 e
// escreve atomicamente em destPath/destFilename com conflict resolution.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// MultipartThresholdBytes — arquivos > 100 MB usam s3manager.Downloader (multipart paralelo)
const MultipartThresholdBytes = 100 * 1024 * 1024

// ErrWarmingRequested indica que o objeto está em cold storage e o executor
// já disparou um RestoreObject pra trazê-lo. O caller deve marcar warming_state
// e tentar de novo numa próxima rodada do loop.
type ErrWarmingRequested struct {
	StorageClass string
	Tier         string
	Bucket       string
	Key          string
}

func (e *ErrWarmingRequested) Error() string {
	return fmt.Sprintf("warming requested: %s/%s (class=%s, tier=%s)", e.Bucket, e.Key, e.StorageClass, e.Tier)
}

// ErrWarmingInProgress indica que o objeto já está sendo warmed mas ainda não
// está disponível. Não dispara novo RestoreObject, só espera.
type ErrWarmingInProgress struct {
	StorageClass string
	Bucket       string
	Key          string
}

func (e *ErrWarmingInProgress) Error() string {
	return fmt.Sprintf("warming in progress: %s/%s (class=%s)", e.Bucket, e.Key, e.StorageClass)
}

// Options configura o executor.
type Options struct {
	S3       *s3.Client
	HostRoot string // raiz (em container Docker = "/host"; standalone = "/")
}

// Run baixa um item do bucket e escreve no destino.
//
// Estratégia de conflito (item.ConflictStrategy):
//   - "suffix-version": se o arquivo existe, escreve como "<name>.v<versionId8>.<ext>"
//   - "overwrite": sobrescreve sem aviso
//   - "skip": não escreve, retorna nil
//
// Em qualquer estratégia, a escrita é atômica: baixa pra arquivo temp no mesmo
// diretório e renomeia ao final. Se falhar no meio, o temp é removido.
//
// Valida SHA-256 do conteúdo baixado contra item.SourceSha256 antes de renomear.
func Run(ctx context.Context, opts Options, item api.RestoreItem) error {
	if opts.S3 == nil {
		return errors.New("s3 client é obrigatório")
	}
	if item.Bucket == "" || item.SourceKey == "" {
		return errors.New("item sem bucket ou source_key")
	}
	if !strings.HasPrefix(item.DestPath, "/") {
		return fmt.Errorf("dest_path deve ser absoluto: %q", item.DestPath)
	}
	// dest_filename pode conter subdiretórios relativos (restore de pasta/snapshot
	// preserva a estrutura), mas nunca path absoluto, "..", ou backslash.
	subDir, baseName, err := splitDestFilename(item.DestFilename)
	if err != nil {
		return err
	}

	rootedDir := filepath.Join(opts.HostRoot, item.DestPath, filepath.FromSlash(subDir))
	if err := os.MkdirAll(rootedDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rootedDir, err)
	}

	finalName, err := resolveConflict(rootedDir, baseName, item.SourceVersionID, item.ConflictStrategy)
	if err != nil {
		return err
	}
	if finalName == "" {
		// skip
		return nil
	}
	finalPath := filepath.Join(rootedDir, finalName)

	tmpFile, err := os.CreateTemp(rootedDir, ".arkame-restore-*."+finalName)
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	written, hashErr := downloadObject(ctx, opts.S3, item, tmpFile)
	if cerr := tmpFile.Close(); cerr != nil && hashErr == nil {
		hashErr = cerr
	}
	if hashErr != nil {
		var iose *s3types.InvalidObjectState
		if errors.As(hashErr, &iose) {
			return handleColdStorage(ctx, opts.S3, item, iose)
		}
		return hashErr
	}

	if item.SourceSize > 0 && written.bytes != item.SourceSize {
		return fmt.Errorf("size mismatch: esperado=%d baixado=%d", item.SourceSize, written.bytes)
	}
	if item.SourceSha256 != "" && !strings.EqualFold(written.sha256, item.SourceSha256) {
		return fmt.Errorf("sha256 mismatch: esperado=%s baixado=%s", item.SourceSha256, written.sha256)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename tmp → final: %w", err)
	}
	cleanup = false
	_ = os.Chtimes(finalPath, time.Now(), time.Now())
	return nil
}

type downloadResult struct {
	bytes  int64
	sha256 string
}

// downloadObject baixa o objeto e calcula SHA-256.
//   - Arquivos < 100MB: GetObject streamed + io.MultiWriter
//   - Arquivos >= 100MB: s3manager.Downloader (multipart paralelo), depois
//     re-lê o tmp pra calcular sha256
//
// O segundo caminho lê o disco 2x; trade-off pra ter parallel multipart sem
// implementar próprio fan-out + reorder de chunks.
func downloadObject(
	ctx context.Context,
	s3c *s3.Client,
	item api.RestoreItem,
	tmpFile *os.File,
) (downloadResult, error) {
	if item.SourceSize >= MultipartThresholdBytes {
		return downloadMultipart(ctx, s3c, item, tmpFile)
	}
	return downloadStream(ctx, s3c, item, tmpFile)
}

func downloadStream(ctx context.Context, s3c *s3.Client, item api.RestoreItem, w io.Writer) (downloadResult, error) {
	in := &s3.GetObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		in.VersionId = &item.SourceVersionID
	}
	out, err := s3c.GetObject(ctx, in)
	if err != nil {
		return downloadResult{}, fmt.Errorf("s3 GetObject %s@%s: %w", item.SourceKey, item.SourceVersionID, err)
	}
	defer out.Body.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), out.Body)
	if err != nil {
		return downloadResult{}, fmt.Errorf("write tmp: %w", err)
	}
	return downloadResult{bytes: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

func downloadMultipart(ctx context.Context, s3c *s3.Client, item api.RestoreItem, tmpFile *os.File) (downloadResult, error) {
	in := &s3.GetObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		in.VersionId = &item.SourceVersionID
	}

	dl := manager.NewDownloader(s3c, func(d *manager.Downloader) {
		d.Concurrency = 4
		d.PartSize = 16 * 1024 * 1024 // 16 MiB
	})
	n, err := dl.Download(ctx, tmpFile, in)
	if err != nil {
		return downloadResult{}, fmt.Errorf("s3 multipart download %s@%s: %w", item.SourceKey, item.SourceVersionID, err)
	}

	// Re-lê pra hash (Downloader não permite TeeWriter)
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return downloadResult{}, fmt.Errorf("seek tmp: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, tmpFile); err != nil {
		return downloadResult{}, fmt.Errorf("hash tmp: %w", err)
	}
	return downloadResult{bytes: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// handleColdStorage é chamado quando GetObject retorna InvalidObjectState
// (objeto em GLACIER ou DEEP_ARCHIVE). Verifica via HeadObject se já há restore
// em andamento; se não, dispara RestoreObject.
//
// Tier Standard é usado por default (3-5h em GLACIER, 12h em DEEP_ARCHIVE).
// Para urgência, mudar pra Expedited (1-5min, mais caro) via item.metadata futuro.
func handleColdStorage(
	ctx context.Context,
	s3c *s3.Client,
	item api.RestoreItem,
	iose *s3types.InvalidObjectState,
) error {
	storageClass := ""
	if iose.StorageClass != "" {
		storageClass = string(iose.StorageClass)
	}

	// HeadObject pra checar x-amz-restore header
	headIn := &s3.HeadObjectInput{Bucket: &item.Bucket, Key: &item.SourceKey}
	if item.SourceVersionID != "" {
		headIn.VersionId = &item.SourceVersionID
	}
	head, herr := s3c.HeadObject(ctx, headIn)
	if herr == nil && head.Restore != nil {
		// "ongoing-request=\"true\"" → ainda warming
		// "ongoing-request=\"false\", expiry-date=..." → ready, mas GetObject falhou:
		//   improvável (raça), trate como warming-in-progress de qualquer forma
		if strings.Contains(*head.Restore, `ongoing-request="true"`) {
			return &ErrWarmingInProgress{
				StorageClass: storageClass,
				Bucket:       item.Bucket,
				Key:          item.SourceKey,
			}
		}
	}

	// Sem restore em andamento → dispara
	tier := s3types.TierStandard
	restoreIn := &s3.RestoreObjectInput{
		Bucket: &item.Bucket,
		Key:    &item.SourceKey,
		RestoreRequest: &s3types.RestoreRequest{
			Days: aws32(7),
			GlacierJobParameters: &s3types.GlacierJobParameters{
				Tier: tier,
			},
		},
	}
	if item.SourceVersionID != "" {
		restoreIn.VersionId = &item.SourceVersionID
	}
	if _, rerr := s3c.RestoreObject(ctx, restoreIn); rerr != nil {
		// RestoreAlreadyInProgress → warming já tá rolando, ok
		var raip *s3types.ObjectAlreadyInActiveTierError
		if errors.As(rerr, &raip) {
			return &ErrWarmingInProgress{
				StorageClass: storageClass,
				Bucket:       item.Bucket,
				Key:          item.SourceKey,
			}
		}
		return fmt.Errorf("s3 RestoreObject %s@%s: %w", item.SourceKey, item.SourceVersionID, rerr)
	}

	return &ErrWarmingRequested{
		StorageClass: storageClass,
		Tier:         string(tier),
		Bucket:       item.Bucket,
		Key:          item.SourceKey,
	}
}

// aws32 retorna ponteiro pra int32 (helper, equivalente ao aws.Int32 do SDK).
func aws32(n int32) *int32 { return &n }

// splitDestFilename valida e separa o dest_filename em subdiretório relativo
// (POSIX, pode ser vazio) e nome-base. Rejeita path absoluto, backslash, "..",
// e segmentos vazios — o destino final fica sempre contido em dest_path.
func splitDestFilename(destFilename string) (subDir, baseName string, err error) {
	if destFilename == "" || strings.Contains(destFilename, "\\") || strings.HasPrefix(destFilename, "/") {
		return "", "", fmt.Errorf("dest_filename inválido: %q", destFilename)
	}
	segments := strings.Split(destFilename, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("dest_filename inválido: %q", destFilename)
		}
	}
	baseName = segments[len(segments)-1]
	subDir = strings.Join(segments[:len(segments)-1], "/")
	return subDir, baseName, nil
}

// resolveConflict decide o nome final a usar dado o arquivo de destino.
// Retorna nome vazio se a estratégia for "skip" e o arquivo já existir.
func resolveConflict(dir, filename, versionID, strategy string) (string, error) {
	full := filepath.Join(dir, filename)
	if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
		return filename, nil
	} else if err != nil {
		return "", fmt.Errorf("stat %s: %w", full, err)
	}

	switch strategy {
	case "overwrite":
		return filename, nil
	case "skip":
		return "", nil
	case "suffix-version", "":
		short := versionID
		if len(short) > 8 {
			short = short[:8]
		}
		if short == "" {
			short = time.Now().UTC().Format("20060102T150405")
		}
		ext := filepath.Ext(filename)
		base := strings.TrimSuffix(filename, ext)
		// se já com sufixo (idempotência), incrementa contador
		candidate := fmt.Sprintf("%s.v%s%s", base, short, ext)
		for i := 1; ; i++ {
			full := filepath.Join(dir, candidate)
			if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
				return candidate, nil
			}
			candidate = fmt.Sprintf("%s.v%s.%d%s", base, short, i, ext)
			if i > 1000 {
				return "", fmt.Errorf("conflict resolution exaurido em %s", dir)
			}
		}
	default:
		return "", fmt.Errorf("conflict_strategy desconhecida: %q", strategy)
	}
}
