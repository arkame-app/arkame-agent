package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"

	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/pkg/version"
)

// painelFalso responde o heartbeat e o /update, e guarda o que recebeu.
type painelFalso struct {
	mu           gosync.Mutex
	heartbeats   []map[string]any
	consultas    []string
	statusDoHB   int
	respDoUpdate string
}

func (p *painelFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		var corpo map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &corpo)
		p.heartbeats = append(p.heartbeats, corpo)
		if p.statusDoHB != 0 {
			w.WriteHeader(p.statusDoHB)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	case strings.HasSuffix(r.URL.Path, "/update"):
		p.consultas = append(p.consultas, r.URL.RawQuery)
		_, _ = io.WriteString(w, p.respDoUpdate)
	default:
		// O "bucket": responde erro a qualquer consulta S3.
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// configDeTeste grava um env-file completo apontando para o painel falso.
func configDeTeste(t *testing.T, painel string, linhas ...string) string {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token.jwt")
	if err := os.WriteFile(token, []byte("a.b.c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conteudo := append([]string{
		"AGENT_ID=a1",
		"PANEL_URL=" + painel,
		"TOKEN_PATH=" + token,
		"STORAGE_ACCESS_KEY=ak",
		"STORAGE_SECRET_KEY=sk",
	}, linhas...)
	arquivo := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(arquivo, []byte(strings.Join(conteudo, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return arquivo
}

// O self-test passa com a configuração boa, diz a versão na primeira linha e
// marca o heartbeat como de teste (o painel não o registra).
func TestSelfTestPassaEMarcaOHeartbeat(t *testing.T) {
	p := &painelFalso{}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	var saida bytes.Buffer
	if err := autoTeste(context.Background(), &saida, configDeTeste(t, srv.URL), false); err != nil {
		t.Fatalf("o self-test falhou: %v\n%s", err, saida.String())
	}
	primeira, _, _ := strings.Cut(saida.String(), "\n")
	if primeira != atualizacao.LinhaDaVersao+version.Version {
		t.Fatalf("primeira linha = %q", primeira)
	}
	if len(p.heartbeats) != 1 || p.heartbeats[0]["self_test"] != true {
		t.Fatalf("heartbeats = %v", p.heartbeats)
	}
}

// O self-test reprova — com erro, e o comando sai diferente de zero — quando
// o painel recusa o heartbeat, quando falta o token ou a identidade.
func TestSelfTestReprova(t *testing.T) {
	p := &painelFalso{statusDoHB: http.StatusUnauthorized}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	if err := autoTeste(context.Background(), io.Discard, configDeTeste(t, srv.URL), false); err == nil {
		t.Fatal("o painel recusou o heartbeat e o self-test passou")
	}

	bom := &painelFalso{}
	srvBom := httptest.NewServer(bom)
	t.Cleanup(srvBom.Close)
	semToken := configDeTeste(t, srvBom.URL, "TOKEN_PATH="+filepath.Join(t.TempDir(), "nao-existe"))
	if err := autoTeste(context.Background(), io.Discard, semToken, false); err == nil {
		t.Fatal("sem token e o self-test passou")
	}
	semID := configDeTeste(t, srvBom.URL, "AGENT_ID=", "AGENT_ID_PATH="+filepath.Join(t.TempDir(), "nao-existe"))
	if err := autoTeste(context.Background(), io.Discard, semID, false); err == nil {
		t.Fatal("sem identidade e o self-test passou")
	}
	if len(bom.heartbeats) != 0 {
		t.Fatalf("o self-test sem token ou identidade falou com o painel: %v", bom.heartbeats)
	}

	// O comando de verdade sai com erro (e o processo, com código ≠ 0).
	cmd := newRootCmd()
	cmd.SetArgs([]string{"self-test", "--config", configDeTeste(t, srv.URL)})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("o comando self-test devolveu sucesso com o painel recusando")
	}
}

// O bucket que não responde só reprova quando exigido.
func TestSelfTestBucketSoReprovaQuandoExigido(t *testing.T) {
	p := &painelFalso{}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	cfg := configDeTeste(t, srv.URL, "STORAGE_BUCKET=b", "STORAGE_ENDPOINT="+srv.URL)
	if err := autoTeste(context.Background(), io.Discard, cfg, false); err != nil {
		t.Fatalf("bucket fora do ar, não exigido, e o self-test falhou: %v", err)
	}
	if err := autoTeste(context.Background(), io.Discard, cfg, true); err == nil {
		t.Fatal("bucket fora do ar, exigido, e o self-test passou")
	}
}

// trocaAnotada troca o aplicarAtualizacao e os serviços pelos do teste.
type trocaAnotada struct {
	pedidos     []atualizacao.Pedido
	opcoes      []atualizacao.Opcoes
	reiniciados []string
	err         error
}

func anotarTroca(t *testing.T) *trocaAnotada {
	t.Helper()
	a := &trocaAnotada{}
	antesA, antesR, antesS := aplicarAtualizacao, reiniciarServico, servicosDoPrograma
	t.Cleanup(func() { aplicarAtualizacao, reiniciarServico, servicosDoPrograma = antesA, antesR, antesS })
	aplicarAtualizacao = func(_ context.Context, p atualizacao.Pedido, o atualizacao.Opcoes) (*atualizacao.Resultado, error) {
		a.pedidos = append(a.pedidos, p)
		a.opcoes = append(a.opcoes, o)
		if a.err != nil {
			return nil, a.err
		}
		return &atualizacao.Resultado{Versao: p.Versao}, nil
	}
	servicosDoPrograma = func(string) []string { return []string{"arkame-agent", "arkame-agent-oci"} }
	reiniciarServico = func(nome string) error { a.reiniciados = append(a.reiniciados, nome); return nil }
	return a
}

// O `update` pergunta ao painel a versão e o SHA-256, passa os dois à troca
// (o segundo canal) e reinicia os serviços que usam o programa.
func TestUpdateConsultaOPainelEReinicia(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	p := &painelFalso{respDoUpdate: `{"update":{"version":"0.5.0","sha256":"` + sha + `","base_url":"https://get.arkame.app/releases","file":"arkame-agent_linux_amd64"}}`}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	a := anotarTroca(t)
	cfg, err := config.Load(configDeTeste(t, srv.URL), config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	var saida bytes.Buffer
	if err := atualizarAgora(context.Background(), &saida, cfg, pedidoManual{}); err != nil {
		t.Fatalf("o update falhou: %v\n%s", err, saida.String())
	}
	if len(a.pedidos) != 1 {
		t.Fatalf("a troca rodou %d vez(es)", len(a.pedidos))
	}
	ped := a.pedidos[0]
	if ped.Versao != "0.5.0" || ped.Sha256 != sha || ped.BaseURL != "https://get.arkame.app/releases" || ped.Arquivo != "arkame-agent_linux_amd64" {
		t.Fatalf("pedido = %+v", ped)
	}
	if !ped.Forcar || ped.PermitirReversao {
		t.Fatalf("pedido manual: forçar=%v reversão=%v", ped.Forcar, ped.PermitirReversao)
	}
	if o := a.opcoes[0]; o.Reserva != atualizacao.ReleasesNoGitHub || len(o.ArgsDoAutoTeste) < 3 || o.ArgsDoAutoTeste[0] != "self-test" || o.PrepararNovo == nil {
		t.Fatalf("opções = %+v", o)
	}
	if strings.Join(a.reiniciados, ",") != "arkame-agent,arkame-agent-oci" {
		t.Fatalf("reiniciados = %v", a.reiniciados)
	}
	if len(p.consultas) != 1 || p.consultas[0] != "" {
		t.Fatalf("consultas ao painel = %q", p.consultas)
	}

	// Com --version, a consulta leva a versão; com --no-restart, ninguém reinicia.
	a.reiniciados = nil
	if err := atualizarAgora(context.Background(), io.Discard, cfg, pedidoManual{versao: "0.5.0", semReiniciar: true, permitirReversao: true}); err != nil {
		t.Fatal(err)
	}
	if p.consultas[1] != "version=0.5.0" || len(a.reiniciados) != 0 || !a.pedidos[1].PermitirReversao {
		t.Fatalf("consulta=%q reiniciados=%v pedido=%+v", p.consultas[1], a.reiniciados, a.pedidos[1])
	}
}

// Sem versão mais nova publicada, nada acontece. Com a troca falhando, o
// comando falha e diz que o programa atual continua.
func TestUpdateSemNovidadeEComFalha(t *testing.T) {
	p := &painelFalso{respDoUpdate: `{"update":null}`}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	a := anotarTroca(t)
	cfg, _ := config.Load(configDeTeste(t, srv.URL), config.Overrides{})
	var saida bytes.Buffer
	if err := atualizarAgora(context.Background(), &saida, cfg, pedidoManual{}); err != nil {
		t.Fatal(err)
	}
	if len(a.pedidos) != 0 || !strings.Contains(saida.String(), "Nada a atualizar") {
		t.Fatalf("pedidos=%d saída=%q", len(a.pedidos), saida.String())
	}

	// --sha256 dispensa o painel, mas exige a versão.
	if err := atualizarAgora(context.Background(), io.Discard, cfg, pedidoManual{sha256: strings.Repeat("ab", 32)}); err == nil {
		t.Fatal("--sha256 sem --version foi aceito")
	}
	consultasAntes := len(p.consultas)
	a.err = &atualizacao.Falha{Codigo: atualizacao.FalhaAssinatura, Err: errors.New("assinatura")}
	err := atualizarAgora(context.Background(), io.Discard, cfg, pedidoManual{versao: "0.5.0", sha256: strings.Repeat("ab", 32)})
	if err == nil || !strings.Contains(err.Error(), "o programa atual continua") {
		t.Fatalf("erro = %v", err)
	}
	if len(p.consultas) != consultasAntes {
		t.Fatal("com --version e --sha256, o painel foi consultado")
	}
	if len(a.reiniciados) != 0 {
		t.Fatalf("a troca falhou e serviços foram reiniciados: %v", a.reiniciados)
	}
}
