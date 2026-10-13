package verificacao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// objetoFalso é uma versão no bucket de mentira.
type objetoFalso struct {
	conteudo []byte
	classe   string // x-amz-storage-class
	arquivo  string // x-amz-archive-status
	// tamanhoNoHead, quando > 0, é o Content-Length que o HEAD diz (diferente
	// do conteúdo).
	tamanhoNoHead int64
	// semGet: o HEAD acha, o GET responde NoSuchVersion (sumiu entre os dois).
	semGet bool
	// frioNoGet: o HEAD não diz a classe (OCI) e o GET responde InvalidObjectState.
	frioNoGet  bool
	headStatus int // status forçado do HEAD (403, 500…)
}

// bucketFalso é um S3 de mentira: HEAD e GET por chave + versão, e conta o
// que foi pedido — inclusive os RestoreObject, que nunca podem sair.
type bucketFalso struct {
	mu       sync.Mutex
	objetos  map[string]*objetoFalso
	heads    int
	gets     int
	restores int
	// aoGet roda no começo de cada GET, com a chave pedida.
	aoGet func(chave string)
}

func (b *bucketFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	chave := strings.TrimPrefix(r.URL.Path, "/b/")
	versao := r.URL.Query().Get("versionId")
	b.mu.Lock()
	o := b.objetos[chave+"\x00"+versao]
	aoGet := b.aoGet
	switch {
	case r.Method == http.MethodPost && r.URL.Query().Has("restore"):
		b.restores++
	case r.Method == http.MethodHead:
		b.heads++
	case r.Method == http.MethodGet:
		b.gets++
	}
	b.mu.Unlock()

	erro := func(status int, codigo string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, codigo, codigo)
		}
	}
	switch r.Method {
	case http.MethodPost:
		w.WriteHeader(http.StatusAccepted)
	case http.MethodHead:
		if o == nil {
			erro(http.StatusNotFound, "NotFound")
			return
		}
		if o.headStatus != 0 {
			erro(o.headStatus, "Erro")
			return
		}
		n := int64(len(o.conteudo))
		if o.tamanhoNoHead > 0 {
			n = o.tamanhoNoHead
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		if o.classe != "" {
			w.Header().Set("x-amz-storage-class", o.classe)
		}
		if o.arquivo != "" {
			w.Header().Set("x-amz-archive-status", o.arquivo)
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if aoGet != nil {
			aoGet(chave)
		}
		switch {
		case o == nil || o.semGet:
			erro(http.StatusNotFound, "NoSuchVersion")
		case o.frioNoGet:
			erro(http.StatusForbidden, "InvalidObjectState")
		default:
			_, _ = w.Write(o.conteudo)
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (b *bucketFalso) contagem() (heads, gets, restores int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.heads, b.gets, b.restores
}

func novoBucket(t *testing.T) (*bucketFalso, *s3.Client) {
	t.Helper()
	b := &bucketFalso{objetos: map[string]*objetoFalso{}}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return b, s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
}

const prefixo = "arkame/data/a1/"

// guardar põe um objeto no bucket e devolve o item do plano que o descreve
// como o catálogo o registrou.
func (b *bucketFalso) guardar(chave string, conteudo []byte) api.VerifyItem {
	b.mu.Lock()
	b.objetos[chave+"\x00v1"] = &objetoFalso{conteudo: conteudo}
	b.mu.Unlock()
	h := sha256.Sum256(conteudo)
	return api.VerifyItem{Key: chave, VersionID: "v1", Size: int64(len(conteudo)), SHA256: hex.EncodeToString(h[:])}
}

func (b *bucketFalso) objeto(chave string) *objetoFalso { return b.objetos[chave+"\x00v1"] }

// mesa é a pasta da rodada dentro de uma pasta vigiada: o teste confere que
// nada foi gravado fora da rodada.
type mesa struct {
	vigiada string
	rodada  string
}

func novaMesa(t *testing.T) mesa {
	t.Helper()
	vigiada := t.TempDir()
	rodada := filepath.Join(vigiada, "estado", "verificacao", "a1_st1", "run")
	if err := os.MkdirAll(rodada, 0o700); err != nil {
		t.Fatal(err)
	}
	return mesa{vigiada: vigiada, rodada: rodada}
}

// arquivos lista, relativo à pasta vigiada, todo arquivo que existe nela.
func (m mesa) arquivos(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(m.vigiada, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(m.vigiada, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (m mesa) opcoes(s3c *s3.Client) Opcoes {
	return Opcoes{S3: s3c, Bucket: "b", PrefixRoot: prefixo, Pasta: m.rodada, HostRoot: "/", MaxBytes: 1 << 30, Prazo: time.Minute}
}

func disco(t *testing.T, livre uint64) {
	t.Helper()
	antes := EspacoLivre
	EspacoLivre = func(string) (uint64, error) { return livre, nil }
	t.Cleanup(func() { EspacoLivre = antes })
}

func conferir(t *testing.T, r api.VerifyResultItem, status, erro string) {
	t.Helper()
	if r.Status != status || r.Error != erro {
		t.Fatalf("item %q: veio %s/%q (%s), queria %s/%q", r.Key, r.Status, r.Error, r.Detail, status, erro)
	}
}

// A amostra sã: cada item é baixado, gravado com o nome neutro, relido,
// conferido e apagado; o sha256 relido do disco vai no resultado.
func TestAmostraSaEConferidaEApagada(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	itens := []api.VerifyItem{
		b.guardar(prefixo+"etc/app.conf", []byte("conteúdo um")),
		b.guardar(prefixo+"var/dados.bin", []byte("conteúdo dois, maior")),
		b.guardar(prefixo+"vazio", nil),
	}
	var vistos [][]string
	b.aoGet = func(string) { vistos = append(vistos, m.arquivos(t)) }

	res, interrompida := Rodar(context.Background(), m.opcoes(s3c), itens)

	if interrompida || len(res) != 3 {
		t.Fatalf("interrompida=%v resultados=%d", interrompida, len(res))
	}
	for i, r := range res {
		conferir(t, r, StatusOK, "")
		if r.SHA256 != itens[i].SHA256 || r.Bytes != itens[i].Size {
			t.Fatalf("item %d: sha256/bytes do resultado não são os do arquivo relido: %+v", i, r)
		}
	}
	// Durante cada download só existe o temporário daquele item, na rodada.
	for i, arquivos := range vistos {
		if len(arquivos) != 1 || !strings.HasPrefix(arquivos[0], "estado/verificacao/a1_st1/run/.arkame-restore-") ||
			!strings.HasSuffix(arquivos[0], "."+NomeDoItem(i)) {
			t.Fatalf("durante o item %d havia no disco %v; esperava só o temporário de %s (o item anterior já apagado)", i, arquivos, NomeDoItem(i))
		}
	}
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("a rodada deixou arquivos no disco: %v", sobrou)
	}
	if _, _, restores := b.contagem(); restores != 0 {
		t.Fatalf("a verificação pediu %d RestoreObject", restores)
	}
}

// Objeto apagado do bucket: o item falha com not_found — pelo HEAD ou, se
// sumiu entre o HEAD e o download, pelo GET — e os outros seguem.
func TestObjetoApagadoFalhaComNotFound(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	a := b.guardar(prefixo+"a", []byte("aaa"))
	some := b.guardar(prefixo+"some", []byte("bbb"))
	noGet := b.guardar(prefixo+"no-get", []byte("ccc"))
	d := b.guardar(prefixo+"d", []byte("ddd"))
	delete(b.objetos, some.Key+"\x00v1")
	b.objeto(noGet.Key).semGet = true

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{a, some, noGet, d})

	conferir(t, res[0], StatusOK, "")
	conferir(t, res[1], StatusFalhou, ErroNaoEncontrado)
	conferir(t, res[2], StatusFalhou, ErroNaoEncontrado)
	conferir(t, res[3], StatusOK, "")
	if res[1].SHA256 != "" || res[2].SHA256 != "" {
		t.Fatal("item que falhou não leva sha256")
	}
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("sobrou no disco: %v", sobrou)
	}
}

// O conteúdo no bucket não é o que o catálogo registra (mesmo tamanho, outro
// sha256): hash_mismatch, e nada fica no disco.
func TestHashDivergenteFalha(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	it := b.guardar(prefixo+"relatorio.odt", []byte("o que o catálogo viu"))
	b.objeto(it.Key).conteudo = []byte("o que o bucket tem!!")
	it.Size = int64(len(b.objeto(it.Key).conteudo))

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{it})

	conferir(t, res[0], StatusFalhou, ErroHashDiverge)
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("o arquivo divergente ficou no disco: %v", sobrou)
	}
}

// O tamanho no bucket não é o do catálogo: size_mismatch já no HEAD, sem baixar.
func TestTamanhoDivergenteFalhaSemBaixar(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	it := b.guardar(prefixo+"banco.dump", []byte("123456"))
	b.objeto(it.Key).tamanhoNoHead = 9999

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{it})

	conferir(t, res[0], StatusFalhou, ErroTamanhoDiverge)
	if _, gets, _ := b.contagem(); gets != 0 {
		t.Fatalf("baixou (%d GET) um objeto cujo tamanho já não batia", gets)
	}
}

// Item maior que 5% do espaço livre: pulado como no_space, sem baixar. O que
// cabe segue.
func TestSemEspacoPulaOItem(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 2000) // 5% = 100 bytes
	grande := b.guardar(prefixo+"grande", make([]byte, 101))
	noLimite := b.guardar(prefixo+"no-limite", make([]byte, 100))

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{grande, noLimite})

	conferir(t, res[0], StatusPulado, PuladoSemEspaco)
	conferir(t, res[1], StatusOK, "")
	if _, gets, _ := b.contagem(); gets != 1 {
		t.Fatalf("esperava 1 GET (só o que cabe), foram %d", gets)
	}
}

// Classe fria: pulado como cold, sem download e sem RestoreObject — nem
// quando a classe só aparece no GET (InvalidObjectState, o caso da OCI).
// GLACIER_IR lê na hora e é conferido.
func TestClasseFriaEPuladaSemReidratar(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	glacier := b.guardar(prefixo+"g", []byte("g"))
	deep := b.guardar(prefixo+"d", []byte("d"))
	it := b.guardar(prefixo+"it", []byte("i"))
	ir := b.guardar(prefixo+"ir", []byte("r"))
	oci := b.guardar(prefixo+"oci", []byte("o"))
	b.objeto(glacier.Key).classe = "GLACIER"
	b.objeto(deep.Key).classe = "DEEP_ARCHIVE"
	b.objeto(it.Key).classe, b.objeto(it.Key).arquivo = "INTELLIGENT_TIERING", "DEEP_ARCHIVE_ACCESS"
	b.objeto(ir.Key).classe = "GLACIER_IR"
	b.objeto(oci.Key).frioNoGet = true

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{glacier, deep, it, ir, oci})

	conferir(t, res[0], StatusPulado, PuladoFrio)
	conferir(t, res[1], StatusPulado, PuladoFrio)
	conferir(t, res[2], StatusPulado, PuladoFrio)
	conferir(t, res[3], StatusOK, "")
	conferir(t, res[4], StatusPulado, PuladoFrio)
	_, gets, restores := b.contagem()
	if gets != 2 {
		t.Fatalf("esperava 2 GET (GLACIER_IR e o que só se revela no GET), foram %d", gets)
	}
	if restores != 0 {
		t.Fatalf("a verificação pediu %d reidratação(ões) paga(s)", restores)
	}
}

// A chave do objeto não escolhe onde se grava: com `..`, barra invertida ou o
// que for, o arquivo no disco é item-NNNN dentro da rodada. Chave fora do
// prefixo, absoluta ou com NUL nem é baixada.
func TestChaveMaliciosaNaoEscolheODestino(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	alvo := filepath.Join(m.vigiada, "passwd")
	itens := []api.VerifyItem{
		b.guardar(prefixo+"../../../../passwd", []byte("root::0:0")),
		b.guardar(prefixo+`..\..\..\..\passwd`, []byte("barra invertida")),
		b.guardar(prefixo+"a/./b/../item-0000", []byte("nome de outro item")),
		b.guardar(alvo, []byte("absoluta")),
		b.guardar("/etc/passwd", []byte("absoluta 2")),
		b.guardar("outro-prefixo/x", []byte("fora do prefixo")),
		{Key: prefixo + "com\x00nulo", VersionID: "v1", Size: 1, SHA256: strings.Repeat("a", 64)},
		{Key: prefixo + "sem-versao", VersionID: "", Size: 1, SHA256: strings.Repeat("a", 64)},
		{Key: prefixo + "sha-ruim", VersionID: "v1", Size: 1, SHA256: "../../x"},
	}
	var vistos []string
	b.aoGet = func(string) { vistos = append(vistos, m.arquivos(t)...) }

	res, _ := Rodar(context.Background(), m.opcoes(s3c), itens)

	for i := 0; i < 3; i++ {
		conferir(t, res[i], StatusOK, "")
	}
	for i := 3; i < len(itens); i++ {
		conferir(t, res[i], StatusPulado, PuladoInvalido)
	}
	if _, gets, _ := b.contagem(); gets != 3 {
		t.Fatalf("esperava 3 GET (só as chaves sob o prefixo), foram %d", gets)
	}
	if len(vistos) != 3 {
		t.Fatalf("esperava um arquivo por download, vi %v", vistos)
	}
	for i, v := range vistos {
		if !strings.HasPrefix(v, "estado/verificacao/a1_st1/run/.arkame-restore-") || !strings.HasSuffix(v, "."+NomeDoItem(i)) {
			t.Fatalf("o item %d foi gravado em %q: o nome no disco tem de ser o índice do item, dentro da rodada", i, v)
		}
	}
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("ficou arquivo no disco (algum fora da rodada?): %v", sobrou)
	}
}

// Um arquivo velho com o nome do item e o conteúdo certo (resto de uma rodada
// interrompida) não vale como restaurado: o objeto sumiu do bucket, e o item
// tem de falhar. O executor, sozinho, daria o item por já feito.
func TestArquivoVelhoNaoViraOk(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	conteudo := []byte("o arquivo como o catálogo o registra")
	it := b.guardar(prefixo+"docs/contrato.pdf", conteudo)
	b.objeto(it.Key).semGet = true
	if err := os.WriteFile(filepath.Join(m.rodada, NomeDoItem(0)), conteudo, 0o600); err != nil {
		t.Fatal(err)
	}

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{it})

	conferir(t, res[0], StatusFalhou, ErroNaoEncontrado)
	if _, gets, _ := b.contagem(); gets != 1 {
		t.Fatalf("o item não foi baixado de novo (%d GET): o arquivo velho serviu de prova", gets)
	}
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("o arquivo velho ficou no disco: %v", sobrou)
	}
}

// O teto de bytes da rodada: o item que o estouraria é pulado (over_budget)
// sem baixar; um menor, depois, ainda entra.
func TestTetoDeBytesDaRodada(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	a := b.guardar(prefixo+"a", make([]byte, 60))
	grande := b.guardar(prefixo+"grande", make([]byte, 50))
	c := b.guardar(prefixo+"c", make([]byte, 40))
	o := m.opcoes(s3c)
	o.MaxBytes = 100

	res, _ := Rodar(context.Background(), o, []api.VerifyItem{a, grande, c})

	conferir(t, res[0], StatusOK, "")
	conferir(t, res[1], StatusPulado, PuladoAcimaTeto)
	conferir(t, res[2], StatusOK, "")
	if heads, gets, _ := b.contagem(); gets != 2 || heads != 2 {
		t.Fatalf("o item acima do teto não pode custar requisição: %d HEAD, %d GET", heads, gets)
	}
}

// Prazo da rodada vencido: o item em curso e os restantes vão como timeout —
// não como falha do arquivo.
func TestPrazoDaRodada(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	a := b.guardar(prefixo+"a", []byte("a"))
	lento := b.guardar(prefixo+"lento", []byte("b"))
	c := b.guardar(prefixo+"c", []byte("c"))
	o := m.opcoes(s3c)
	o.Prazo = 400 * time.Millisecond
	b.aoGet = func(chave string) {
		if chave == lento.Key {
			time.Sleep(900 * time.Millisecond)
		}
	}

	res, interrompida := Rodar(context.Background(), o, []api.VerifyItem{a, lento, c})

	if interrompida {
		t.Fatal("prazo vencido não é interrupção: a rodada relata o que fez")
	}
	conferir(t, res[0], StatusOK, "")
	conferir(t, res[1], StatusPulado, PuladoPrazo)
	conferir(t, res[2], StatusPulado, PuladoPrazo)
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("sobrou no disco: %v", sobrou)
	}
}

// Backup esperando o bucket: a rodada encerra no item em que está, e os
// restantes vão como busy.
func TestCederEncerraARodada(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	itens := []api.VerifyItem{b.guardar(prefixo+"a", []byte("a")), b.guardar(prefixo+"b", []byte("b")), b.guardar(prefixo+"c", []byte("c"))}
	o := m.opcoes(s3c)
	perguntas := 0
	o.Ceder = func() bool { perguntas++; return perguntas >= 2 } // cede antes do 3º

	res, interrompida := Rodar(context.Background(), o, itens)

	if interrompida {
		t.Fatal("ceder não é interrupção")
	}
	conferir(t, res[0], StatusOK, "")
	conferir(t, res[1], StatusOK, "")
	conferir(t, res[2], StatusPulado, PuladoOcupado)
	if _, gets, _ := b.contagem(); gets != 2 {
		t.Fatalf("esperava 2 GET antes de ceder, foram %d", gets)
	}
}

// Serviço parando no meio: a rodada volta como interrompida (quem chama não
// relata) e não deixa arquivo.
func TestServicoParandoInterrompeARodada(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	itens := []api.VerifyItem{b.guardar(prefixo+"a", []byte("a")), b.guardar(prefixo+"b", []byte("b"))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.aoGet = func(string) { cancel(); time.Sleep(50 * time.Millisecond) }

	res, interrompida := Rodar(ctx, m.opcoes(s3c), itens)

	if !interrompida {
		t.Fatalf("a rodada cortada pela parada do serviço tem de voltar como interrompida; veio %+v", res)
	}
	if _, gets, _ := b.contagem(); gets != 1 {
		t.Fatalf("seguiu baixando depois da parada: %d GET", gets)
	}
	if sobrou := m.arquivos(t); len(sobrou) != 0 {
		t.Fatalf("sobrou no disco: %v", sobrou)
	}
}

// Credencial recusada e erro do provedor são falha do item, cada um com o seu
// código; pasta local sem escrita é item pulado (não é defeito do backup).
func TestClassificacaoDosErros(t *testing.T) {
	b, s3c := novoBucket(t)
	m := novaMesa(t)
	disco(t, 1<<40)
	negado := b.guardar(prefixo+"negado", []byte("x"))
	quebrado := b.guardar(prefixo+"quebrado", []byte("y"))
	b.objeto(negado.Key).headStatus = http.StatusForbidden
	b.objeto(quebrado.Key).headStatus = http.StatusInternalServerError

	res, _ := Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{negado, quebrado})

	conferir(t, res[0], StatusFalhou, ErroAcessoNegado)
	conferir(t, res[1], StatusFalhou, ErroDeDownload)
	if len(res[1].Detail) > maxDetalhe {
		t.Fatalf("detalhe com %d bytes", len(res[1].Detail))
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // sem como tirar a escrita da pasta
	}
	ok := b.guardar(prefixo+"ok", []byte("z"))
	if err := os.Chmod(m.rodada, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(m.rodada, 0o700) })
	res, _ = Rodar(context.Background(), m.opcoes(s3c), []api.VerifyItem{ok})
	conferir(t, res[0], StatusPulado, PuladoErroLocal)
}

// Os tetos do agente valem contra qualquer número que o painel mande.
func TestLimites(t *testing.T) {
	casos := []struct {
		bytes, segundos int64
		querBytes       int64
		querPrazo       time.Duration
	}{
		{0, 0, MaxBytesPadrao, PrazoPadrao},
		{-5, -5, MaxBytesPadrao, PrazoPadrao},
		{1 << 20, 600, 1 << 20, 10 * time.Minute},
		{1 << 50, 5, MaxBytesTeto, PrazoMinimo},
		{MaxBytesTeto + 1, 1 << 40, MaxBytesTeto, PrazoMaximo},
	}
	for _, c := range casos {
		b, p := Limites(c.bytes, c.segundos)
		if b != c.querBytes || p != c.querPrazo {
			t.Errorf("Limites(%d, %d) = %d, %v; queria %d, %v", c.bytes, c.segundos, b, p, c.querBytes, c.querPrazo)
		}
	}
}

// A pasta da rodada vira o par raiz + destino do executor: dentro do HostRoot
// (Docker) o destino é relativo a ele; fora, a raiz é a do sistema.
func TestDestinoDoRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows o destino é o caminho inteiro")
	}
	casos := []struct{ hostRoot, pasta, raiz, destino string }{
		{"/host", "/host/var/lib/arkame/estado/verificacao/x/r", "/host", "/var/lib/arkame/estado/verificacao/x/r"},
		{"/host", "/etc/arkame/estado/verificacao/x/r", "/", "/etc/arkame/estado/verificacao/x/r"},
		{"/host", "/hostil/estado/r", "/", "/hostil/estado/r"},
		{"/", "/var/lib/arkame/estado/verificacao/x/r", "/", "/var/lib/arkame/estado/verificacao/x/r"},
		{"", "/var/lib/r", "/", "/var/lib/r"},
	}
	for _, c := range casos {
		raiz, destino := destinoDoRun(c.hostRoot, c.pasta)
		if raiz != c.raiz || destino != c.destino {
			t.Errorf("destinoDoRun(%q, %q) = %q, %q; queria %q, %q", c.hostRoot, c.pasta, raiz, destino, c.raiz, c.destino)
		}
	}
}
