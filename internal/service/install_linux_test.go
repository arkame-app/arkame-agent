//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		// systemd < 231 (CentOS 7) só conhece ReadWriteDirectories=.
		if escopo == ScopeSystem && linhas["ReadWriteDirectories"] != `"/srv/arkame cfg"` {
			t.Errorf("ReadWriteDirectories=%s", linhas["ReadWriteDirectories"])
		}
		if escopo == ScopeUser && strings.Contains(unit, "ReadWrite") {
			t.Errorf("unit de usuário com ReadWrite*:\n%s", unit)
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

// A unit é trocada inteira (temporário, fsync, rename), não truncada no lugar:
// uma unit antiga com outro modo sai 0644 e o temporário não fica para trás.
func TestInstalarTrocaAUnitInteira(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("o serviço do usuário não instala como root")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	antesExec, antesProc := executar, procurar
	t.Cleanup(func() { executar, procurar = antesExec, antesProc })
	procurar = func(nome string) (string, error) { return "/usr/bin/" + nome, nil }
	executar = func(_ context.Context, nome string, _ ...string) ([]byte, error) {
		if nome == "loginctl" {
			return []byte("Linger=yes\n"), nil
		}
		return nil, nil
	}
	unit := filepath.Join(xdg, "systemd", "user", "arkame-agent.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("velha"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigPath: filepath.Join(t.TempDir(), "agent.env")}
	if _, err := Install(context.Background(), cfg, Options{
		Name: "arkame-agent", Scope: ScopeUser, BinaryPath: "/usr/local/bin/arkame-agent",
	}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("unit com modo %v, queria 0644", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(unit); !strings.Contains(string(b), "ExecStart=") {
		t.Fatalf("unit sem ExecStart: %q", b)
	}
	if _, err := os.Stat(unit + ".novo"); !os.IsNotExist(err) {
		t.Fatal("o temporário ficou para trás")
	}
}

// Sem TimeoutStopSec= valia o padrão de 90s do systemd, e o SIGKILL chegava
// no meio da finalização do daemon. O prazo é EsperaParada, nas duas units, e
// a leitura de volta da unit continua achando o config e o programa.
func TestUnitDaAoProcessoOPrazoDeParada(t *testing.T) {
	if time.Duration(segundosDeParada())*time.Second != EsperaParada {
		t.Fatalf("segundosDeParada() = %d não é EsperaParada (%s)", segundosDeParada(), EsperaParada)
	}
	querido := fmt.Sprintf("TimeoutStopSec=%d", int(EsperaParada.Seconds()))
	for _, escopo := range []Scope{ScopeSystem, ScopeUser} {
		unit := textoDaUnit(escopo, "arkame-agent", "/usr/local/bin/arkame-agent", "/etc/arkame/a.env", []string{"/etc/arkame"})
		var achadas []string
		for _, l := range strings.Split(unit, "\n") {
			if strings.HasPrefix(l, "TimeoutStopSec=") {
				achadas = append(achadas, l)
			}
		}
		if len(achadas) != 1 || achadas[0] != querido {
			t.Errorf("%s: TimeoutStopSec %q, queria só %q\n%s", escopo, achadas, querido, unit)
		}
		if _, depois, _ := strings.Cut(unit, "[Service]"); !strings.Contains(strings.SplitN(depois, "[Install]", 2)[0], querido) {
			t.Errorf("%s: TimeoutStopSec fora da seção [Service]", escopo)
		}
		if strings.Contains(unit, "%!") {
			t.Errorf("%s: verbo do Sprintf sobrando na unit:\n%s", escopo, unit)
		}
		if c := configDaUnit(unit); c != "/etc/arkame/a.env" {
			t.Errorf("%s: configDaUnit = %q", escopo, c)
		}
		if p := programaDaUnit(unit); p != "/usr/local/bin/arkame-agent" {
			t.Errorf("%s: programaDaUnit = %q", escopo, p)
		}
	}
}
