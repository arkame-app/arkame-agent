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
	"io/fs"
	"log/slog"
	"os"
	"path"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/caminho"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/time/rate"
)

// MultipartThresholdBytes — arquivos >= 100 MB sobem via s3manager.Uploader
// (multipart paralelo), simétrico ao download no restore. Abaixo disso,
// PutObject simples basta.
const MultipartThresholdBytes = 100 * 1024 * 1024

// limiarDoMultipart é o MultipartThresholdBytes; os testes o baixam.
var limiarDoMultipart int64 = MultipartThresholdBytes

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
	// O que abriu tem de ser um arquivo comum, como o que a varredura viu.
	// Trocado no meio por um link para /dev/sda1, a leitura levaria o disco
	// cru, com /etc/shadow dentro (revisão, 18/10). Vale para todo plano.
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !arquivoComum(st) {
		return nil, false, fmt.Errorf("abrindo %s: %w", fi.RelativePath, ErrArquivoMudou)
	}
	// /proc e /sys: "arquivos" que são a memória e o estado do kernel
	// (/proc/kcore), regulares para o sistema. Por um link trocado depois da
	// varredura, ou num procfs montado dentro de um chroot da origem.
	if doKernel(f) {
		return nil, false, ErrDoKernel
	}
	if o.Restrito {
		// No plano do MCP, também o mesmo arquivo (dispositivo e inode). Nos
		// outros, um arquivo salvo por "grava e renomeia" durante o backup
		// é lido na versão nova, como sempre foi.
		if !mesmoArquivo(fi.visto, st) {
			return nil, false, fmt.Errorf("abrindo %s: %w", fi.RelativePath, ErrArquivoMudou)
		}
		if ligacaoDeArquivoFechado(st, donoDaPasta(fi.AbsolutePath)) {
			return nil, false, ErrCaminhoRestrito
		}
		if o.Leitura == nil {
			if err := conferirAberto(o.HostRoot, f); err != nil {
				return nil, false, err
			}
		}
	}

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
		// O índice guarda a data do arquivo, não a do objeto: o LastModified
		// é quando a versão subiu (às vezes semanas antes), e a restauração
		// por data e a tela do arquivo mostravam a data do envio.
		existing.ModifiedAt = dataParaOPainel(fi.ModTime)
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
	// O primeiro envio da execução vai sozinho (portaoDoVersionamento): com
	// vários trabalhadores, um bucket suspenso teria várias versões "null"
	// sobrescritas antes de o primeiro envio acusar.
	versionID, err := o.portao.com(func() (string, error) {
		return enviarArquivo(ctx, o, fi, f, key, hash, meta)
	})
	if err != nil {
		return nil, false, err
	}

	return &api.FileEntry{
		Key:        key,
		VersionID:  versionID,
		Size:       fi.Size,
		SHA256:     hash,
		ModifiedAt: dataParaOPainel(fi.ModTime),
	}, false, nil
}

// enviarArquivo sobe o conteúdo já hasheado e devolve o VersionId do
// provedor. Bucket sem versionamento ou suspenso é erro, não versão.
func enviarArquivo(ctx context.Context, o EngineOptions, fi FileInfo, f *os.File, key, hash string, meta map[string]string) (string, error) {
	var versionID string
	var err error
	if fi.Size >= limiarDoMultipart {
		// Multipart manual: ContentLength explícito + corpo seekable por parte,
		// sem ChecksumAlgorithm — evita o Content-Encoding aws-chunked que o
		// s3/manager emite e que provedores S3-compat (OCI) não suportam (501).
		// No máximo dois multiparts ao mesmo tempo: cada um segura um buffer
		// do tamanho da parte (16 MiB ou mais).
		if o.multiparts != nil {
			select {
			case o.multiparts <- struct{}{}:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		versionID, err = uploadMultipartCom(ctx, o.S3, o.Bucket, key, f, meta, o.limitador(), hash, fi.Size)
		if o.multiparts != nil {
			<-o.multiparts
		}
		if err != nil {
			return "", fmt.Errorf("multipart put %s: %w", key, err)
		}
	} else {
		size := fi.Size
		lido := newLeitorComHash(io.NewSectionReader(f, 0, size))
		putOut, err := o.S3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &o.Bucket,
			Key:           &key,
			Body:          leitorLimitado(ctx, lido, o.limitador()),
			Metadata:      meta,
			ContentLength: &size, // S3-compat (OCI) exige Content-Length; sem isso o SDK manda chunked → 411
		})
		if err != nil {
			return "", fmt.Errorf("put %s: %w", key, err)
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
			return "", fmt.Errorf("put %s: %w", key, ErrArquivoMudou)
		}
	}

	if versionID == "" {
		// O objeto subiu, mas sem VersionId: o bucket não versiona. O painel
		// descarta entrada sem version_id, e a sessão terminaria "complete"
		// com zero arquivos indexados — um backup que não restaura nada.
		return "", ErrBucketSemVersionamento
	}
	if versionID == "null" {
		// Versionamento suspenso: o envio grava a versão "null", que
		// sobrescreve a "null" anterior — cada backup apaga o de antes, e o
		// índice apontaria todas as datas para o mesmo conteúdo. Só o envio
		// conta: a versão "null" que o dedup reaproveita é de antes de o
		// versionamento ser ativado, e é legítima.
		return "", ErrVersionamentoSuspenso
	}

	return versionID, nil
}

// portaoDoVersionamento faz o primeiro envio de uma execução acontecer
// sozinho: enquanto nenhum envio voltou com um VersionId de verdade, os
// outros esperam. Se o primeiro acusa bucket sem versionamento (ou
// suspenso), ninguém mais envia.
type portaoDoVersionamento struct {
	mu      gosync.Mutex
	provado atomic.Bool
	negado  error
}

func (p *portaoDoVersionamento) com(enviar func() (string, error)) (string, error) {
	if p == nil || p.provado.Load() {
		return enviar()
	}
	p.mu.Lock()
	if p.provado.Load() {
		p.mu.Unlock()
		return enviar()
	}
	defer p.mu.Unlock()
	if p.negado != nil {
		return "", p.negado
	}
	v, err := enviar()
	switch {
	case err == nil:
		p.provado.Store(true)
	case errors.Is(err, ErrBucketSemVersionamento):
		p.negado = err
	}
	return v, err
}

// uploadMultipartCom sobe um arquivo grande em partes (sequenciais) via
// multipart manual, com o limitador de banda dado (o da execução). Cada
// UploadPart usa um corpo seekable (bytes.Reader, opcionalmente throttled) +
// ContentLength explícito e SEM ChecksumAlgorithm, evitando o
// Content-Encoding aws-chunked que o s3/manager emite — não suportado por
// provedores S3-compat como a OCI (501 NotImplemented). Em qualquer falha,
// aborta o multipart pra não deixar partes órfãs no bucket.
func uploadMultipartCom(ctx context.Context, s3c *s3.Client, bucket, key string, f *os.File, meta map[string]string, lim *rate.Limiter, hashEsperado string, tamanho int64) (string, error) {
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
				Body:          leitorLimitado(ctx, bytes.NewReader(buf[:n]), lim),
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

// ErrDoKernel: o arquivo aberto está num sistema de arquivos do kernel
// (procfs, sysfs). Fica de fora calado: não é dado de backup.
var ErrDoKernel = errors.New("pseudoarquivo do kernel (/proc, /sys), fora do backup")

// ErrCaminhoRestrito: num plano do MCP, o arquivo aberto está numa pasta com
// chaves ou senhas, ou não deu para saber onde ele está. Fica de fora sem
// contar como falha (RestrictedSkipped).
var ErrCaminhoRestrito = errors.New("arquivo numa pasta com chaves ou senhas do sistema; um plano criado pelo assistente (MCP) não o lê")

// conferirAberto confere o arquivo pelo que foi de fato aberto, e não pelo
// caminho da varredura: entre uma e outra, uma pasta no meio pode ter virado
// link (Unix) ou junção (Windows, sem privilégio) para /root/.ssh. Com
// instantâneo não se aplica: a leitura vem de uma cópia que não muda.
//
// Não saber onde o arquivo está não é o mesmo que ele ser sensível: é falha
// do arquivo (a sessão fica parcial e nada vira remoção no catálogo), e não
// um item pulado, que numa sessão completa tiraria do catálogo tudo o que
// estava guardado (revisão, 18/10).
func conferirAberto(hostRoot string, f *os.File) error {
	final, err := caminho.DoAberto(f)
	if err != nil {
		return fmt.Errorf("não consegui conferir onde está o arquivo aberto: %w", err)
	}
	if caminho.Windows && !caminho.ResolvidoNoWindows(final) {
		return fmt.Errorf("não consegui conferir onde está o arquivo aberto (%s)", final)
	}
	if caminho.CaminhoSensivel(hostRoot, final) {
		return ErrCaminhoRestrito
	}
	return nil
}

// arquivoComum: arquivo, e não dispositivo, pasta, FIFO ou link. No Windows o
// Go marca como irregular os reparse points que a varredura copia (OneDrive
// já baixado, WOF do compact.exe), e o File.Stat devolve o mesmo: lá vale o
// irregular sem outro tipo, o critério do irregularLegivel (revisão, 18/10:
// sem isto, todo arquivo do OneDrive falhava).
func arquivoComum(st fs.FileInfo) bool {
	if caminho.Windows {
		return st.Mode().Type()&^fs.ModeIrregular == 0
	}
	return st.Mode().IsRegular()
}

// mesmoArquivo: fora do Windows, o dispositivo e o inode do que abriu são os
// que a varredura viu. No Windows a varredura vem do FindFirstFile, sem o
// número do arquivo, e a conferência fica com o tipo e o caminho real.
func mesmoArquivo(visto, aberto fs.FileInfo) bool {
	if visto == nil || caminho.Windows {
		return true
	}
	return os.SameFile(visto, aberto)
}

// ErrArquivoMudou: o conteúdo enviado não é o que foi hasheado — o arquivo foi
// alterado durante o backup. O arquivo falha nesta sessão e volta na próxima.
var ErrArquivoMudou = errors.New("arquivo mudou durante o envio (o conteúdo enviado não confere com o sha256 calculado); fica para o próximo backup")

// ErrBucketSemVersionamento: o bucket devolveu objeto sem VersionId. Sem
// versão não há o que indexar nem restaurar; a sessão inteira falha com este
// erro em vez de terminar "complete" sem nada indexado.
var ErrBucketSemVersionamento = errors.New("bucket sem versionamento: ative o versionamento do bucket (o objeto subiu sem VersionId e o backup não seria restaurável)")

// ErrVersionamentoSuspenso: o envio voltou com VersionId "null" — o bucket
// está com o versionamento suspenso, e cada envio sobrescreve a versão "null"
// anterior. É um caso de ErrBucketSemVersionamento (errors.Is vale para os
// dois), com a causa certa na mensagem.
var ErrVersionamentoSuspenso error = erroVersionamentoSuspenso{}

type erroVersionamentoSuspenso struct{}

func (erroVersionamentoSuspenso) Error() string {
	return "versionamento do bucket suspenso: reative o versionamento do bucket (o objeto subiu como versão \"null\", que cada backup sobrescreve, e as versões anteriores não seriam restauráveis)"
}

func (erroVersionamentoSuspenso) Is(alvo error) bool { return alvo == ErrBucketSemVersionamento }

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
	// ModifiedAt fica para o chamador, que tem a data do arquivo.
	return &api.FileEntry{
		Key:       key,
		VersionID: versionID,
		Size:      size,
		SHA256:    hash,
	}, true
}

// Limites da data que vai ao painel: o JSON de time.Time (RFC 3339) só aceita
// os anos 0 a 9999 — fora disso o json.Marshal falha e a sessão inteira não
// fecha —, e o Postgres não tem o ano 0.
var (
	menorDataDoPainel = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maiorDataDoPainel = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
)

// dataParaOPainel é a data do arquivo para o índice, presa aos anos que o
// formato do painel aceita (0001–9999 em UTC: contrato, item 19). O arquivo
// com data fora disso segue com backup — o conteúdo sobe e a entrada vai ao
// índice, com a data da ponta da faixa: a data só serve para a tela e para o
// painel decidir se a entrada "continua igual", e presa ela é sempre a mesma.
// Recusar o arquivo deixaria sem backup, para sempre, o que tem o relógio
// errado na origem.
func dataParaOPainel(t time.Time) time.Time {
	switch {
	case t.Before(menorDataDoPainel):
		return menorDataDoPainel
	case t.After(maiorDataDoPainel):
		return maiorDataDoPainel
	}
	// O instante cabe, mas o texto leva o fuso em que a data veio (o do
	// servidor): a menos de um dia das pontas o ano escrito pode sair 10000 —
	// que o json.Marshal recusa — ou 0000. Aí vai em UTC, em que cabe.
	if a := t.Year(); a < 1 || a > 9999 {
		return t.UTC()
	}
	return t
}

// MaxBytesDaChave é o teto da chave de um objeto, em bytes UTF-8: o do S3 (e
// dos compatíveis), e o que o painel aceita numa entrada do índice — acima
// disso ele recusa o lote inteiro com `invalid_batch` (contrato, item 19).
const MaxBytesDaChave = 1024

// ErrChaveLonga: o caminho do arquivo dá uma chave que não cabe no
// armazenamento. É falha daquele arquivo (a sessão fecha parcial, com ele de
// exemplo), não do backup.
var ErrChaveLonga = errors.New("o caminho é longo demais para o armazenamento: a chave do arquivo passa de 1.024 bytes (encurte o caminho ou tire o arquivo do plano)")

// erroDaChave devolve ErrChaveLonga, com o tamanho, se a chave não cabe.
func erroDaChave(chave string) error {
	if len(chave) > MaxBytesDaChave {
		return fmt.Errorf("%w; esta tem %d", ErrChaveLonga, len(chave))
	}
	return nil
}
