package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/purge"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// mesaDoDaemon é um servidor de mentira inteiro: árvore no disco, bucket
// falso, painel falso e a configuração do agente com estado local. rodar
// executa o plano como o daemon executa.
type mesaDoDaemon struct {
	t      *testing.T
	raiz   string
	s3     *bancada.S3Falso
	s3c    *s3.Client
	painel *bancada.PainelFalso
	c      *api.Client
	cfg    *config.Config
	plano  api.Plan
}

func novaMesaDoDaemon(t *testing.T) *mesaDoDaemon {
	t.Helper()
	recuosCurtos(t)
	antes := recuoMaximoDoLote
	recuoMaximoDoLote = 5 * time.Millisecond
	t.Cleanup(func() { recuoMaximoDoLote = antes })

	m := &mesaDoDaemon{t: t, raiz: t.TempDir(), s3: bancada.NovoS3Falso(), painel: bancada.NovoPainelFalso()}
	srvS3 := httptest.NewServer(m.s3)
	t.Cleanup(srvS3.Close)
	srvP := httptest.NewServer(m.painel)
	t.Cleanup(srvP.Close)
	m.s3c = bancada.ClienteS3(srvS3.URL)
	m.c, _ = api.New(api.Options{BaseURL: srvP.URL, Bearer: "t"})
	m.cfg = &config.Config{AgentID: "ag1", HostRoot: "/", StorageID: "st1", StorageBucket: "b", StateDir: t.TempDir()}
	m.plano = api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{m.raiz}, StorageRef: api.StorageRef{ID: "st1", Bucket: "b"}}
	return m
}

// escrever grava um arquivo com a data de uma hora atrás: arquivo recém
// gravado é "data suspeita" e não serviria de atalho.
func (m *mesaDoDaemon) escrever(rel, conteudo string) {
	m.t.Helper()
	p := filepath.Join(m.raiz, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(conteudo), 0o644); err != nil {
		m.t.Fatal(err)
	}
	antiga := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, antiga, antiga); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mesaDoDaemon) semear(n int) {
	for i := 0; i < n; i++ {
		m.escrever(fmt.Sprintf("p%02d/arquivo-%05d.txt", i%9, i), fmt.Sprintf("conteúdo %d", i))
	}
}

func (m *mesaDoDaemon) rodar() error {
	m.t.Helper()
	return executePlan(context.Background(), m.c, m.s3c, m.cfg, m.plano)
}

func (m *mesaDoDaemon) rodarOK() *bancada.SessaoFalsa {
	m.t.Helper()
	if err := m.rodar(); err != nil {
		m.t.Fatalf("backup: %v", err)
	}
	return m.painel.Ultima()
}

func (m *mesaDoDaemon) chave(rel string) string {
	return "data/ag1/" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(m.raiz, rel)), "/")
}

// conferir compara o catálogo do painel (arquivos presentes na última sessão
// concluída) com o disco e com o bucket.
func (m *mesaDoDaemon) conferir() {
	m.t.Helper()
	cat := m.painel.Catalogo("ag1", "p1")
	n := 0
	_ = filepath.WalkDir(m.raiz, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		b, _ := os.ReadFile(p)
		soma := sha256.Sum256(b)
		k := "data/ag1/" + strings.TrimPrefix(filepath.ToSlash(p), "/")
		e, tem := cat[k]
		if !tem {
			m.t.Errorf("arquivo fora do catálogo do painel: %s", k)
			return nil
		}
		atual, ok := m.s3.Atual(k)
		if e.SHA256 != hex.EncodeToString(soma[:]) || !ok || atual.ID != e.VersionID || atual.SHA256 != e.SHA256 {
			m.t.Errorf("catálogo de %s não confere: %+v; bucket: %+v (existe=%v)", k, e, atual, ok)
		}
		return nil
	})
	if len(cat) != n {
		m.t.Errorf("catálogo com %d arquivos, disco com %d", len(cat), n)
	}
}

func semS3AlemDoVersionamento(t *testing.T, f *bancada.S3Falso) {
	t.Helper()
	for op, n := range f.Requisicoes() {
		if op != "GetBucketVersioning" && n > 0 {
			t.Fatalf("o bucket não devia ser consultado: %v", f.Requisicoes())
		}
	}
}

// O caminho feliz do contrato: primeira sessão em lotes com o inventário
// completo, a seguinte em delta — sem mudança, nenhum lote e nenhuma
// requisição de arquivo ao bucket; com mudança, só ela.
func TestIndiceEmLotesEDelta(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 40
	m.semear(250)

	s := m.rodarOK()
	if s.Status != "complete" || s.Modo != "full" || s.Lotes != 7 || s.Entradas != 250 || s.Presentes != 250 {
		t.Fatalf("primeira: %+v", s)
	}
	if _, tem := s.Complete["version_map"]; tem || s.Complete["index"] == nil {
		t.Fatalf("o /complete em lotes não leva version_map: %v", s.Complete)
	}
	if !slices.Contains(s.Capacidades, api.CapacidadeIndiceEmLotes) || !slices.Contains(s.Capacidades, api.CapacidadeIndiceDelta) {
		t.Fatalf("capacidades no /start: %v", s.Capacidades)
	}
	m.conferir()

	m.s3.Zerar()
	s = m.rodarOK()
	if s.Modo != "delta" || s.Lotes != 0 || s.Entradas != 0 || s.Presentes != 250 || s.Status != "complete" {
		t.Fatalf("sem mudança: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	st := s.Complete["stats"].(map[string]any)
	if st["files_total"] != float64(250) || st["files_uploaded"] != float64(0) || st["files_new"] != float64(0) {
		t.Fatalf("stats: %v", st)
	}
	m.conferir()

	m.escrever("p00/arquivo-00000.txt", "mudou")
	m.escrever("novo.txt", "novo")
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-00001.txt")); err != nil {
		t.Fatal(err)
	}
	m.s3.Zerar()
	s = m.rodarOK()
	if s.Modo != "delta" || s.Lotes != 1 || s.Entradas != 3 || s.Presentes != 250 || s.RemocoesSemX != 0 {
		t.Fatalf("com mudança: %+v", s)
	}
	if m.s3.N("PutObject") != 2 || m.s3.N("HeadObject") != 2 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
	st = s.Complete["stats"].(map[string]any)
	if st["files_new"] != float64(1) || st["files_changed"] != float64(1) || st["files_removed"] != float64(1) {
		t.Fatalf("stats: %v", st)
	}
	m.conferir()
	for _, l := range m.painel.LotesRecebidos() {
		if l.Bytes > 2<<20 || l.Entradas > 40 {
			t.Fatalf("lote fora do limite: %+v", l)
		}
	}
}

// Lote cuja resposta se perde é mandado de novo, igual, e o painel o aceita
// como o mesmo (idempotência por sessão + seq).
func TestLoteDuplicadoEAceito(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.semear(100)
	var engolidos atomic.Int32
	m.painel.Engolir = func(rota string, corpo []byte) bool {
		return rota == "files" && strings.HasPrefix(string(corpo), `{"seq":2,`) && engolidos.Add(1) == 1
	}
	s := m.rodarOK()
	if s.Lotes != 5 || s.Entradas != 100 || s.Status != "complete" {
		t.Fatalf("%+v", s)
	}
	if r := m.painel.Rotas()["files"]; r.Requisicoes != 6 || r.Status[502] != 1 {
		t.Fatalf("o lote 2 devia ter ido duas vezes: %+v", r)
	}
	m.conferir()
}

// O painel perde lotes já aceitos (a tabela de preparo é UNLOGGED). No meio:
// 409 seq_gap, e o agente manda de novo a partir do que o painel espera. No
// fim: 409 index_mismatch no /complete, e o agente reenvia os que faltam.
func TestLotesPerdidosSaoReenviados(t *testing.T) {
	for _, quando := range []string{"files", "complete"} {
		t.Run(quando, func(t *testing.T) {
			m := novaMesaDoDaemon(t)
			m.painel.MaxEntradas = 20
			m.semear(100)
			var feito atomic.Bool
			m.painel.Responder = func(rota string, r *http.Request, corpo []byte) (int, string) {
				gatilho := rota == "complete"
				if quando == "files" {
					gatilho = rota == "files" && strings.HasPrefix(string(corpo), `{"seq":4,`)
				}
				if gatilho && feito.CompareAndSwap(false, true) {
					m.painel.PerderLotes(m.painel.Ultima().ID, 1)
				}
				return 0, ""
			}
			s := m.rodarOK()
			if len(m.painel.Sessoes()) != 1 || s.Lotes != 5 || s.Entradas != 100 || s.Status != "complete" {
				t.Fatalf("%+v", s)
			}
			if r := m.painel.Rotas()[quando]; r.Status[409] != 1 {
				t.Fatalf("esperava um 409 em %s: %+v", quando, r)
			}
			if m.s3.N("PutObject") != 100 {
				t.Fatalf("nenhum arquivo sobe de novo: %v", m.s3.Requisicoes())
			}
			m.conferir()
		})
	}
}

// Lote recusado de vez (seq_conflict: o painel tem outro conteúdo naquele
// seq): não há como consertar a sessão. Os arquivos subiram e o estado local
// sabe quais — o índice é refeito numa sessão nova, sem reenviar nada.
func TestSeqConflictRefazOIndiceEmOutraSessao(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.semear(100)
	var feito atomic.Bool
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota == "files" && strings.HasPrefix(string(corpo), `{"seq":3,`) && feito.CompareAndSwap(false, true) {
			return 409, `{"error":"seq_conflict"}`
		}
		return 0, ""
	}
	if err := m.rodar(); err != nil {
		t.Fatal(err)
	}
	ss := m.painel.Sessoes()
	if len(ss) != 2 || ss[0].Status != "failed" || ss[0].ErrorCode != codigoIndiceRecusado {
		t.Fatalf("sessões: %+v", ss)
	}
	if ss[1].Status != "complete" || ss[1].Modo != "full" || ss[1].Entradas != 100 || ss[1].Presentes != 100 {
		t.Fatalf("sessão refeita: %+v", ss[1])
	}
	if m.s3.N("PutObject") != 100 || m.s3.N("HeadObject") != 100 {
		t.Fatalf("refazer o índice não toca no bucket: %v", m.s3.Requisicoes())
	}
	m.conferir()
	// O estado ficou confirmado na sessão refeita: a seguinte é delta e vazia.
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("depois de refeito: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
}

// 409 base_mismatch: a base do delta não é a última sessão concluída no
// painel. A sessão é refeita em modo full, numa sessão nova — sem repetir os
// comandos do plano e com o que o comando de depois já apagou (o dump) ainda
// no inventário, porque ele saiu do estado, não de outra varredura.
func TestBaseMismatchRefazEmFull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comandos de shell do teste são POSIX")
	}
	m := novaMesaDoDaemon(t)
	m.semear(60)
	dump := filepath.Join(m.raiz, "dump.sql")
	conta := filepath.Join(t.TempDir(), "vezes")
	m.plano.PreHook = fmt.Sprintf("echo dump-de-hoje > %q && touch -d '1 hour ago' %q && echo x >> %q", dump, dump, conta)
	m.plano.PostHook = fmt.Sprintf("rm -f %q", dump)
	m.rodarOK()

	m.escrever("p00/arquivo-00000.txt", "mudou")
	m.painel.ForcarBaseErrada(1)
	m.s3.Zerar()
	if err := m.rodar(); err != nil {
		t.Fatal(err)
	}
	ss := m.painel.Sessoes()
	if len(ss) != 3 || ss[1].Status != "failed" || ss[1].ErrorCode != codigoIndiceRecusado {
		t.Fatalf("sessões: %d; a do delta recusado: %+v", len(ss), ss[1])
	}
	nova := ss[2]
	if nova.Status != "complete" || nova.Modo != "full" || nova.Entradas != 61 || nova.Presentes != 61 {
		t.Fatalf("sessão refeita: %+v", nova)
	}
	cat := m.painel.Catalogo("ag1", "p1")
	if _, tem := cat[m.chave("dump.sql")]; !tem {
		t.Fatal("o dump do comando de antes tinha de estar no inventário da sessão refeita")
	}
	if b, _ := os.ReadFile(conta); strings.Count(string(b), "x") != 2 {
		t.Fatalf("os comandos do plano rodam uma vez por backup, não por sessão: %q", b)
	}
	// O dump é o mesmo conteúdo da véspera (dedup) e um arquivo mudou.
	if m.s3.N("PutObject") != 1 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
	// A seguinte volta ao delta sobre a sessão refeita.
	s := m.rodarOK()
	if s.Modo != "delta" || s.Base != nova.ID {
		t.Fatalf("depois de refeita: %+v", s)
	}
}

// Painel fora do ar no meio do backup: os lotes esperam no disco e o backup
// não para; quando o painel volta, chegam todos.
func TestPainelForaDoArNoMeioNaoParaOBackup(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.semear(100)
	var n atomic.Int32
	m.painel.Responder = func(rota string, _ *http.Request, _ []byte) (int, string) {
		if rota == "files" && n.Add(1) <= 8 {
			return 503, "fora do ar"
		}
		return 0, ""
	}
	s := m.rodarOK()
	if s.Lotes != 5 || s.Entradas != 100 || s.Status != "complete" {
		t.Fatalf("%+v", s)
	}
	m.conferir()
}

// Painel de antes do contrato (produção, v2.76.0): sem `index` na resposta do
// /start. O /complete é o de sempre — os mesmos campos, o version_map inteiro
// na ordem da varredura — e nada vai para /files.
func TestPainelAntigoRecebeOCompleteInlineDeSempre(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.SemLotes = true
	m.semear(120)
	var corpos []string
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota == "complete" {
			corpos = append(corpos, string(corpo))
		}
		return 0, ""
	}
	conferirCorpo := func(corpo string) {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(corpo), &got); err != nil {
			t.Fatal(err)
		}
		var campos []string
		for k := range got {
			campos = append(campos, k)
		}
		slices.Sort(campos)
		if fmt.Sprint(campos) != "[cloud_only_skipped reparse_skipped stats status version_map]" {
			t.Fatalf("campos do /complete inline: %v", campos)
		}
		var st map[string]any
		_ = json.Unmarshal(got["stats"], &st)
		if len(st) != 5 || st["files_total"] != float64(120) {
			t.Fatalf("stats do /complete inline: %v", st)
		}
		// A ordem dos campos no corpo é a da v0.4.31.
		if !strings.HasPrefix(corpo, `{"status":"complete","stats":{"files_total":120,"files_uploaded":`) ||
			!strings.Contains(corpo, `"bytes_total":`) || !strings.HasSuffix(strings.TrimSpace(corpo), `"cloud_only_skipped":0,"reparse_skipped":0}`) {
			t.Fatalf("forma do corpo mudou: %.120s … %s", corpo, corpo[len(corpo)-60:])
		}
		var mapa []map[string]any
		_ = json.Unmarshal(got["version_map"], &mapa)
		if len(mapa) != 120 {
			t.Fatalf("version_map com %d entradas", len(mapa))
		}
		var ordem []string
		_ = filepath.WalkDir(m.raiz, func(p string, d fs.DirEntry, _ error) error {
			if !d.IsDir() {
				ordem = append(ordem, "data/ag1/"+strings.TrimPrefix(filepath.ToSlash(p), "/"))
			}
			return nil
		})
		for i, e := range mapa {
			if len(e) != 5 || e["key"] != ordem[i] || e["version_id"] == "" || len(e["sha256"].(string)) != 64 || e["modified_at"] == nil {
				t.Fatalf("entrada %d do version_map: %v (queria %s)", i, e, ordem[i])
			}
		}
	}

	s := m.rodarOK()
	if s.Modo != "inline" || s.Indexados != 120 || len(s.Capacidades) == 0 {
		t.Fatalf("%+v", s)
	}
	if r := m.painel.Rotas()["files"]; r.Requisicoes != 0 {
		t.Fatalf("painel antigo recebeu /files: %+v", r)
	}
	conferirCorpo(corpos[0])
	m.conferir()

	// Segunda vez: mesmo corpo (fora o que subiu). Contra o painel antigo o
	// agente é a v0.4.31: relê e confere tudo no bucket, e não deixa estado
	// local nenhum no disco.
	m.s3.Zerar()
	m.rodarOK()
	conferirCorpo(corpos[1])
	if m.s3.N("HeadObject") != 120 || m.s3.N("PutObject") != 0 {
		t.Fatalf("painel antigo: queria 120 HeadObject e nenhum envio; s3: %v", m.s3.Requisicoes())
	}
	if restos, _ := os.ReadDir(m.cfg.StateDir); len(restos) != 0 {
		t.Fatalf("painel antigo: a pasta do estado devia seguir vazia; tem %d arquivo(s) (%s…)", len(restos), restos[0].Name())
	}
	m.conferir()
}

// ARKAME_STATE=always: o estado local vale também contra o painel antigo — o
// /complete segue inline, mas a segunda execução não relê nem consulta o
// bucket.
func TestPainelAntigoComEstadoForcado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.SemLotes = true
	m.cfg.StateAlways = true
	m.semear(60)
	if s := m.rodarOK(); s.Modo != "inline" || s.Indexados != 60 {
		t.Fatalf("%+v", s)
	}
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "inline" || s.Indexados != 60 {
		t.Fatalf("%+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	if restos, _ := os.ReadDir(m.cfg.StateDir); len(restos) == 0 {
		t.Fatal("com o estado forçado, o arquivo de estado devia existir")
	}
	m.conferir()
}

// O painel passa a oferecer lotes depois de o agente novo já estar em campo:
// a primeira sessão em lotes parte sem estado (inventário completo, tudo
// conferido no bucket, nada reenviado) e daí em diante vale o delta.
func TestPainelPassaAOferecerLotes(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.SemLotes = true
	m.semear(40)
	m.rodarOK()
	m.painel.SemLotes = false
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "full" || s.Entradas != 40 {
		t.Fatalf("primeira em lotes: %+v", s)
	}
	if m.s3.N("HeadObject") != 40 || m.s3.N("PutObject") != 0 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Lotes != 0 {
		t.Fatalf("segunda em lotes: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	m.conferir()
}

// `run --rehash-all` vale para UM backup de cada plano: o seguinte volta ao
// atalho. ARKAME_REHASH_ALL=1 vale enquanto estiver ligada.
func TestRehashAllPelaFlagValeUmaExecucao(t *testing.T) {
	t.Cleanup(func() { rehashFeito.Delete("p1") })
	m := novaMesaDoDaemon(t)
	m.semear(30)
	m.rodarOK()

	m.cfg.RehashAllOnce = true
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Lotes != 0 {
		t.Fatalf("%+v", s)
	}
	if m.s3.N("HeadObject") != 30 {
		t.Fatalf("com --rehash-all o primeiro backup confere tudo no bucket; s3: %v", m.s3.Requisicoes())
	}
	m.s3.Zerar()
	m.rodarOK()
	semS3AlemDoVersionamento(t, m.s3)

	m.cfg.RehashAllOnce, m.cfg.RehashAll = false, true
	for i := 0; i < 2; i++ {
		m.s3.Zerar()
		m.rodarOK()
		if m.s3.N("HeadObject") != 30 {
			t.Fatalf("ARKAME_REHASH_ALL=1 vale em todo backup; execução %d, s3: %v", i+1, m.s3.Requisicoes())
		}
	}
}

// O 413 index_too_large do painel de produção: uma tentativa só, a sessão é
// marcada como falha (complete_failed) e o backup devolve o erro — como a
// v0.4.31. O estado local não é confirmado.
func TestPainelAntigoRecusaIndiceGrande(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.SemLotes = true
	m.painel.MaxInlineArquivos = 50
	m.semear(80)
	err := m.rodar()
	if err == nil || !strings.Contains(err.Error(), "index_too_large") || !strings.Contains(err.Error(), "413") {
		t.Fatalf("erro = %v", err)
	}
	rotas := m.painel.Rotas()
	if rotas["complete"].Requisicoes != 1 || rotas["fail"].Requisicoes != 1 {
		t.Fatalf("413 é recusa definitiva: uma tentativa e um /fail; rotas=%+v", rotas)
	}
	if s := m.painel.Ultima(); s.Status != "failed" || s.ErrorCode != "index_too_large" {
		t.Fatalf("%+v", s)
	}
	// Não confirmado: a execução seguinte confere tudo no bucket de novo (e
	// não reenvia).
	m.s3.Zerar()
	_ = m.rodar()
	if m.s3.N("HeadObject") != 80 || m.s3.N("PutObject") != 0 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
}

// O /complete chegou e só a resposta se perdeu: na sessão seguinte o painel
// dá aquela sessão como base, e o estado a aceita como confirmada — sem
// reler os arquivos dela.
func TestCompleteComRespostaPerdidaConfirmaPelaBase(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(50)
	var perder atomic.Bool
	perder.Store(true)
	m.painel.Engolir = func(rota string, _ []byte) bool { return rota == "complete" && perder.Load() }
	if err := m.rodar(); err == nil {
		t.Fatal("sem resposta do /complete o backup devolve erro")
	}
	if s := m.painel.Ultima(); s.Status != "complete" {
		t.Fatalf("no painel a sessão fechou: %+v", s)
	}
	perder.Store(false)
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("%+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	m.conferir()
}

// Depois do expurgo, a entrada da versão apagada sai do estado: o arquivo é
// conferido de novo no bucket em vez de o atalho afirmar o que não existe.
func TestExpurgoDaBaixaNoEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(30)
	m.rodarOK()
	k := m.chave("p03/arquivo-00003.txt")
	v, _ := m.s3.Atual(k)
	m.s3.ApagarVersao(k, v.ID)
	esquecerVersoesApagadas(m.cfg, []purge.Version{{Key: k, VersionID: v.ID}})
	m.s3.Zerar()
	s := m.rodarOK()
	if m.s3.N("HeadObject") != 1 || m.s3.N("PutObject") != 1 || s.Entradas != 1 {
		t.Fatalf("s3=%v sessão=%+v", m.s3.Requisicoes(), s)
	}
	m.conferir()
}

// Sessão parcial fecha em delta, sem `removed`.
func TestSessaoParcialFechaEmDeltaSemRemoved(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("precisa de um arquivo ilegível")
	}
	m := novaMesaDoDaemon(t)
	m.semear(30)
	m.rodarOK()
	_ = os.Remove(filepath.Join(m.raiz, "p01/arquivo-00001.txt"))
	trancado := filepath.Join(m.raiz, "p02/arquivo-00002.txt")
	_ = os.WriteFile(trancado, []byte("mudou"), 0o000)
	_ = os.Chmod(trancado, 0o000)
	s := m.rodarOK()
	if s.Status != "partial" || s.Modo != "delta" || s.Entradas != 0 || s.Presentes != 30 {
		t.Fatalf("%+v", s)
	}
}

// Painel fora do ar do meio do backup até o fim das tentativas: o /complete
// não é mandado com o índice pela metade. A sessão vai como falha, o estado
// não é confirmado, e o backup seguinte manda o índice sem reenviar arquivo.
func TestPainelForaDoArAteOFimFalhaSemConfirmar(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.semear(100)
	var fora atomic.Bool
	fora.Store(true)
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota == "files" && fora.Load() && !strings.HasPrefix(string(corpo), `{"seq":1,`) {
			return 503, "fora do ar"
		}
		return 0, ""
	}
	if err := m.rodar(); err == nil {
		t.Fatal("sem o índice inteiro o backup devolve erro")
	}
	rotas := m.painel.Rotas()
	if rotas["complete"].Requisicoes != 0 || rotas["fail"].Requisicoes != 1 {
		t.Fatalf("nem /complete com índice pela metade, e um /fail: %+v", rotas)
	}
	if s := m.painel.Ultima(); s.Status != "failed" || s.ErrorCode != "complete_failed" {
		t.Fatalf("%+v", s)
	}
	fora.Store(false)
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Status != "complete" || s.Modo != "full" || s.Entradas != 100 || m.s3.N("PutObject") != 0 || m.s3.N("HeadObject") != 100 {
		t.Fatalf("seguinte: %+v s3=%v", s, m.s3.Requisicoes())
	}
	m.conferir()
}

// 413 `index_too_large` num lote: o teto é da sessão (150 mil enquanto o
// painel ainda grava o catálogo antigo), e o painel a fecha como falha na
// mesma resposta. É recusa definitiva, como no /complete inline: o agente não
// divide o lote, não manda /complete e NÃO refaz o índice em outra sessão.
func TestIndiceGrandeDemaisEmLotesERecusaDefinitiva(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.painel.MaxArquivosDaSessao = 50
	m.semear(80)
	err := m.rodar()
	if err == nil || !strings.Contains(err.Error(), "index_too_large") || !strings.Contains(err.Error(), "413") {
		t.Fatalf("erro = %v", err)
	}
	sessoes := m.painel.Sessoes()
	if len(sessoes) != 1 {
		t.Fatalf("recusa definitiva não abre outra sessão: %d sessões", len(sessoes))
	}
	if s := sessoes[0]; s.Status != "failed" || s.ErrorCode != "index_too_large" || s.Lotes != 2 {
		t.Fatalf("%+v", s)
	}
	rotas := m.painel.Rotas()
	if rotas["complete"].Requisicoes != 0 || rotas["files"].Status[413] != 1 || rotas["start"].Requisicoes != 1 {
		t.Fatalf("um 413 só, sem /complete e sem outra sessão: %+v", rotas)
	}
	// Nada foi confirmado no estado: a execução seguinte confere no bucket o
	// que já subiu (sem reenviar) e é recusada de novo.
	enviados := m.s3.N("PutObject")
	m.s3.Zerar()
	if err := m.rodar(); err == nil || !strings.Contains(err.Error(), "index_too_large") {
		t.Fatalf("segunda execução: %v", err)
	}
	if m.s3.N("PutObject") > 80-enviados {
		t.Fatalf("arquivo reenviado depois da recusa: %v (a primeira enviou %d)", m.s3.Requisicoes(), enviados)
	}
}

// 413 `batch_too_large`: o lote não coube (o painel, ou um proxy no caminho,
// aceita menos do que foi anunciado). O agente divide o lote ao meio e segue;
// a contagem do /complete acompanha os lotes que o painel de fato aceitou.
func TestLoteGrandeDemaisEDividido(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(64)
	const cabe = 6000
	recusados := 0
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota == "files" && len(corpo) > cabe {
			recusados++
			return 413, `{"error":"batch_too_large","limit":{"entries":5000,"bytes":6000}}`
		}
		return 0, ""
	}
	s := m.rodarOK()
	if recusados == 0 {
		t.Fatal("o teste não exercitou a divisão: nenhum lote passou do que cabe")
	}
	if s.Status != "complete" || s.Modo != "full" || s.Entradas != 64 || s.Lotes < 2 {
		t.Fatalf("%+v", s)
	}
	for _, l := range m.painel.LotesRecebidos() {
		if l.Bytes > cabe {
			t.Fatalf("lote de %d bytes aceito acima do que cabe", l.Bytes)
		}
	}
	if len(m.painel.Sessoes()) != 1 {
		t.Fatalf("dividir o lote não abre outra sessão: %d", len(m.painel.Sessoes()))
	}
	m.conferir()
}
