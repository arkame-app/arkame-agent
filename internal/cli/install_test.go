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
