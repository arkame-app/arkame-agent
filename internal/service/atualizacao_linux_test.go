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

// linhaDaUnit devolve o valor de uma chave da unit.
func linhaDaUnit(unit, chave string) string {
	for _, l := range strings.Split(unit, "\n") {
		if v, ok := strings.CutPrefix(l, chave+"="); ok {
			return v
		}
	}
	return ""
}

// A unit de sistema libera a pasta do programa: sem ela, o ProtectSystem=full
// deixa /usr/local/bin só para leitura e a atualização automática não troca o
// programa. Nas duas linhas (o systemd antigo só lê ReadWriteDirectories=).
func TestUnitDeSistemaLiberaAPastaDoPrograma(t *testing.T) {
	cfg := &config.Config{TokenPath: "/etc/arkame/token.jwt", PrivateKeyPath: "/etc/arkame/key.pem", AgentIDPath: "/etc/arkame/agent.id"}
	for programa, quer := range map[string]string{
		"/usr/local/bin/arkame-agent":  "/etc/arkame /usr/local/bin",
		"/opt/arkame/bin/arkame-agent": "/etc/arkame /opt/arkame/bin",
		"/opt/meu agente/arkame-agent": `/etc/arkame "/opt/meu agente"`,
		// A pasta que já está na lista não entra duas vezes; a raiz, nunca.
		"/etc/arkame/arkame-agent": "/etc/arkame",
		"/arkame-agent":            "/etc/arkame",
	} {
		// A unit que o install e o `service refresh` gravam de verdade.
		cfg.ConfigPath = "/etc/arkame/agent.env"
		unit := unitDoServico(cfg, Options{Name: "arkame-agent", Scope: ScopeSystem, BinaryPath: programa})
		for _, chave := range []string{"ReadWritePaths", "ReadWriteDirectories"} {
			if got := linhaDaUnit(unit, chave); got != quer {
				t.Errorf("%s: %s=%s, queria %s", programa, chave, got, quer)
			}
		}
	}
}

// O programa registrado por um link: a troca acontece na pasta real, e é ela
// que tem de estar liberada (além da do link).
func TestUnitLiberaAPastaRealDoPrograma(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "arkame-agent"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "arkame-agent"), filepath.Join(link, "arkame-agent")); err != nil {
		t.Fatal(err)
	}
	realResolvida, _ := filepath.EvalSymlinks(real)
	got := pastasGravaveis(&config.Config{TokenPath: "/etc/arkame/token.jwt"}, filepath.Join(link, "arkame-agent"))
	if strings.Join(got, " ") != "/etc/arkame "+link+" "+realResolvida {
		t.Fatalf("pastas = %v", got)
	}
}

// `service refresh`: regrava a unit antiga (sem a pasta do programa) com o
// modelo novo, mantém o programa e o arquivo de configuração que ela tinha,
// recarrega o systemd — e não habilita nem reinicia nada.
func TestRegravarAtualizaAUnitSemReiniciar(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("o teste usa o serviço do usuário")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dirUnits := filepath.Join(xdg, "systemd", "user")
	if err := os.MkdirAll(dirUnits, 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dirUnits, "arkame-agent-oci.service")
	antiga := "[Service]\nExecStart=/opt/arkame/bin/arkame-agent run --config /srv/oci.env\nEnvironmentFile=/srv/oci.env\nRestart=always\n"
	if err := os.WriteFile(unitPath, []byte(antiga), 0o644); err != nil {
		t.Fatal(err)
	}

	var chamadas []string
	antes := executar
	t.Cleanup(func() { executar = antes })
	executar = func(_ context.Context, nome string, args ...string) ([]byte, error) {
		chamadas = append(chamadas, nome+" "+strings.Join(args, " "))
		return nil, nil
	}

	cfg := &config.Config{ConfigPath: "/srv/oci.env", TokenPath: "/srv/oci.token.jwt"}
	arquivo, err := Regravar(context.Background(), cfg, Options{Name: "arkame-agent-oci", Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	if arquivo != unitPath {
		t.Fatalf("arquivo = %q", arquivo)
	}
	nova, _ := os.ReadFile(unitPath)
	if p := programaDaUnit(string(nova)); p != "/opt/arkame/bin/arkame-agent" {
		t.Fatalf("o programa da unit mudou para %q", p)
	}
	if c := configDaUnit(string(nova)); c != "/srv/oci.env" {
		t.Fatalf("a configuração da unit mudou para %q", c)
	}
	if !strings.Contains(string(nova), "TimeoutStopSec=") {
		t.Fatalf("a unit não foi regravada com o modelo novo:\n%s", nova)
	}
	if strings.Join(chamadas, "|") != "systemctl --user daemon-reload" {
		t.Fatalf("comandos = %v, queria só o daemon-reload", chamadas)
	}

	// Serviço que não existe: erro, e nada é criado.
	if _, err := Regravar(context.Background(), cfg, Options{Name: "arkame-agent-nao-existe", Scope: ScopeUser}); err == nil {
		t.Fatal("regravou um serviço que não está instalado")
	}
	if _, err := os.Stat(filepath.Join(dirUnits, "arkame-agent-nao-existe.service")); err == nil {
		t.Fatal("o refresh criou uma unit nova")
	}
}

// A unit de sistema regravada ganha a pasta do programa nas linhas de escrita.
func TestRegravarUnitDeSistemaLiberaAPastaDoPrograma(t *testing.T) {
	if os.Geteuid() != 0 {
		// Sem root, o texto da unit é o mesmo: confere pelo modelo.
		unit := unitDoServico(&config.Config{ConfigPath: "/etc/arkame/agent.env", TokenPath: "/etc/arkame/token.jwt"},
			Options{Name: "arkame-agent", Scope: ScopeSystem, BinaryPath: "/usr/local/bin/arkame-agent"})
		// A unit do usuário não tem ProtectSystem nem lista de escrita.
		doUsuario := unitDoServico(&config.Config{ConfigPath: "/x/agent.env"}, Options{Name: "arkame-agent", Scope: ScopeUser, BinaryPath: "/x/arkame-agent"})
		if strings.Contains(doUsuario, "ReadWrite") {
			t.Fatalf("unit de usuário com ReadWrite*:\n%s", doUsuario)
		}
		if got := linhaDaUnit(unit, "ReadWritePaths"); got != "/etc/arkame /usr/local/bin" {
			t.Fatalf("ReadWritePaths=%s", got)
		}
		if _, err := Regravar(context.Background(), &config.Config{ConfigPath: "/etc/arkame/agent.env"},
			Options{Name: "arkame-agent", Scope: ScopeSystem, BinaryPath: "/usr/local/bin/arkame-agent"}); err == nil {
			t.Fatal("regravou a unit do sistema sem root")
		}
	}
}

// O programa novo da atualização fica com o dono e o modo do atual; o que
// outro usuário pode gravar é recusado quando o atual é do root.
func TestPrepararProgramaNovo(t *testing.T) {
	dir := t.TempDir()
	atual := filepath.Join(dir, "arkame-agent")
	novo := filepath.Join(dir, "arkame-agent.novo")
	if err := os.WriteFile(atual, []byte("a"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(novo, []byte("n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepararProgramaNovo(atual, novo); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(novo); fi.Mode().Perm() != 0o750 {
		t.Fatalf("modo do novo = %v, queria o do atual (0750)", fi.Mode().Perm())
	}
	// Sem programa atual (a primeira troca no volume do Docker): nada a fazer.
	if err := PrepararProgramaNovo(filepath.Join(dir, "nao-existe"), novo); err != nil {
		t.Fatalf("sem programa atual: %v", err)
	}

	// O atual é do root; o novo, gravável por todos: recusa.
	antesDono := lerDono
	t.Cleanup(func() { lerDono = antesDono })
	lerDono = func(p string) (donoDoCaminho, error) {
		if p == novo {
			return donoDoCaminho{uid: 0, gid: 0, modo: 0o757}, nil
		}
		return donoDoCaminho{uid: 0, gid: 0, modo: 0o755}, nil
	}
	if os.Geteuid() == 0 {
		t.Skip("como root, o chown de verdade muda o arquivo do teste")
	}
	err := PrepararProgramaNovo(atual, novo)
	if err == nil || !strings.Contains(err.Error(), "gravável por qualquer usuário") {
		t.Fatalf("programa novo gravável por todos aceito para o serviço root: %v", err)
	}
	// Do root e sem escrita alheia: passa.
	lerDono = func(string) (donoDoCaminho, error) { return donoDoCaminho{uid: 0, gid: 0, modo: 0o755}, nil }
	if err := PrepararProgramaNovo(atual, novo); err != nil {
		t.Fatalf("programa novo só do root recusado: %v", err)
	}
}
