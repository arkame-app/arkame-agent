package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// A instalação sem root (--service-scope user) e sem --config usava
// /etc/arkame/agent.env e parava em "permission denied" (ou "sem permissão
// para gravar em /etc/arkame"). Agora o padrão no escopo user é
// $XDG_CONFIG_HOME/arkame/agent.env, com token, chave e agent.id ao lado.
func TestInstallSemConfigNoEscopoUserUsaAPastaDoUsuario(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows o serviço é sempre do sistema")
	}
	limparAmbienteDaIdentidade(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
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

	var instalado *config.Config
	antes := instalarServico
	instalarServico = func(_ context.Context, cfg *config.Config, o service.Options) (*service.Installed, error) {
		instalado = cfg
		return &service.Installed{Name: o.Name, Scope: service.ScopeUser}, nil
	}
	t.Cleanup(func() { instalarServico = antes })

	cmd := newInstallCmd()
	cmd.SetArgs([]string{"--token", "atk_x", "--panel-url", srv.URL, "--check-storage=false", "--service-scope", "user"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	pasta := filepath.Join(xdg, "arkame")
	if instalado == nil || instalado.ConfigPath != filepath.Join(pasta, "agent.env") {
		t.Fatalf("serviço com a configuração %v, queria %s", instalado, filepath.Join(pasta, "agent.env"))
	}
	for _, p := range []string{instalado.TokenPath, instalado.PrivateKeyPath, instalado.AgentIDPath} {
		if filepath.Dir(p) != pasta {
			t.Fatalf("identidade fora da pasta do usuário: %s", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("identidade não gravada: %v", err)
		}
	}
	if st, err := os.Stat(pasta); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("pasta da configuração: %v %v", st, err)
	}
}

func TestConfigPadraoPorEscopo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows o serviço é sempre do sistema")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, quer := configPadrao("user"), filepath.Join(home, ".config", "arkame", "agent.env"); got != quer {
		t.Fatalf("user sem XDG_CONFIG_HOME: %s, queria %s", got, quer)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	if got, quer := configPadrao("user"), filepath.Join(home, "xdg", "arkame", "agent.env"); got != quer {
		t.Fatalf("user com XDG_CONFIG_HOME: %s, queria %s", got, quer)
	}
	// XDG_CONFIG_HOME relativo não vale (a especificação manda ignorar).
	t.Setenv("XDG_CONFIG_HOME", "rel")
	if got := configPadrao("user"); !strings.HasPrefix(got, home) {
		t.Fatalf("XDG_CONFIG_HOME relativo: %s", got)
	}
	if got := configPadrao("system"); got != config.DefaultPath {
		t.Fatalf("system: %s", got)
	}

	// Sem registro do serviço, os comandos (status, uninstall…) no escopo
	// user caem no mesmo arquivo da instalação.
	antes := configDoServico
	configDoServico = func(string, service.Scope) (string, bool) { return "", false }
	t.Cleanup(func() { configDoServico = antes })
	c := newStatusCmd()
	if got, err := configDoAgente(c, config.DefaultPath, service.DefaultName, "user"); err != nil || got != configPadrao("user") {
		t.Fatalf("status no escopo user: %q, %v", got, err)
	}
}
