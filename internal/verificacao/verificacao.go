// Package verificacao executa o teste de restauração por amostra: restaura,
// para uma pasta privada do agente, as versões que o painel escolheu, confere
// e apaga.
//
// O painel escolhe a amostra e o agente executa, porque as credenciais do
// bucket são do cliente e ficam só aqui. Quem grava é o executor da
// restauração de verdade (restore.Run): o que se prova é que o caminho que o
// cliente usaria num dia ruim funciona — baixar a versão, gravar, conferir
// tamanho e SHA-256.
//
// O plano vem de fora, e este código desconfia dele:
//
//   - o nome do arquivo no disco é o índice do item (item-0007), nunca a chave
//     do objeto: uma chave como `../../etc/passwd` não escolhe onde se grava;
//   - item fora do prefixo do armazenamento, sem versão ou com sha256 que não
//     é hex de 64 não é baixado. O prefixo vem do próprio painel, junto com o
//     plano: a conferência pega plano malformado, e NÃO é defesa contra um
//     painel malicioso — o agente lê do bucket qualquer chave que o painel
//     pedir. O que protege nesse caso é que os dados restaurados não saem da
//     máquina (o relato leva só status, tamanho e hash) e que o nome no disco
//     é neutro;
//   - objeto em classe fria (Glacier, Deep Archive) é pulado, sem pedir a
//     reidratação, que o provedor cobra;
//   - o que cabe no disco é decidido aqui: o teto de bytes da rodada e 5% do
//     espaço livre por item.
//
// Cada arquivo é apagado logo depois de conferido (o pico de disco é o maior
// arquivo). Os dados restaurados não saem da máquina e não vão para o log.
package verificacao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/caminho"
	"github.com/arkame-app/agent/internal/restore"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Status de um item.
const (
	StatusOK     = "ok"
	StatusFalhou = "failed"
	StatusPulado = "skipped"
)

// Códigos de erro do contrato (item 37).
const (
	ErroNaoEncontrado  = "not_found"
	ErroHashDiverge    = "hash_mismatch"
	ErroTamanhoDiverge = "size_mismatch"
	ErroAcessoNegado   = "access_denied"
	ErroDeDownload     = "download_error"

	PuladoFrio      = "cold"
	PuladoSemEspaco = "no_space"
	PuladoAcimaTeto = "over_budget"
	PuladoOcupado   = "busy"
	PuladoPrazo     = "timeout"
	PuladoErroLocal = "local_error"
	PuladoInvalido  = "invalid_item"
	// PuladoTransitorio: o bucket não deu resposta definitiva (5xx, prazo,
	// rede, DNS). Não se pôde testar; não é defeito do backup.
	PuladoTransitorio = "transient"
	maxDetalhe        = 200
	fracaoDoLivre     = 20 // um item ocupa no máximo 1/20 (5%) do espaço livre
	nomeDoItemPadrao  = "item-%04d"
)

// Tetos próprios do agente, valham o que valerem os do painel.
const (
	MaxItens       = 1000
	MaxBytesTeto   = 16 << 30
	MaxBytesPadrao = 1 << 30
	PrazoMinimo    = 60 * time.Second
	PrazoMaximo    = 6 * time.Hour
	PrazoPadrao    = 1 * time.Hour
)

// EspacoLivre devolve os bytes livres no sistema de arquivos da pasta.
// Variável para o teste simular o disco cheio.
var EspacoLivre = espacoLivre

// Opcoes reúne o que uma rodada precisa saber.
type Opcoes struct {
	S3         *s3.Client
	Bucket     string
	PrefixRoot string
	// Pasta é a pasta da rodada: já criada, privada, só desta rodada.
	Pasta string
	// HostRoot é a raiz do servidor como o agente a vê (/host no Docker).
	HostRoot string
	// MaxBytes é o teto somado dos itens baixados; Prazo, o da rodada inteira.
	MaxBytes int64
	Prazo    time.Duration
	// RunID só entra no log.
	RunID string
	// Ceder, entre um item e outro, diz se a rodada deve parar para dar a vez
	// (há backup esperando o bucket). nil: nunca cede.
	Ceder func() bool
}

// NomeDoItem é o nome, na pasta da rodada, do arquivo do i-ésimo item.
func NomeDoItem(i int) string { return fmt.Sprintf(nomeDoItemPadrao, i) }

// Limites aplica os tetos do agente ao que o painel pediu.
func Limites(maxBytes, prazoSegundos int64) (int64, time.Duration) {
	if maxBytes <= 0 {
		maxBytes = MaxBytesPadrao
	}
	if maxBytes > MaxBytesTeto {
		maxBytes = MaxBytesTeto
	}
	// Em segundos antes de virar Duration: um número absurdo estouraria a conta.
	var prazo time.Duration
	switch {
	case prazoSegundos <= 0:
		prazo = PrazoPadrao
	case prazoSegundos < int64(PrazoMinimo/time.Second):
		prazo = PrazoMinimo
	case prazoSegundos > int64(PrazoMaximo/time.Second):
		prazo = PrazoMaximo
	default:
		prazo = time.Duration(prazoSegundos) * time.Second
	}
	return maxBytes, prazo
}

// Rodar executa a rodada e devolve um resultado por item, na ordem do plano.
//
// interrompida: o ctx foi cancelado (o serviço está parando) antes do fim. Os
// resultados então não valem como relato: quem chama não os manda, e o painel
// reentrega a rodada.
func Rodar(ctx context.Context, o Opcoes, itens []api.VerifyItem) (resultados []api.VerifyResultItem, interrompida bool) {
	prazo := o.Prazo
	if prazo <= 0 {
		prazo = PrazoPadrao
	}
	rctx, cancelar := context.WithTimeout(ctx, prazo)
	defer cancelar()

	raiz, destino := destinoDoRun(o.HostRoot, o.Pasta)
	exec := restore.Options{
		S3:       o.S3,
		HostRoot: raiz,
		// Sem pastas protegidas: o destino não vem do painel, é a pasta da
		// rodada, que este agente escolheu — e ela fica dentro da pasta do
		// estado, que para a restauração comum é protegida.
		Protegidas:   nil,
		SemReidratar: true,
	}

	var somados int64
	resultados = make([]api.VerifyResultItem, 0, len(itens))
	motivoDoResto := ""
	for i, it := range itens {
		r := api.VerifyResultItem{Key: it.Key, VersionID: it.VersionID}
		if ctx.Err() != nil {
			return resultados, true
		}
		if motivoDoResto == "" {
			switch {
			case rctx.Err() != nil:
				motivoDoResto = PuladoPrazo
			case i > 0 && o.Ceder != nil && o.Ceder():
				motivoDoResto = PuladoOcupado
			}
		}
		if motivoDoResto != "" {
			r.Status, r.Error = StatusPulado, motivoDoResto
			resultados = append(resultados, r)
			continue
		}

		inicio := time.Now()
		conferirItem(rctx, o, exec, destino, i, it, &somados, &r)
		r.Ms = time.Since(inicio).Milliseconds()
		if ctx.Err() != nil {
			// O serviço parou no meio deste item: o resultado dele não diz
			// nada sobre o backup.
			return resultados, true
		}
		if rctx.Err() != nil && r.Status != StatusOK {
			// O prazo da rodada venceu durante o item: não é falha do arquivo.
			r.Status, r.Error, r.Detail, r.SHA256 = StatusPulado, PuladoPrazo, "", ""
		}
		slog.Debug("item da verificação", "indice", i, "key", it.Key, "status", r.Status, "erro", r.Error)
		resultados = append(resultados, r)
	}
	return resultados, false
}

// conferirItem faz um item: valida, olha o objeto, confere espaço, restaura,
// relê, confere e apaga. O arquivo do item nunca sobrevive à função.
func conferirItem(ctx context.Context, o Opcoes, exec restore.Options, destino string, i int, it api.VerifyItem, somados *int64, r *api.VerifyResultItem) {
	pular := func(codigo, detalhe string) { r.Status, r.Error, r.Detail = StatusPulado, codigo, cortar(detalhe) }
	falhar := func(codigo, detalhe string) { r.Status, r.Error, r.Detail = StatusFalhou, codigo, cortar(detalhe) }

	if motivo := itemInvalido(o, it); motivo != "" {
		pular(PuladoInvalido, motivo)
		return
	}
	if o.MaxBytes > 0 && *somados+it.Size > o.MaxBytes {
		pular(PuladoAcimaTeto, "")
		return
	}

	head, err := o.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &o.Bucket, Key: &it.Key, VersionId: &it.VersionID})
	if err != nil {
		codigo, falha := classificarErroDeS3(err)
		if falha {
			falhar(codigo, err.Error())
		} else {
			pular(codigo, err.Error())
		}
		return
	}
	if frio(head) {
		pular(PuladoFrio, string(head.StorageClass))
		return
	}
	if head.ContentLength != nil && *head.ContentLength != it.Size {
		falhar(ErroTamanhoDiverge, fmt.Sprintf("tamanho no bucket=%d, no catálogo=%d", *head.ContentLength, it.Size))
		return
	}

	livre, err := EspacoLivre(o.Pasta)
	if err != nil {
		pular(PuladoErroLocal, "espaço livre: "+err.Error())
		return
	}
	if it.Size > 0 && uint64(it.Size) > livre/fracaoDoLivre {
		pular(PuladoSemEspaco, "")
		return
	}

	nome := NomeDoItem(i)
	arquivo := filepath.Join(o.Pasta, nome)
	// Um arquivo com este nome (resto de uma rodada interrompida) faria o
	// executor dar o item por já restaurado, sem baixar nada.
	if err := os.Remove(arquivo); err != nil && !errors.Is(err, fs.ErrNotExist) {
		pular(PuladoErroLocal, "limpando o item: "+err.Error())
		return
	}
	defer os.Remove(arquivo)

	*somados += it.Size
	err = restore.Run(ctx, exec, api.RestoreItem{
		ItemID:           nome,
		Bucket:           o.Bucket,
		SourceKey:        it.Key,
		SourceVersionID:  it.VersionID,
		SourceSize:       it.Size,
		SourceSha256:     it.SHA256,
		DestPath:         destino,
		DestFilename:     nome,
		ConflictStrategy: "overwrite",
	})
	if err != nil {
		var diverge *restore.ErroDeConteudo
		switch {
		case errors.As(err, &diverge):
			if strings.HasPrefix(diverge.Error(), "size mismatch") {
				falhar(ErroTamanhoDiverge, err.Error())
			} else {
				falhar(ErroHashDiverge, err.Error())
			}
		case errors.Is(err, restore.ErrFrio):
			pular(PuladoFrio, "")
		default:
			codigo, falha := classificarErroDoRun(err)
			if falha {
				falhar(codigo, err.Error())
			} else {
				pular(codigo, err.Error())
			}
		}
		return
	}

	// O que vale é o que ficou no disco: relê o arquivo gravado.
	aposRestaurar(arquivo)
	tamanho, soma, err := reler(arquivo)
	if err != nil {
		pular(PuladoErroLocal, "relendo o item: "+err.Error())
		return
	}
	// O executor já conferiu tamanho e sha256 do que veio do bucket. Se o que
	// está no disco é outra coisa, o defeito é do disco local (ou alguém mexeu
	// na pasta): não se acusa o backup por isso.
	if tamanho != it.Size || !strings.EqualFold(soma, it.SHA256) {
		slog.Warn("teste de restauração: o arquivo relido do disco não é o que foi baixado; item pulado (disco local)",
			"run_id", o.RunID, "indice", i)
		pular(PuladoErroLocal, "o arquivo relido do disco difere do que foi baixado e conferido")
		return
	}
	r.Status, r.Bytes, r.SHA256 = StatusOK, tamanho, soma
}

// aposRestaurar é o ponto, entre a gravação e a releitura, em que o teste
// estraga o arquivo como faria um disco com defeito.
var aposRestaurar = func(string) {}

// itemInvalido devolve por que o item não é baixado ("" quando serve).
func itemInvalido(o Opcoes, it api.VerifyItem) string {
	switch {
	case it.Key == "" || !utf8.ValidString(it.Key) || strings.ContainsRune(it.Key, 0):
		return "key vazia ou malformada"
	case it.VersionID == "":
		return "item sem version_id"
	case o.PrefixRoot != "" && !strings.HasPrefix(it.Key, o.PrefixRoot):
		return "key fora do prefixo do armazenamento"
	case it.Size < 0:
		return "size negativo"
	case !hex64(it.SHA256):
		return "sha256 não é hex de 64"
	}
	return ""
}

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// frio diz se o objeto está numa classe que só se lê depois de reidratar.
// GLACIER_IR (leitura imediata) não é.
func frio(h *s3.HeadObjectOutput) bool {
	switch h.StorageClass {
	case s3types.StorageClassGlacier, s3types.StorageClassDeepArchive:
		return true
	}
	return h.ArchiveStatus != ""
}

// classificarErroDeS3 leva o erro de uma chamada à S3 ao código do contrato.
// falha=false: o item é pulado (não se pôde testar), não falho.
//
// Só reprova o backup a resposta definitiva do bucket: a versão não existe
// (404), a chave do servidor não lê (403), ou outro 4xx que repetir não muda
// (download_error). 5xx, limite de requisições, prazo, rede e DNS são do
// momento: o item vai como pulado (transient) e a rodada seguinte o testa —
// um provedor fora do ar por dez minutos não pode virar "teste de restauração
// falhou" na caixa do cliente.
func classificarErroDeS3(err error) (codigo string, falha bool) {
	switch {
	case restore.ObjetoSumiu(err):
		return ErroNaoEncontrado, true
	case acessoNegado(err):
		return ErroAcessoNegado, true
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		st := re.HTTPStatusCode()
		if st >= 400 && st < 500 && st != http.StatusRequestTimeout && st != http.StatusTooManyRequests {
			return ErroDeDownload, true
		}
	}
	return PuladoTransitorio, false
}

// classificarErroDoRun separa o que é do bucket ou da rede (classificarErroDeS3)
// do que é do disco local (o item é pulado: não é defeito do backup).
func classificarErroDoRun(err error) (codigo string, falha bool) {
	if restore.ObjetoSumiu(err) || acessoNegado(err) {
		return classificarErroDeS3(err)
	}
	var deApi smithy.APIError
	var deCaminho *fs.PathError
	msg := err.Error()
	switch {
	case errors.As(err, &deApi), strings.HasPrefix(msg, "s3 "):
		return classificarErroDeS3(err)
	case strings.HasPrefix(msg, "write tmp") && !errors.As(err, &deCaminho):
		// A cópia parou na leitura do corpo (rede), não na gravação.
		return PuladoTransitorio, false
	}
	return PuladoErroLocal, false
}

func acessoNegado(err error) bool {
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "AccessDenied", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return true
		}
	}
	var re interface{ HTTPStatusCode() int }
	return errors.As(err, &re) && re.HTTPStatusCode() == 403
}

// reler lê o arquivo gravado e devolve o tamanho e o sha256 dele. Só arquivo
// comum: um link no lugar não é seguido.
func reler(arquivo string) (int64, string, error) {
	st, err := os.Lstat(arquivo)
	if err != nil {
		return 0, "", err
	}
	if !st.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s não é arquivo comum", filepath.Base(arquivo))
	}
	f, err := os.Open(arquivo)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// destinoDoRun traduz a pasta da rodada (caminho real neste processo) para o
// par raiz + destino que o executor da restauração espera. Com a pasta sob o
// HostRoot (o caso do Docker, /host/var/lib/arkame/…), a raiz é o HostRoot e o
// destino é o caminho dentro dele; fora dele (a pasta do estado caiu na da
// configuração, que é do container), a raiz é a do sistema. No Windows não há
// HostRoot: o destino é o caminho inteiro, com a unidade.
func destinoDoRun(hostRoot, pasta string) (raiz, destino string) {
	if caminho.Windows {
		return hostRoot, pasta
	}
	hr := filepath.Clean(hostRoot)
	if hostRoot == "" {
		hr = "/"
	}
	if rel, err := filepath.Rel(hr, pasta); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hr, "/" + filepath.ToSlash(rel)
	}
	return "/", pasta
}

// cortar limita o detalhe a maxDetalhe bytes, sem partir um caractere, e troca
// por espaço o que não é texto de uma linha (NUL, controles, UTF-8 inválido):
// o detalhe é a mensagem do SDK, que cita a chave do objeto — e uma chave pode
// trazer qualquer byte. O painel grava isso em jsonb, que recusa \u0000.
func cortar(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, " "))
	if len(s) <= maxDetalhe {
		return s
	}
	corte := maxDetalhe
	for corte > 0 && !utf8.RuneStart(s[corte]) {
		corte--
	}
	return s[:corte]
}

// ClassificarErroDeS3 é a régua do que é resposta definitiva do bucket e do
// que é erro do momento, para quem mais precisa dela (o inventário do bucket):
// uma régua só, para um provedor fora do ar não virar "falta" em lugar nenhum.
func ClassificarErroDeS3(err error) (codigo string, definitivo bool) {
	return classificarErroDeS3(err)
}
