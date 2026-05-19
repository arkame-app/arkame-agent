package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// EngineOptions configura uma execução de sync.
type EngineOptions struct {
	S3           *s3.Client
	Bucket       string
	PrefixRoot   string   // "data/<agent-ulid>/"
	HostRoot     string   // raiz do fs (em Docker: /host)
	SourcePaths  []string // paths do plano
	ExcludeGlobs []string
	MaxMbps      int // throttle
}

// Result é o retorno do Run — montado no formato que o painel espera em SessionComplete.
type Result struct {
	Stats      api.SessionStats
	VersionMap []api.FileEntry
}

// Run executa o sync de um plano.
//
// Fluxo:
//  1. Walk(hostRoot, sourcePaths, excludeGlobs) → stream de FileInfo
//  2. Para cada arquivo:
//     a) Calcula SHA-256
//     b) Se já existe no bucket com mesmo hash → pula (dedup)
//     c) PutObject com throttle → recebe VersionId do S3
//     d) Append em version_map
//  3. Retorna stats + version_map — caller envia ao painel via SessionComplete
//
// IMPORTANTE: esta é uma implementação skeleton. O "c) PutObject" está
// simplificado (single-part). Para arquivos grandes, trocar por
// CreateMultipartUpload + UploadPart (em TODO abaixo).
func Run(ctx context.Context, o EngineOptions) (*Result, error) {
	fileCh, errCh := Walk(ctx, o.HostRoot, o.SourcePaths, o.ExcludeGlobs)
	result := &Result{}

	for fi := range fileCh {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		entry, err := processFile(ctx, o, fi)
		if err != nil {
			slog.Warn("arquivo falhou, pulando",
				"path", fi.RelativePath,
				"err", err)
			continue
		}
		result.Stats.FilesTotal++
		result.Stats.BytesTotal += fi.Size
		if entry != nil {
			result.Stats.FilesUploaded++
			result.Stats.BytesUploaded += fi.Size
			result.VersionMap = append(result.VersionMap, *entry)
		}
	}
	if err, ok := <-errCh; ok && err != nil {
		return result, err
	}
	return result, nil
}

// processFile sobe um arquivo individual. Retorna nil se arquivo não mudou
// (dedup file-level) ou FileEntry se um novo upload foi feito.
//
// TODO: implementar dedup checando HeadObject no bucket + comparando sha256.
// TODO: trocar PutObject por Multipart para arquivos > 100 MB.
func processFile(ctx context.Context, o EngineOptions, fi FileInfo) (*api.FileEntry, error) {
	f, err := os.Open(fi.AbsolutePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Calcula SHA-256 streamed (e em paralelo lê pro upload)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hash: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	// Reabre para upload (poderíamos usar io.TeeReader + pipe mas simplifica)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	key := path.Join(o.PrefixRoot, fi.RelativePath)
	body := NewThrottledReader(f, o.MaxMbps)

	putOut, err := o.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   &o.Bucket,
		Key:      &key,
		Body:     body,
		Metadata: map[string]string{"sha256": hash},
	})
	if err != nil {
		return nil, fmt.Errorf("put %s: %w", key, err)
	}

	versionID := ""
	if putOut.VersionId != nil {
		versionID = *putOut.VersionId
	}

	return &api.FileEntry{
		Key:        key,
		VersionID:  versionID,
		Size:       fi.Size,
		SHA256:     hash,
		ModifiedAt: time.Unix(0, fi.ModTime),
	}, nil
}
