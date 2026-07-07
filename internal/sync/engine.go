package sync

import (
	"bytes"
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

// MultipartThresholdBytes — arquivos >= 100 MB sobem via s3manager.Uploader
// (multipart paralelo), simétrico ao download no restore. Abaixo disso,
// PutObject simples basta.
const MultipartThresholdBytes = 100 * 1024 * 1024

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
	Stats       api.SessionStats
	VersionMap  []api.FileEntry
	FilesFailed int // arquivos que falharam no upload (pulados); usado p/ decidir partial/failed
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
			result.FilesFailed++
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
//  1. Calcula SHA-256 do arquivo local
//  2. HeadObject no bucket — se já existe com mesmo sha256 metadata, é dedup
//     (mesmo hash = mesmo conteúdo, S3 já tem). Retorna FileEntry com o
//     VersionId existente — o painel registra como se fosse novo upload (a
//     version aparece no version_map atual mas referencia o objeto antigo).
//  3. Senão, PutObject novo com sha256 no metadata.
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
	meta := map[string]string{"sha256": hash}

	var versionID string
	if fi.Size >= MultipartThresholdBytes {
		// Multipart manual: ContentLength explícito + corpo seekable por parte,
		// sem ChecksumAlgorithm — evita o Content-Encoding aws-chunked que o
		// s3/manager emite e que provedores S3-compat (OCI) não suportam (501).
		versionID, err = uploadMultipart(ctx, o.S3, o.Bucket, key, f, meta, o.MaxMbps)
		if err != nil {
			return nil, false, fmt.Errorf("multipart put %s: %w", key, err)
		}
	} else {
		size := fi.Size
		putOut, err := o.S3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &o.Bucket,
			Key:           &key,
			Body:          NewThrottledReader(f, o.MaxMbps),
			Metadata:      meta,
			ContentLength: &size, // S3-compat (OCI) exige Content-Length; sem isso o SDK manda chunked → 411
		})
		if err != nil {
			return nil, false, fmt.Errorf("put %s: %w", key, err)
		}
		if putOut.VersionId != nil {
			versionID = *putOut.VersionId
		}
	}

	return &api.FileEntry{
		Key:        key,
		VersionID:  versionID,
		Size:       fi.Size,
		SHA256:     hash,
		ModifiedAt: time.Unix(0, fi.ModTime),
	}, false, nil
}

// uploadMultipart sobe um arquivo grande em partes (sequenciais) via multipart
// manual. Cada UploadPart usa um corpo seekable (bytes.Reader, opcionalmente
// throttled) + ContentLength explícito e SEM ChecksumAlgorithm, evitando o
// Content-Encoding aws-chunked que o s3/manager emite — não suportado por
// provedores S3-compat como a OCI (501 NotImplemented). Em qualquer falha,
// aborta o multipart pra não deixar partes órfãs no bucket.
func uploadMultipart(ctx context.Context, s3c *s3.Client, bucket, key string, f *os.File, meta map[string]string, maxMbps int) (string, error) {
	const partSize = 16 * 1024 * 1024 // 16 MiB

	create, err := s3c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		Metadata: meta,
	})
	if err != nil {
		return "", fmt.Errorf("create multipart: %w", err)
	}
	uploadID := create.UploadId

	abort := func() {
		_, _ = s3c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: &bucket, Key: &key, UploadId: uploadID,
		})
	}

	var parts []s3types.CompletedPart
	buf := make([]byte, partSize)
	for partNum := int32(1); ; partNum++ {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			pn := partNum
			cl := int64(n)
			out, uerr := s3c.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        &bucket,
				Key:           &key,
				UploadId:      uploadID,
				PartNumber:    &pn,
				ContentLength: &cl,
				Body:          NewThrottledReader(bytes.NewReader(buf[:n]), maxMbps),
			})
			if uerr != nil {
				abort()
				return "", fmt.Errorf("upload parte %d: %w", partNum, uerr)
			}
			parts = append(parts, s3types.CompletedPart{ETag: out.ETag, PartNumber: &pn})
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			abort()
			return "", fmt.Errorf("lendo parte %d: %w", partNum, rerr)
		}
	}

	if len(parts) == 0 {
		abort()
		return "", fmt.Errorf("nenhuma parte para enviar")
	}

	comp, err := s3c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          &bucket,
		Key:             &key,
		UploadId:        uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()
		return "", fmt.Errorf("complete multipart: %w", err)
	}
	if comp.VersionId != nil {
		return *comp.VersionId, nil
	}
	return "", nil
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
