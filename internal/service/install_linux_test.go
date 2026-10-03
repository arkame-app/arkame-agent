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

// --config com espaço ia cru no ExecStart: o systemd partia o caminho em dois
// argumentos e o serviço subia com o arquivo errado. E % na unit é
// especificador do systemd (%h, %u): um % do caminho tem de ir como %%.
func TestUnitProtegeOConfigComEspacoEPorcento(t *testing.T) {
	const (
		programa = "/opt/meu agente/arkame-agent"
		cfg      = "/srv/arkame cfg/100%.env"
	)
	for _, escopo := range []Scope{ScopeSystem, ScopeUser} {
		unit := textoDaUnit(escopo, "arkame-agent", programa, cfg, writablePaths(&config.Config{TokenPath: "/srv/arkame cfg/token.jwt"}))
		linhas := map[string]string{}
		for _, l := range strings.Split(unit, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				linhas[k] = v
			}
		}
		if q := `"/opt/meu agente/arkame-agent" run --config "/srv/arkame cfg/100%%.env"`; linhas["ExecStart"] != q {
			t.Errorf("%s: ExecStart=%s\nqueria   %s", escopo, linhas["ExecStart"], q)
		}
		if q := "/srv/arkame cfg/100%%.env"; linhas["EnvironmentFile"] != q {
			t.Errorf("%s: EnvironmentFile=%s, queria %s", escopo, linhas["EnvironmentFile"], q)
		}
		if escopo == ScopeSystem && linhas["ReadWritePaths"] != `"/srv/arkame cfg"` {
			t.Errorf("ReadWritePaths=%s", linhas["ReadWritePaths"])
		}
		// O uninstall e o reconhecimento dos outros agentes leem a unit de volta.
		if c := configDaUnit(unit); c != cfg {
			t.Errorf("%s: configDaUnit = %q, queria %q", escopo, c, cfg)
		}
		if c := configDaUnit(strings.Replace(unit, "EnvironmentFile=", "#", 1)); c != cfg {
			t.Errorf("%s: configDaUnit pelo ExecStart = %q, queria %q", escopo, c, cfg)
		}
		arq := filepath.Join(t.TempDir(), "x.service")
		if err := os.WriteFile(arq, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
		if !unitChamaPrograma(arq, programa) {
			t.Errorf("%s: a unit não foi reconhecida como do programa %s", escopo, programa)
		}
	}

	// Com %, sem espaço: sem aspas, mas escapado.
	unit := textoDaUnit(ScopeUser, "arkame-agent", "/usr/local/bin/arkame-agent", "/etc/arkame/a%b.env", nil)
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/arkame-agent run --config /etc/arkame/a%%b.env\n") {
		t.Errorf("unit:\n%s", unit)
	}
}
