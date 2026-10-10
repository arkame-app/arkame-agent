package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/pkg/version"
)

// trocaFalsa é o atualizacao.Aplicar dos testes: conta as chamadas, guarda o
// pedido e devolve o que o teste mandar.
type trocaFalsa struct {
	chamadas atomic.Int32
	pedido   atualizacao.Pedido
	durante  func()
	err      error
}

func (f *trocaFalsa) aplicar(_ context.Context, p atualizacao.Pedido, _ atualizacao.Opcoes) (*atualizacao.Resultado, error) {
	f.chamadas.Add(1)
	f.pedido = p
	if f.durante != nil {
		f.durante()
	}
	if f.err != nil {
		return nil, f.err
	}
	return &atualizacao.Resultado{Versao: p.Versao}, nil
}

// atualizadorDeTeste é um atualizador sem impedimento, com a troca falsa.
func atualizadorDeTeste(t *testing.T, f *trocaFalsa) (*atualizador, *atomic.Int32) {
	t.Helper()
	var reinicios atomic.Int32
	dir := t.TempDir()
	a := &atualizador{
		cfg:        &config.Config{AgentID: "a1", ConfigPath: filepath.Join(dir, "agent.env")},
		oc:         &ocupacao{trava: &exclusaoDoBucket{}},
		reiniciar:  func() { reinicios.Add(1) },
		programa:   filepath.Join(dir, "arkame-agent"),
		pedidos:    make(chan api.UpdateOffer, 1),
		naoAntesDe: map[string]time.Time{},
		aplicar:    f.aplicar,
	}
	return a, &reinicios
}

var oferta = api.UpdateOffer{Version: "0.5.0", SHA256: strings.Repeat("ab", 32), BaseURL: "https://get.arkame.app/releases", File: "arkame-agent_linux_amd64"}

// Atualização pedida com backup em curso: adiada, sem baixar nada, e o painel
// fica sabendo por quê. Com o bucket livre, acontece.
func TestAtualizacaoPedidaDuranteBackupEAdiada(t *testing.T) {
	f := &trocaFalsa{}
	a, reinicios := atualizadorDeTeste(t, f)

	emCurso, terminar := make(chan struct{}), make(chan struct{})
	fim := make(chan error, 1)
	go func() {
		fim <- a.oc.trava.backup(func() error { close(emCurso); <-terminar; return nil })
	}()
	<-emCurso

	a.tentar(context.Background(), oferta)
	if n := f.chamadas.Load(); n != 0 {
		t.Fatalf("a troca rodou %d vez(es) com backup em curso", n)
	}
	if reinicios.Load() != 0 {
		t.Fatal("o daemon foi encerrado com backup em curso")
	}
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "deferred" || r.Error != atualizacao.AdiadaBackup || r.Version != "0.5.0" {
		t.Fatalf("relato = %+v", r)
	}

	close(terminar)
	<-fim
	a.tentar(context.Background(), oferta)
	if n := f.chamadas.Load(); n != 1 {
		t.Fatalf("com o bucket livre, a troca rodou %d vez(es)", n)
	}
	if reinicios.Load() != 1 {
		t.Fatal("troca feita e o daemon não foi encerrado")
	}
	if f.pedido.Versao != "0.5.0" || f.pedido.Sha256 != oferta.SHA256 || f.pedido.BaseURL != oferta.BaseURL || f.pedido.Arquivo != oferta.File {
		t.Fatalf("pedido passado à troca = %+v", f.pedido)
	}
}

// Com o expurgo de retenção em curso, o motivo é o expurgo.
func TestAtualizacaoPedidaDuranteExpurgoEAdiada(t *testing.T) {
	f := &trocaFalsa{}
	a, _ := atualizadorDeTeste(t, f)
	emCurso, terminar := make(chan struct{}), make(chan struct{})
	fim := make(chan bool, 1)
	go func() { fim <- a.oc.trava.limpeza(func() { close(emCurso); <-terminar }) }()
	<-emCurso
	a.tentar(context.Background(), oferta)
	close(terminar)
	<-fim
	if f.chamadas.Load() != 0 {
		t.Fatal("a troca rodou com expurgo em curso")
	}
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "deferred" || r.Error != atualizacao.AdiadaExpurgo {
		t.Fatalf("relato = %+v", r)
	}
}

// Atualização pedida com restauração em curso: adiada.
func TestAtualizacaoPedidaDuranteRestauracaoEAdiada(t *testing.T) {
	f := &trocaFalsa{}
	a, reinicios := atualizadorDeTeste(t, f)
	if !a.oc.comecarRestauracao() {
		t.Fatal("a restauração não começou")
	}
	a.tentar(context.Background(), oferta)
	if f.chamadas.Load() != 0 || reinicios.Load() != 0 {
		t.Fatal("a troca rodou com restauração em curso")
	}
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "deferred" || r.Error != atualizacao.AdiadaRestauracao {
		t.Fatalf("relato = %+v", r)
	}
	a.oc.terminarRestauracao()
	a.tentar(context.Background(), oferta)
	if f.chamadas.Load() != 1 || reinicios.Load() != 1 {
		t.Fatalf("sem restauração: chamadas=%d reinícios=%d", f.chamadas.Load(), reinicios.Load())
	}
}

// Enquanto a troca acontece, nem backup, nem expurgo, nem restauração começam
// — e o daemon é encerrado ainda com o bucket fechado.
func TestDuranteATrocaNadaComeca(t *testing.T) {
	f := &trocaFalsa{}
	a, _ := atualizadorDeTeste(t, f)
	f.durante = func() {
		if a.oc.comecarRestauracao() {
			t.Error("uma restauração começou durante a troca")
		}
		if a.oc.trava.limpeza(func() {}) {
			t.Error("um expurgo começou durante a troca")
		}
		if a.oc.trava.mu.TryLock() {
			t.Error("o bucket ficou livre para um backup durante a troca")
		}
	}
	a.reiniciar = func() {
		if a.oc.trava.mu.TryLock() {
			t.Error("o bucket foi liberado antes de o daemon ser encerrado")
		}
	}
	a.tentar(context.Background(), oferta)
	if f.chamadas.Load() != 1 {
		t.Fatal("a troca não rodou")
	}
	// Depois de tudo, a trava volta (o processo de verdade já saiu).
	if !a.oc.trava.mu.TryLock() {
		t.Fatal("a trava do bucket ficou presa depois da troca")
	}
}

// A troca que falha vai ao painel com o código, o programa segue, e a mesma
// versão não é tentada de novo a cada heartbeat — só com `force`.
func TestFalhaDaTrocaERelatadaENaoSeRepete(t *testing.T) {
	f := &trocaFalsa{err: &atualizacao.Falha{Codigo: atualizacao.FalhaAssinatura, Err: errors.New("x")}}
	a, reinicios := atualizadorDeTeste(t, f)
	a.tentar(context.Background(), oferta)
	if reinicios.Load() != 0 {
		t.Fatal("a troca falhou e o daemon foi encerrado")
	}
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "failed" || r.Error != atualizacao.FalhaAssinatura || r.Version != "0.5.0" {
		t.Fatalf("relato = %+v", r)
	}
	if !a.oc.trava.mu.TryLock() {
		t.Fatal("a trava do bucket ficou presa depois da falha")
	}
	a.oc.trava.mu.Unlock()

	a.tentar(context.Background(), oferta)
	if n := f.chamadas.Load(); n != 1 {
		t.Fatalf("a versão que falhou foi tentada de novo (%d chamadas)", n)
	}
	forcada := oferta
	forcada.Force = true
	a.tentar(context.Background(), forcada)
	if n := f.chamadas.Load(); n != 2 || !f.pedido.Forcar {
		t.Fatalf("com force: %d chamadas, forçar=%v", n, f.pedido.Forcar)
	}
	// Outra versão não herda a espera.
	outra := oferta
	outra.Version = "0.5.1"
	a.tentar(context.Background(), outra)
	if n := f.chamadas.Load(); n != 3 {
		t.Fatalf("a versão seguinte não foi tentada (%d chamadas)", n)
	}
}

// A troca adiada por dentro (outra troca na máquina) não conta como falha.
func TestTrocaAdiadaNaoEFalha(t *testing.T) {
	f := &trocaFalsa{err: &atualizacao.Adiada{Motivo: atualizacao.AdiadaOutraTroca}}
	a, _ := atualizadorDeTeste(t, f)
	a.tentar(context.Background(), oferta)
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "deferred" || r.Error != atualizacao.AdiadaOutraTroca {
		t.Fatalf("relato = %+v", r)
	}
	a.tentar(context.Background(), oferta)
	if f.chamadas.Load() != 2 {
		t.Fatal("a troca adiada não foi tentada de novo")
	}
}

// Agente que não se atualiza sozinho: não declara a capacidade, conta ao
// painel o impedimento e ignora o `update` que vier.
func TestAgenteImpedidoNaoDeclaraNemAplica(t *testing.T) {
	f := &trocaFalsa{}
	a, _ := atualizadorDeTeste(t, f)
	a.impedimento = atualizacao.ImpedidaPastaSoLeitura
	if c := a.capacidades(); c == nil || len(c) != 0 {
		t.Fatalf("capacidades = %#v, queria a lista vazia", c)
	}
	if r := a.relatoDoHeartbeat(); r == nil || r.Blocked != atualizacao.ImpedidaPastaSoLeitura {
		t.Fatalf("relato = %+v", r)
	}
	a.receber(&oferta)
	select {
	case <-a.pedidos:
		t.Fatal("o pedido entrou na fila de um agente impedido")
	default:
	}
}

// painelDoHeartbeat guarda o corpo do heartbeat e responde o que o teste mandar.
func painelDoHeartbeat(t *testing.T, resposta func() (int, string)) (*api.Client, func() map[string]any) {
	t.Helper()
	var mu gosync.Mutex
	var corpo map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		corpo = nil
		_ = json.Unmarshal(b, &corpo)
		mu.Unlock()
		status, texto := resposta()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, texto)
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	return c, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return corpo
	}
}

// O heartbeat declara as capacidades (sempre lista) e leva o relato; o
// `update` da resposta vai para o atualizador.
func TestHeartbeatDeclaraCapacidadesEEntregaOPedido(t *testing.T) {
	c, corpo := painelDoHeartbeat(t, func() (int, string) {
		return 200, `{"ok":true,"update":{"version":"0.5.0","sha256":"` + oferta.SHA256 + `","base_url":"https://get.arkame.app/releases","file":"arkame-agent_linux_amd64","force":true}}`
	})
	a, _ := atualizadorDeTeste(t, &trocaFalsa{})
	a.relatar("deferred", "0.5.0", atualizacao.AdiadaBackup)
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)

	enviado := corpo()
	caps, ok := enviado["capabilities"].([]any)
	if !ok || len(caps) != 1 || caps[0] != atualizacao.Capacidade {
		t.Fatalf("capabilities = %#v", enviado["capabilities"])
	}
	relato, _ := enviado["update"].(map[string]any)
	if relato["state"] != "deferred" || relato["version"] != "0.5.0" || relato["error"] != atualizacao.AdiadaBackup {
		t.Fatalf("update = %#v", enviado["update"])
	}
	select {
	case p := <-a.pedidos:
		if p.Version != "0.5.0" || p.SHA256 != oferta.SHA256 || !p.Force || p.File != "arkame-agent_linux_amd64" {
			t.Fatalf("pedido = %+v", p)
		}
	default:
		t.Fatal("o `update` da resposta não chegou ao atualizador")
	}

	// Sem atualizador (o heartbeat de um comando avulso) e com o agente
	// impedido, o campo vai como lista vazia — ausente é agente antigo.
	enviarHeartbeat(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{})
	if caps, ok := corpo()["capabilities"].([]any); !ok || len(caps) != 0 {
		t.Fatalf("sem atualizador: capabilities = %#v", corpo()["capabilities"])
	}
	if _, tem := corpo()["update"]; tem {
		t.Fatalf("sem nada a relatar, o heartbeat levou update: %#v", corpo()["update"])
	}
}

// instalacaoEmProva monta, numa pasta, o programa novo no lugar, o anterior ao
// lado e o registro da troca em prova — como a troca deixa.
func instalacaoEmProva(t *testing.T) (programa string) {
	t.Helper()
	return instalacaoEmProvaDesde(t, time.Date(2026, 10, 5, 2, 0, 0, 0, time.UTC))
}

// instalacaoEmProvaDesde é a instalação em prova com a troca feita em quando
// (o teto da prova conta dali).
func instalacaoEmProvaDesde(t *testing.T, quando time.Time) (programa string) {
	t.Helper()
	dir := t.TempDir()
	programa = filepath.Join(dir, "arkame-agent")
	l := atualizacao.LugarDe(programa)
	if err := os.WriteFile(l.Atual, []byte("novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Anterior, []byte("anterior"), 0o755); err != nil {
		t.Fatal(err)
	}
	registro := fmt.Sprintf(`{"situacao":"em_prova","versao":%q,"versao_anterior":"0.4.31","sha256":"x","aplicada_em":%q}`,
		version.Version, quando.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(l.Estado, []byte(registro), 0o644); err != nil {
		t.Fatal(err)
	}
	return programa
}

func lerArquivo(t *testing.T, caminho string) string {
	t.Helper()
	b, err := os.ReadFile(caminho)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// O programa novo sobe e o painel não aceita nenhum heartbeat dele no prazo
// da prova: o anterior volta ao lugar e o daemon encerra para o serviço subir
// o anterior.
func TestProgramaNovoSemHeartbeatVoltaAoAnterior(t *testing.T) {
	antes := atualizacao.PrazoDaProva
	t.Cleanup(func() { atualizacao.PrazoDaProva = antes })
	atualizacao.PrazoDaProva = 150 * time.Millisecond

	// A troca é de agora: o teto da prova está longe, e mesmo assim quem
	// ouve "não" do painel volta no prazo normal.
	programa := instalacaoEmProvaDesde(t, time.Now())
	a, reinicios := atualizadorDeTeste(t, &trocaFalsa{})
	a.programa = programa
	a.prova = atualizacao.AbrirProva(programa, version.Version)
	if !a.prova.Aberta() {
		t.Fatal("a instalação em prova não abriu a prova")
	}
	// O painel recusa todos os heartbeats do programa novo.
	c, _ := painelDoHeartbeat(t, func() (int, string) { return 401, `{"error":"x"}` })
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)

	a.vigiarProva(context.Background())
	if reinicios.Load() != 1 {
		t.Fatalf("reinícios = %d, queria 1", reinicios.Load())
	}
	if got := lerArquivo(t, programa); got != "anterior" {
		t.Fatalf("no lugar ficou %q, queria o programa anterior", got)
	}
	if !strings.Contains(lerArquivo(t, atualizacao.LugarDe(programa).Estado), `"situacao": "revertida"`) {
		t.Fatalf("registro:\n%s", lerArquivo(t, atualizacao.LugarDe(programa).Estado))
	}
	// Com a troca desfeita, o agente não aceita outra enquanto sai.
	a.tentar(context.Background(), oferta)
	if reinicios.Load() != 1 {
		t.Fatal("uma troca nova começou com o daemon de saída")
	}
}

// prazosDaProva encurta o prazo, o passo e o teto da prova para o teste.
func prazosDaProva(t *testing.T, prazo, passo, teto time.Duration) {
	t.Helper()
	p, s, x := atualizacao.PrazoDaProva, atualizacao.PassoDaExtensao, atualizacao.TetoDaProva
	t.Cleanup(func() { atualizacao.PrazoDaProva, atualizacao.PassoDaExtensao, atualizacao.TetoDaProva = p, s, x })
	atualizacao.PrazoDaProva, atualizacao.PassoDaExtensao, atualizacao.TetoDaProva = prazo, passo, teto
}

// O painel cai logo depois da troca (5xx, 429, ou nem responde): o prazo da
// prova vence e o programa novo NÃO é desfeito — a prova é estendida. Quando o
// painel volta e aceita um heartbeat, a troca é confirmada.
func TestProvaComPainelForaDoArEEstendida(t *testing.T) {
	for nome, status := range map[string]int{"500": 500, "503": 503, "429": 429, "sem resposta": 0} {
		t.Run(nome, func(t *testing.T) {
			prazosDaProva(t, 100*time.Millisecond, 30*time.Millisecond, time.Minute)
			programa := instalacaoEmProvaDesde(t, time.Now())
			a, reinicios := atualizadorDeTeste(t, &trocaFalsa{})
			a.programa = programa
			a.prova = atualizacao.AbrirProva(programa, version.Version)

			var noAr atomic.Bool
			c, _ := painelDoHeartbeat(t, func() (int, string) {
				if noAr.Load() {
					return 200, `{"ok":true}`
				}
				return status, `{"error":"x"}`
			})
			if status == 0 {
				// Ninguém escutando: erro de rede.
				morto := httptest.NewServer(http.NotFoundHandler())
				morto.Close()
				fora, _ := api.New(api.Options{BaseURL: morto.URL, Bearer: "t"})
				enviarHeartbeatCom(context.Background(), fora, a.cfg, service.Detected{}, &estadoDoToken{}, a)
			} else {
				enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)
			}

			fim := make(chan struct{})
			go func() { a.vigiarProva(context.Background()); close(fim) }()
			select {
			case <-fim:
				t.Fatalf("a prova terminou com o painel fora do ar: reinícios=%d, no lugar %q", reinicios.Load(), lerArquivo(t, programa))
			case <-time.After(4 * atualizacao.PrazoDaProva):
			}
			if got := lerArquivo(t, programa); got != "novo" || reinicios.Load() != 0 {
				t.Fatalf("o programa novo foi desfeito com o painel fora do ar: no lugar %q, reinícios %d", got, reinicios.Load())
			}
			// O painel volta.
			noAr.Store(true)
			enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)
			select {
			case <-fim:
			case <-time.After(5 * time.Second):
				t.Fatal("a prova confirmada não encerrou a vigia")
			}
			if reinicios.Load() != 0 || lerArquivo(t, programa) != "novo" {
				t.Fatal("a troca confirmada depois da extensão foi desfeita")
			}
			if !strings.Contains(lerArquivo(t, atualizacao.LugarDe(programa).Estado), `"situacao": "confirmada"`) {
				t.Fatalf("registro:\n%s", lerArquivo(t, atualizacao.LugarDe(programa).Estado))
			}
		})
	}
}

// A extensão tem teto: o painel fora do ar além dele, o anterior volta.
func TestProvaEstendidaVoltaNoTeto(t *testing.T) {
	prazosDaProva(t, 50*time.Millisecond, 20*time.Millisecond, 400*time.Millisecond)
	programa := instalacaoEmProvaDesde(t, time.Now())
	a, reinicios := atualizadorDeTeste(t, &trocaFalsa{})
	a.programa = programa
	a.prova = atualizacao.AbrirProva(programa, version.Version)
	c, _ := painelDoHeartbeat(t, func() (int, string) { return 503, `{"error":"x"}` })
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)

	inicio := time.Now()
	a.vigiarProva(context.Background())
	if d := time.Since(inicio); d < 300*time.Millisecond {
		t.Fatalf("voltou em %v: não estendeu até o teto", d)
	}
	if reinicios.Load() != 1 || lerArquivo(t, programa) != "anterior" {
		t.Fatalf("passado o teto, o anterior devia voltar: reinícios=%d, no lugar %q", reinicios.Load(), lerArquivo(t, programa))
	}
}

// O processo que nem chegou a tentar um heartbeat não ganha extensão.
func TestProvaSemNenhumHeartbeatVoltaNoPrazo(t *testing.T) {
	prazosDaProva(t, 80*time.Millisecond, 20*time.Millisecond, time.Minute)
	programa := instalacaoEmProvaDesde(t, time.Now())
	a, reinicios := atualizadorDeTeste(t, &trocaFalsa{})
	a.programa = programa
	a.prova = atualizacao.AbrirProva(programa, version.Version)
	a.vigiarProva(context.Background())
	if reinicios.Load() != 1 || lerArquivo(t, programa) != "anterior" {
		t.Fatalf("reinícios=%d, no lugar %q", reinicios.Load(), lerArquivo(t, programa))
	}
}

// O relato leva o instante do estado relatado (`at`): o da reversão vem do
// registro do disco; o da tentativa em curso, de quando ela foi anotada.
func TestRelatoDoHeartbeatLevaOInstante(t *testing.T) {
	programa := instalacaoEmProva(t)
	l := atualizacao.LugarDe(programa)
	registro := `{"situacao":"revertida","versao":"0.5.0","sha256":"x","aplicada_em":"2026-10-05T02:00:00Z","mudou_em":"2026-10-05T02:05:00Z","motivo":"no_heartbeat"}`
	if err := os.WriteFile(l.Estado, []byte(registro), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := atualizadorDeTeste(t, &trocaFalsa{})
	a.programa = programa
	c, corpo := painelDoHeartbeat(t, func() (int, string) { return 200, `{"ok":true}` })
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)
	relato, _ := corpo()["update"].(map[string]any)
	if relato["state"] != "rolled_back" || relato["at"] != "2026-10-05T02:05:00Z" {
		t.Fatalf("update = %#v", corpo()["update"])
	}

	antes := time.Now().Add(-time.Second)
	a.relatar("failed", "0.5.0", atualizacao.FalhaDownload)
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)
	relato, _ = corpo()["update"].(map[string]any)
	em, err := time.Parse(time.RFC3339Nano, fmt.Sprint(relato["at"]))
	if err != nil || em.Before(antes) || relato["state"] != "failed" {
		t.Fatalf("update = %#v (%v)", corpo()["update"], err)
	}
}

// Com um heartbeat aceito dentro do prazo, o programa novo fica.
func TestProgramaNovoComHeartbeatFica(t *testing.T) {
	antes := atualizacao.PrazoDaProva
	t.Cleanup(func() { atualizacao.PrazoDaProva = antes })
	atualizacao.PrazoDaProva = 150 * time.Millisecond

	programa := instalacaoEmProva(t)
	a, reinicios := atualizadorDeTeste(t, &trocaFalsa{})
	a.programa = programa
	a.prova = atualizacao.AbrirProva(programa, version.Version)

	c, corpo := painelDoHeartbeat(t, func() (int, string) { return 200, `{"ok":true}` })
	// Antes do heartbeat aceito, o relato é "em prova".
	if r := a.relatoDoHeartbeat(); r == nil || r.State != "proving" {
		t.Fatalf("relato antes = %+v", r)
	}
	enviarHeartbeatCom(context.Background(), c, a.cfg, service.Detected{}, &estadoDoToken{}, a)
	if relato, _ := corpo()["update"].(map[string]any); relato["state"] != "proving" {
		t.Fatalf("o heartbeat da prova levou update = %#v", corpo()["update"])
	}

	a.vigiarProva(context.Background())
	if reinicios.Load() != 0 {
		t.Fatal("o daemon foi encerrado com a prova confirmada")
	}
	if got := lerArquivo(t, programa); got != "novo" {
		t.Fatalf("no lugar ficou %q, queria o programa novo", got)
	}
	if !strings.Contains(lerArquivo(t, atualizacao.LugarDe(programa).Estado), `"situacao": "confirmada"`) {
		t.Fatalf("registro:\n%s", lerArquivo(t, atualizacao.LugarDe(programa).Estado))
	}
}

// Com o programa em prova, o agente não empilha outra troca por cima.
func TestEmProvaNaoAceitaOutraTroca(t *testing.T) {
	f := &trocaFalsa{}
	a, _ := atualizadorDeTeste(t, f)
	programa := instalacaoEmProva(t)
	a.programa = programa
	a.prova = atualizacao.AbrirProva(programa, version.Version)
	a.tentar(context.Background(), oferta)
	if f.chamadas.Load() != 0 {
		t.Fatal("uma troca rodou com o programa atual ainda em prova")
	}
}

// Com a troca do programa em curso, a rodada de restauração com itens não
// começa: fica para o programa que subir.
func TestRestauracaoNaoComecaDuranteATroca(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(api.ListRestoreItemsResponse{Items: []api.RestoreItem{
				{ItemID: "i1", Bucket: "b", Status: "queued", DestPath: t.TempDir(), DestFilename: "a.txt", SourceKey: "k", SourceVersionID: "v"},
			}})
			return
		}
		patches.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageBucket: "b"}

	oc := &ocupacao{trava: &exclusaoDoBucket{}}
	liberar, motivo := oc.reservar()
	if motivo != "" {
		t.Fatalf("não reservou: %s", motivo)
	}
	processarFilaDeRestoreCom(context.Background(), c, s3SemUso(t), cfg, map[string]*esperaDeAquecimento{}, oc)
	if n := patches.Load(); n != 0 {
		t.Fatalf("a restauração mexeu em %d item(ns) durante a troca do programa", n)
	}
	liberar()
	processarFilaDeRestoreCom(context.Background(), c, s3SemUso(t), cfg, map[string]*esperaDeAquecimento{}, oc)
	if patches.Load() == 0 {
		t.Fatal("depois da troca, a restauração não rodou")
	}
	// E, terminada a rodada, a troca pode reservar de novo.
	if _, motivo := oc.reservar(); motivo != "" {
		t.Fatalf("a restauração terminou e a troca segue barrada: %s", motivo)
	}
}

// O backup que esperava a trava presa pela troca não abre sessão no programa
// que está de saída.
func TestBackupNaoAbreSessaoComODaemonDeSaida(t *testing.T) {
	var pedidos atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pedidos.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := executarPlanoExclusivo(ctx, c, s3SemUso(t), &config.Config{AgentID: "a1"},
		api.Plan{ID: "p1", Kind: "backup"}, &exclusaoDoBucket{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, queria o cancelamento", err)
	}
	if pedidos.Load() != 0 {
		t.Fatal("o backup falou com o painel com o daemon de saída")
	}
}

// O /sessions/start leva as capacidades declaradas, como o heartbeat.
func TestSessionStartLevaAsCapacidades(t *testing.T) {
	var mu gosync.Mutex
	var start map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions/start") {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			_ = json.Unmarshal(b, &start)
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden) // o teste só quer o corpo do /start
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})

	antes := capacidadesDeclaradas.Load()
	t.Cleanup(func() {
		if antes != nil {
			capacidadesDeclaradas.Store(antes)
		} else {
			capacidadesDeclaradas.Store([]string(nil))
		}
	})
	for _, caso := range [][]string{{atualizacao.Capacidade}, {}} {
		capacidadesDeclaradas.Store(caso)
		_ = executePlan(context.Background(), c, s3SemUso(t), &config.Config{AgentID: "a1"}, api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{"/x"}})
		mu.Lock()
		caps, ok := start["capabilities"].([]any)
		mu.Unlock()
		if !ok || len(caps) != len(caso) {
			t.Fatalf("declaradas %v: capabilities no /start = %#v", caso, start["capabilities"])
		}
	}
}
