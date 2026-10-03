package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// trocarInstalarServico registra as chamadas ao serviço do SO no lugar de
// instalar um de verdade.
func trocarInstalarServico(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	antes := instalarServico
	instalarServico = func(_ context.Context, cfg *config.Config, o service.Options) (*service.Installed, error) {
		n.Add(1)
		return &service.Installed{Name: o.Name, Scope: service.ScopeUser}, nil
	}
	t.Cleanup(func() { instalarServico = antes })
	return &n
}

func limparAmbienteDaIdentidade(t *testing.T) {
	t.Helper()
	for _, k := range []string{"TOKEN_PATH", "PRIVATE_KEY_PATH", "AGENT_ID_PATH", "AGENT_ID", "ENROLLMENT_TOKEN", "PANEL_URL"} {
		t.Setenv(k, "")
	}
}

// --wait=false deixava um enrollment que nada concluía e, com o serviço
// (padrão), instalava um daemon sem token, em laço de "não aprovado". Agora
// para antes de falar com o painel e sem instalar nada.
func TestInstallSemEsperaNaoRegistraNemInstalaServico(t *testing.T) {
	limparAmbienteDaIdentidade(t)
	var chamadas atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chamadas.Add(1)
		_, _ = io.WriteString(w, `{"agent_id":"novo","status":"pending","wait_url":"/w"}`)
	}))
	defer srv.Close()
	servico := trocarInstalarServico(t)

	arquivo := filepath.Join(t.TempDir(), "agent-x.env")
	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--config", arquivo, "--token", "atk_x", "--panel-url", srv.URL,
		"--check-storage=false", "--wait=false"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--wait=false não é aceito") {
		t.Fatalf("esperava a recusa do --wait=false, veio %v", err)
	}
	if n := chamadas.Load(); n != 0 {
		t.Fatalf("o painel recebeu %d chamadas: o enrollment ficaria pendurado", n)
	}
	if n := servico.Load(); n != 0 {
		t.Fatalf("o serviço foi instalado %d vez(es) sem token", n)
	}
}

// Caminho normal: aprovado, o token está no disco quando o serviço é
// instalado.
func TestInstallAprovadoInstalaServicoComToken(t *testing.T) {
	limparAmbienteDaIdentidade(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents/enroll":
			_, _ = io.WriteString(w, `{"agent_id":"novo","status":"pending","wait_url":"/w"}`)
		case "/w":
			_, _ = io.WriteString(w, `{"agent_token":"a.b.c","expires_at":"2027-10-03T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	arquivo := filepath.Join(dir, "agent-x.env")
	token := filepath.Join(dir, "agent-x.token.jwt")
	var tinhaToken bool
	antes := instalarServico
	instalarServico = func(_ context.Context, cfg *config.Config, o service.Options) (*service.Installed, error) {
		_, err := os.Stat(token)
		tinhaToken = err == nil && cfg.TokenPath == token
		return &service.Installed{Name: o.Name, Scope: service.ScopeUser}, nil
	}
	t.Cleanup(func() { instalarServico = antes })

	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--config", arquivo, "--token", "atk_x", "--panel-url", srv.URL, "--check-storage=false"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !tinhaToken {
		t.Fatal("o serviço foi instalado sem o token no disco")
	}
}

// Serviço com nome fora do prefixo arkame-agent não é reconhecido pelo
// uninstall dos outros agentes, que apagaria o programa dele. A recusa vem
// antes de falar com o painel: depois da aprovação, sobraria um servidor
// aprovado sem serviço.
func TestInstallRecusaNomeSemPrefixoAntesDoRegistro(t *testing.T) {
	limparAmbienteDaIdentidade(t)
	var chamadas atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chamadas.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	servico := trocarInstalarServico(t)

	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "agent-x.env"), "--token", "atk_x",
		"--panel-url", srv.URL, "--check-storage=false", "--service-name", "backup-oci"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "arkame-agent-backup-oci") {
		t.Fatalf("esperava a recusa do nome com sugestão, veio %v", err)
	}
	if chamadas.Load() != 0 || servico.Load() != 0 {
		t.Fatalf("painel=%d serviço=%d: nada deveria ter sido feito", chamadas.Load(), servico.Load())
	}
}

// No macOS, sem Acesso Total ao Disco, o serviço não lê Mesa, Documentos,
// Downloads nem iCloud Drive, e nada dizia o que fazer: o install diz o passo,
// com o programa que o serviço chama.
func TestInstallDizOPassoDoAcessoTotalNoMacOS(t *testing.T) {
	inst := &service.Installed{Scope: service.ScopeSystem, Programa: "/usr/local/bin/arkame-agent", StartCmd: "sudo launchctl kickstart -k system/app.arkame.agent"}
	p := passoDoAcessoTotal("darwin", inst)
	for _, quer := range []string{"Acesso Total ao Disco", "Privacidade e Segurança", "/usr/local/bin/arkame-agent", inst.StartCmd} {
		if !strings.Contains(p, quer) {
			t.Fatalf("passo sem %q:\n%s", quer, p)
		}
	}
	if p := passoDoAcessoTotal("darwin", &service.Installed{Scope: service.ScopeUser}); !strings.Contains(p, "~/.local/bin/arkame-agent") {
		t.Fatalf("escopo do usuário sem o programa de ~/.local/bin:\n%s", p)
	}
	if p := passoDoAcessoTotal("linux", inst); p != "" {
		t.Fatalf("fora do macOS não há passo: %q", p)
	}
}

// Reinstalação com a chave no arquivo e um código de outro armazenamento: o
// install gravava o armazenamento novo (e os caminhos da identidade) antes da
// aprovação. Ctrl-C na espera deixava o arquivo com o bucket, a região e o
// endereço novos e o AGENT_ID e o token antigos — o daemon antigo, no
// próximo reinício, mirava o bucket novo com os planos do velho. Agora o
// arquivo só muda com a aprovação.
func TestInstallCanceladoNaEsperaNaoMexeNoArquivo(t *testing.T) {
	limparAmbienteDaIdentidade(t)
	isolarAWS(t)
	semTerminal(t)
	servico := trocarInstalarServico(t)
	s3 := s3Falso(t, map[string][]string{"velho": {"ak"}, "novo": {"ak"}})
	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()
	var esperou atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents/install-config":
			_, _ = io.WriteString(w, `{"agent_id":"ag-novo","display_name":"Srv","storage":{"id":"st-novo","display_name":"Novo","bucket":"novo","region":"sa-east-1","endpoint":"`+s3+`"}}`)
		case "/api/agents/enroll":
			_, _ = io.WriteString(w, `{"agent_id":"ag-novo","status":"pending","wait_url":"/w"}`)
		case "/w":
			// A pessoa desiste enquanto espera a aprovação (Ctrl-C).
			esperou.Store(true)
			cancelar()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	arquivo := filepath.Join(t.TempDir(), "agent-x.env")
	conteudo := "AGENT_ID=ag-velho\nSTORAGE_ID=st-velho\nSTORAGE_BUCKET=velho\nSTORAGE_REGION=us-west-2\nSTORAGE_ENDPOINT=" + s3 +
		"\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\n"
	if err := os.WriteFile(arquivo, []byte(conteudo), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--config", arquivo, "--token", "atk_x", "--panel-url", srv.URL})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(ctx); err == nil {
		t.Fatal("o install cancelado na espera terminou sem erro")
	}
	if !esperou.Load() {
		t.Fatal("o install não chegou à espera da aprovação")
	}
	if b, _ := os.ReadFile(arquivo); string(b) != conteudo {
		t.Fatalf("o arquivo mudou sem aprovação:\n%s", b)
	}
	if servico.Load() != 0 {
		t.Fatal("o serviço foi instalado sem aprovação")
	}
}

// Aprovado, o armazenamento novo, os caminhos da identidade e o AGENT_ID da
// aprovação vão ao arquivo de uma vez.
func TestInstallAprovadoGravaOArmazenamentoNovo(t *testing.T) {
	limparAmbienteDaIdentidade(t)
	isolarAWS(t)
	semTerminal(t)
	trocarInstalarServico(t)
	s3 := s3Falso(t, map[string][]string{"velho": {"ak"}, "novo": {"ak"}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents/install-config":
			_, _ = io.WriteString(w, `{"agent_id":"ag-novo","display_name":"Srv","storage":{"id":"st-novo","display_name":"Novo","bucket":"novo","region":"sa-east-1","endpoint":"`+s3+`"}}`)
		case "/api/agents/enroll":
			_, _ = io.WriteString(w, `{"agent_id":"ag-novo","status":"pending","wait_url":"/w"}`)
		case "/w":
			_, _ = io.WriteString(w, `{"agent_token":"a.b.c","expires_at":"2027-10-03T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	arquivo := filepath.Join(dir, "agent-x.env")
	if err := os.WriteFile(arquivo, []byte("AGENT_ID=ag-velho\nSTORAGE_ID=st-velho\nSTORAGE_BUCKET=velho\nSTORAGE_REGION=us-west-2\nSTORAGE_ENDPOINT="+s3+
		"\nSTORAGE_ACCESS_KEY=ak\nSTORAGE_SECRET_KEY=sk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--config", arquivo, "--token", "atk_x", "--panel-url", srv.URL})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(arquivo, config.Overrides{})
	if cfg.AgentID != "ag-novo" || cfg.StorageID != "st-novo" || cfg.StorageBucket != "novo" || cfg.StorageRegion != "sa-east-1" ||
		cfg.StorageAccessKey != "ak" || cfg.TokenPath != filepath.Join(dir, "agent-x.token.jwt") {
		t.Fatalf("arquivo depois da aprovação: %+v", cfg)
	}
	if _, err := os.Stat(cfg.TokenPath); err != nil {
		t.Fatalf("token: %v", err)
	}
}
