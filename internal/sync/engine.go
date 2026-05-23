package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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

		entry, dedupHit, err := processFile(ctx, o, fi)
		if err != nil {
			slog.Warn("arquivo falhou, pulando",
				"path", fi.RelativePath,
				"err", err)
			continue
		}
		result.Stats.FilesTotal++
		result.Stats.BytesTotal += fi.Size
		if entry != nil {
			result.VersionMap = append(result.VersionMap, *entry)
			if !dedupHit {
				result.Stats.FilesUploaded++
				result.Stats.BytesUploaded += fi.Size
			}
		}
	}
	if err, ok := <-errCh; ok && err != nil {
		return result, err
	}
	return result, nil
}

// processFile sobe um arquivo individual.
//
// Fluxo:
//   1. Calcula SHA-256 do arquivo local
//   2. HeadObject no bucket — se já existe com mesmo sha256 metadata, é dedup
//      (mesmo hash = mesmo conteúdo, S3 já tem). Retorna FileEntry com o
//      VersionId existente — o painel registra como se fosse novo upload (a
//      version aparece no version_map atual mas referencia o objeto antigo).
//   3. Senão, PutObject novo com sha256 no metadata.
//
// TODO: trocar PutObject por Multipart para arquivos > 100 MB.
func processFile(ctx context.Context, o EngineOptions, fi FileInfo) (*api.FileEntry, bool, error) {
	f, err := os.Open(fi.AbsolutePath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	// 1. SHA-256 streamed
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, false, fmt.Errorf("hash: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	key := path.Join(o.PrefixRoot, fi.RelativePath)

	// 2. Dedup: HeadObject pra ver se o arquivo já existe com mesmo hash
	if existing, ok := checkDedup(ctx, o.S3, o.Bucket, key, hash); ok {
		slog.Debug("dedup hit, pulando upload",
			"key", key, "sha256", hash[:8], "size", fi.Size)
		return existing, true, nil
	}

	// 3. Upload novo
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	body := NewThrottledReader(f, o.MaxMbps)

	putOut, err := o.S3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   &o.Bucket,
		Key:      &key,
		Body:     body,
		Metadata: map[string]string{"sha256": hash},
	})
	if err != nil {
		return nil, false, fmt.Errorf("put %s: %w", key, err)
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
	}, false, nil
}

// checkDedup retorna (entry, true) se o objeto já existe no bucket com o
// mesmo sha256 metadata. Caso contrário (ou erro de Head), retorna (nil, false)
// pra que o caller faça o upload novo.
//
// IMPORTANTE: comparamos por sha256 do metadata, não por ETag (que pode ser MD5
// composto em multipart e não bate com sha256). Backups antigos sem o metadata
// "sha256" não dão dedup (fallback seguro: re-upload).
func checkDedup(ctx context.Context, s3c *s3.Client, bucket, key, hash string) (*api.FileEntry, bool) {
	head, err := s3c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		var nf *s3types.NotFound
		if !errors.As(err, &nf) {
			slog.Debug("HeadObject erro, fazendo upload", "key", key, "err", err)
		}
		return nil, false
	}
	existingHash, ok := head.Metadata["sha256"]
	if !ok || existingHash != hash {
		return nil, false
	}
	versionID := ""
	if head.VersionId != nil {
		versionID = *head.VersionId
	}
	size := int64(0)
	if head.ContentLength != nil {
		size = *head.ContentLength
	}
	mtime := time.Now()
	if head.LastModified != nil {
		mtime = *head.LastModified
	}
	return &api.FileEntry{
		Key:        key,
		VersionID:  versionID,
		Size:       size,
		SHA256:     hash,
		ModifiedAt: mtime,
	}, true
}
