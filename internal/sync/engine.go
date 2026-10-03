package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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
	// PrimeiraFalha é o primeiro arquivo que falhou e por quê — exemplo que
	// vai ao painel numa sessão parcial.
	PrimeiraFalha string
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
	// Cancelado na saída: um retorno antecipado (bucket sem versionamento)
	// não pode deixar o walker bloqueado no canal.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fileCh, errCh := Walk(ctx, o.HostRoot, o.SourcePaths, o.ExcludeGlobs)
	result := &Result{}

	for fi := range fileCh {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		entry, dedupHit, err := processFile(ctx, o, fi)
		if errors.Is(err, ErrBucketSemVersionamento) {
			// Não é falha deste arquivo: é do bucket, e vale para todos.
			// Seguir subiria tudo de novo só para nada ser indexado.
			return result, err
		}
		if err != nil {
			slog.Warn("arquivo falhou, pulando",
				"path", fi.RelativePath,
				"err", err)
			result.FilesFailed++
			if result.PrimeiraFalha == "" {
				result.PrimeiraFalha = fi.RelativePath + ": " + err.Error()
			}
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

	// 1. SHA-256 streamed, só dos fi.Size bytes que o walker viu. Arquivo que
	// só cresce (log) seguia sendo lido além disso: o hash cobria bytes que o
	// envio (ContentLength = fi.Size) não manda, e o multipart, que lia até o
	// fim, abortava com ErrArquivoMudou todo dia. Agora o backup é o começo do
	// arquivo, até o tamanho do walk, em todos os caminhos; alteração no lugar
	// dentro desse trecho continua pega pela conferência do hash.
	h := sha256.New()
	if _, err := io.CopyN(h, f, fi.Size); err != nil {
		if errors.Is(err, io.EOF) {
			// Encolheu depois do walk: não há fi.Size bytes para enviar.
			return nil, false, fmt.Errorf("hash: %w", ErrArquivoMudou)
		}
		return nil, false, fmt.Errorf("hash: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	key := path.Join(o.PrefixRoot, fi.RelativePath)

	// 2. Dedup: HeadObject pra ver se o arquivo já existe com mesmo hash
	if existing, ok := checkDedup(ctx, o.S3, o.Bucket, key, hash); ok {
		if existing.VersionID == "" {
			return nil, false, ErrBucketSemVersionamento
		}
		slog.Debug("dedup hit, pulando upload",
			"key", key, "sha256", hash[:8], "size", fi.Size)
		return existing, true, nil
	}

	// 3. Upload novo
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	meta := map[string]string{"sha256": hash}

	if antesDoEnvio != nil {
		antesDoEnvio(fi.AbsolutePath)
	}

	// O hash foi calculado numa primeira leitura; o envio lê o arquivo de
	// novo. Se ele mudou entre as duas (log, banco aberto, planilha salva no
	// meio), o bucket guardaria um conteúdo e o índice outro sha256/tamanho —
	// e a restauração falharia lá na frente com "sha256 mismatch". Por isso o
	// que sobe é hasheado enquanto sobe e comparado com o hash registrado.
	//
	// Na diferença, o arquivo falha (não entra no version_map; a sessão fica
	// parcial e o próximo backup tenta de novo). Registrar o hash do que subiu
	// não serve: o metadado sha256 já foi enviado com o hash antigo, e o
	// conteúdo pode ser uma mistura de antes e depois, que não é uma versão
	// de verdade do arquivo.
	var versionID string
	if fi.Size >= MultipartThresholdBytes {
		// Multipart manual: ContentLength explícito + corpo seekable por parte,
		// sem ChecksumAlgorithm — evita o Content-Encoding aws-chunked que o
		// s3/manager emite e que provedores S3-compat (OCI) não suportam (501).
		versionID, err = uploadMultipart(ctx, o.S3, o.Bucket, key, f, meta, o.MaxMbps, hash, fi.Size)
		if err != nil {
			return nil, false, fmt.Errorf("multipart put %s: %w", key, err)
		}
	} else {
		size := fi.Size
		lido := newLeitorComHash(io.NewSectionReader(f, 0, size))
		putOut, err := o.S3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &o.Bucket,
			Key:           &key,
			Body:          NewThrottledReader(lido, o.MaxMbps),
			Metadata:      meta,
			ContentLength: &size, // S3-compat (OCI) exige Content-Length; sem isso o SDK manda chunked → 411
		})
		if err != nil {
			return nil, false, fmt.Errorf("put %s: %w", key, err)
		}
		if putOut.VersionId != nil {
			versionID = *putOut.VersionId
		}
		if !lido.confere(hash, size) {
			// O objeto já está no bucket, com o metadado sha256 do conteúdo
			// antigo. Deixá-lo lá envenenaria o dedup: se o arquivo voltar ao
			// conteúdo antigo, o HeadObject acharia "mesmo hash" e o índice
			// apontaria para o conteúdo errado. Sai a versão recém-criada.
			removerEnvioInvalido(ctx, o.S3, o.Bucket, key, versionID)
			return nil, false, fmt.Errorf("put %s: %w", key, ErrArquivoMudou)
		}
	}

	if versionID == "" {
		// O objeto subiu, mas sem VersionId: o bucket não versiona. O painel
		// descarta entrada sem version_id, e a sessão terminaria "complete"
		// com zero arquivos indexados — um backup que não restaura nada.
		return nil, false, ErrBucketSemVersionamento
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
func uploadMultipart(ctx context.Context, s3c *s3.Client, bucket, key string, f *os.File, meta map[string]string, maxMbps int, hashEsperado string, tamanho int64) (string, error) {
	partSize := tamanhoDaParte(tamanho)

	create, err := s3c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		Metadata: meta,
	})
	if err != nil {
		return "", fmt.Errorf("create multipart: %w", err)
	}
	uploadID := create.UploadId

	// O abort usa um contexto desligado do cancelamento: a falha mais comum
	// aqui é justamente o serviço parando no meio (ctx cancelado), e com o
	// ctx original o abort nem saía — as partes ficavam órfãs no bucket,
	// cobradas até alguma regra de ciclo de vida limpar.
	abort := func() {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := s3c.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: &bucket, Key: &key, UploadId: uploadID,
		}); err != nil {
			slog.Warn("abort do multipart falhou; partes podem ter ficado no bucket", "key", key, "err", err)
		}
	}

	// Lê só os tamanho bytes que foram hasheados: o que o arquivo cresceu
	// depois (log aberto) fica para o próximo backup, em vez de mudar o que
	// sobe e abortar o envio.
	r := io.LimitReader(f, tamanho)
	var parts []s3types.CompletedPart
	buf := make([]byte, partSize)
	// O que sobe é exatamente o que passa por buf (o SDK reenvia o mesmo
	// buf nas novas tentativas): hasheado aqui, confere com o hash registrado.
	enviado := sha256.New()
	var bytesEnviados int64
	for partNum := int32(1); ; partNum++ {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			enviado.Write(buf[:n])
			bytesEnviados += int64(n)
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
	if bytesEnviados != tamanho || hex.EncodeToString(enviado.Sum(nil)) != hashEsperado {
		// Mudou durante o envio: aborta antes do Complete, e nada chega a
		// existir no bucket.
		abort()
		return "", ErrArquivoMudou
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

const (
	parteMinima  = 16 * 1024 * 1024 // 16 MiB
	maximoPartes = 10000            // limite do S3 por upload multipart
)

// tamanhoDaParte escolhe o tamanho das partes do multipart.
//
// Era fixo em 16 MiB: acima de ~156 GiB o arquivo passava das 10.000 partes
// que o S3 aceita, e o upload falhava na parte 10.001 depois de horas. Agora é
// o maior entre 16 MiB e tamanho/10.000, arredondado para cima em MiB.
func tamanhoDaParte(tamanho int64) int64 {
	const mib = 1024 * 1024
	p := (tamanho + maximoPartes - 1) / maximoPartes
	p = (p + mib - 1) / mib * mib
	return max(p, parteMinima)
}

// ErrArquivoMudou: o conteúdo enviado não é o que foi hasheado — o arquivo foi
// alterado durante o backup. O arquivo falha nesta sessão e volta na próxima.
var ErrArquivoMudou = errors.New("arquivo mudou durante o envio (o conteúdo enviado não confere com o sha256 calculado); fica para o próximo backup")

// ErrBucketSemVersionamento: o bucket devolveu objeto sem VersionId. Sem
// versão não há o que indexar nem restaurar; a sessão inteira falha com este
// erro em vez de terminar "complete" sem nada indexado.
var ErrBucketSemVersionamento = errors.New("bucket sem versionamento: ative o versionamento do bucket (o objeto subiu sem VersionId e o backup não seria restaurável)")

// antesDoEnvio é um gancho só para teste: roda entre o hash e o envio, o
// intervalo em que um arquivo alterado no lugar fazia subir conteúdo diferente
// do registrado.
var antesDoEnvio func(path string)

// leitorComHash hasheia o que o SDK lê para enviar. O SDK pode ler o corpo
// mais de uma vez (assinatura do payload, nova tentativa), sempre voltando ao
// início com Seek: a volta ao zero recomeça o hash, e vale a última leitura
// completa — a que foi enviada. Um Seek para o meio invalida a conferência
// até a próxima volta ao zero.
type leitorComHash struct {
	r        io.ReadSeeker
	h        hash.Hash
	n        int64
	pos      int64
	desviado bool
}

func newLeitorComHash(r io.ReadSeeker) *leitorComHash {
	return &leitorComHash{r: r, h: sha256.New()}
}

func (l *leitorComHash) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.h.Write(p[:n])
		l.n += int64(n)
		l.pos += int64(n)
	}
	return n, err
}

func (l *leitorComHash) Seek(offset int64, whence int) (int64, error) {
	pos, err := l.r.Seek(offset, whence)
	if err != nil {
		return pos, err
	}
	switch {
	case pos == 0:
		l.h.Reset()
		l.n = 0
		l.desviado = false
	case pos != l.pos:
		l.desviado = true
	}
	l.pos = pos
	return pos, nil
}

// confere diz se a última leitura completa tem o hash e o tamanho esperados.
func (l *leitorComHash) confere(hashEsperado string, tamanho int64) bool {
	return !l.desviado && l.n == tamanho && hex.EncodeToString(l.h.Sum(nil)) == hashEsperado
}

// removerEnvioInvalido apaga a versão que acabou de subir com conteúdo que não
// confere. Com o contexto desligado do cancelamento: o serviço parando no meio
// não pode deixar o objeto envenenado para trás. Sem VersionId (bucket sem
// versionamento) o objeto atual é o recém-enviado, que já sobrescreveu o
// anterior — apagá-lo é melhor que deixar um conteúdo com hash errado.
func removerEnvioInvalido(ctx context.Context, s3c *s3.Client, bucket, key, versionID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	in := &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}
	if versionID != "" {
		in.VersionId = &versionID
	}
	if _, err := s3c.DeleteObject(ctx, in); err != nil {
		slog.Warn("não consegui apagar o envio que não confere", "key", key, "version_id", versionID, "err", err)
	}
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
