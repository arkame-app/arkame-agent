//go:build linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// Reinstalar (ou atualizar) com a unit já ativa: `enable --now` não mexia no
// processo de pé, que seguia com o token e o AGENT_ID antigos na memória e
// tomava 401 depois da aprovação. A instalação tem de reiniciar a unit.
func TestInstalarReiniciaAUnitAtiva(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("o serviço do usuário não instala como root")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var chamadas []string
	antesExec, antesProc := executar, procurar
	t.Cleanup(func() { executar, procurar = antesExec, antesProc })
	procurar = func(nome string) (string, error) { return "/usr/bin/" + nome, nil }
	executar = func(_ context.Context, nome string, args ...string) ([]byte, error) {
		chamadas = append(chamadas, nome+" "+strings.Join(args, " "))
		if nome == "loginctl" {
			return []byte("Linger=yes\n"), nil
		}
		return nil, nil
	}

	cfg := &config.Config{ConfigPath: filepath.Join(t.TempDir(), "agent.env")}
	if _, err := Install(context.Background(), cfg, Options{
		Name: "arkame-agent-aws", Scope: ScopeUser, BinaryPath: "/usr/local/bin/arkame-agent", Start: true,
	}); err != nil {
		t.Fatal(err)
	}

	var systemctl []string
	for _, c := range chamadas {
		if strings.HasPrefix(c, "systemctl ") {
			systemctl = append(systemctl, c)
		}
	}
	querido := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable arkame-agent-aws",
		"systemctl --user restart arkame-agent-aws",
	}
	if strings.Join(systemctl, "\n") != strings.Join(querido, "\n") {
		t.Fatalf("comandos do systemctl:\n%s\nqueria:\n%s", strings.Join(systemctl, "\n"), strings.Join(querido, "\n"))
	}
}
