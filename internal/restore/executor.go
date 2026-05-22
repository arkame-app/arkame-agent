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
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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
	if strings.ContainsAny(item.DestFilename, "/\\") || item.DestFilename == "" {
		return fmt.Errorf("dest_filename inválido: %q", item.DestFilename)
	}

	rootedDir := filepath.Join(opts.HostRoot, item.DestPath)
	if err := os.MkdirAll(rootedDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rootedDir, err)
	}

	finalName, err := resolveConflict(rootedDir, item.DestFilename, item.SourceVersionID, item.ConflictStrategy)
	if err != nil {
		return err
	}
	if finalName == "" {
		// skip
		return nil
	}
	finalPath := filepath.Join(rootedDir, finalName)

	getInput := &s3.GetObjectInput{
		Bucket: &item.Bucket,
		Key:    &item.SourceKey,
	}
	if item.SourceVersionID != "" {
		getInput.VersionId = &item.SourceVersionID
	}

	getOut, err := opts.S3.GetObject(ctx, getInput)
	if err != nil {
		return fmt.Errorf("s3 GetObject %s@%s: %w", item.SourceKey, item.SourceVersionID, err)
	}
	defer getOut.Body.Close()

	tmpFile, err := os.CreateTemp(rootedDir, ".arkame-restore-*."+finalName)
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmpFile.Name()
	// se algo der errado depois daqui, garante limpeza
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	mw := io.MultiWriter(tmpFile, h)
	written, err := io.Copy(mw, getOut.Body)
	if cerr := tmpFile.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}

	if item.SourceSize > 0 && written != item.SourceSize {
		return fmt.Errorf("size mismatch: esperado=%d baixado=%d", item.SourceSize, written)
	}

	gotHash := hex.EncodeToString(h.Sum(nil))
	if item.SourceSha256 != "" && !strings.EqualFold(gotHash, item.SourceSha256) {
		return fmt.Errorf("sha256 mismatch: esperado=%s baixado=%s", item.SourceSha256, gotHash)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename tmp → final: %w", err)
	}
	cleanup = false
	// touch mtime pro registro (não-crítico se falhar)
	_ = os.Chtimes(finalPath, time.Now(), time.Now())
	return nil
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
