package service

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// O programa do ExecStart, com prefixos do systemd, aspas e %%, e o
// `ExecStart=` vazio que só zera a lista.
func TestProgramaDaUnit(t *testing.T) {
	casos := map[string]string{
		"[Service]\nExecStart=/usr/local/bin/arkame-agent run --config /etc/arkame/agent.env\n": "/usr/local/bin/arkame-agent",
		"ExecStart=-/opt/arkame/bin/arkame-agent run\n":                                         "/opt/arkame/bin/arkame-agent",
		`ExecStart="/srv/meu bin/arkame-agent" run --config "/a b/x.env"`:                       "/srv/meu bin/arkame-agent",
		"ExecStart=/srv/100%%/arkame-agent run\n":                                               "/srv/100%/arkame-agent",
		"ExecStart=\nExecStart=/usr/local/bin/arkame-agent run\n":                               "/usr/local/bin/arkame-agent",
		"[Service]\nType=simple\n":                                                              "",
	}
	for unit, quer := range casos {
		if got := programaDaUnit(unit); got != quer {
			t.Errorf("programaDaUnit(%q) = %q, queria %q", unit, got, quer)
		}
	}
}

// O programa do ProgramArguments do plist que o install gera.
func TestProgramaDoPlistGerado(t *testing.T) {
	p := montarPlist("app.arkame.agent-oci", "/opt/arkame & cia/bin/arkame-agent", "/etc/arkame/oci.env", "/var/log/x.log", ScopeSystem)
	if got := programaDoPlist(p); got != "/opt/arkame & cia/bin/arkame-agent" {
		t.Fatalf("programaDoPlist = %q", got)
	}
}

func TestLaunchdRodando(t *testing.T) {
	rodando := "system/app.arkame.agent = {\n\tactive count = 1\n\tstate = running\n\tpid = 123\n}\n"
	parado := "system/app.arkame.agent = {\n\tactive count = 0\n\tstate = not running\n}\n"
	if !launchdRodando(rodando) || launchdRodando(parado) || launchdRodando("") {
		t.Fatal("state = running é o único que conta como rodando")
	}
}

// Só entram os serviços que chamam o programa trocado e estão rodando agora:
// os outros não ficaram no programa antigo. Sem repetir.
func TestDoProgramaFiltraProgramaERodando(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "arkame-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "atalho")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	outro := filepath.Join(dir, "outro-arkame-agent")
	todos := []servicoRegistrado{
		{"arkame-agent", ScopeSystem, bin},
		{"arkame-agent-oci", ScopeSystem, bin},
		{"arkame-agent-oci", ScopeSystem, bin}, // repetido
		{"arkame-agent-parado", ScopeSystem, bin},
		{"arkame-agent-outro", ScopeSystem, outro},
		{"backup-legado", ScopeUser, link},
		{"nginx", ScopeSystem, "/usr/sbin/nginx"},
		{"sem-programa", ScopeSystem, ""},
	}
	var consultados []string
	rodando := func(s servicoRegistrado) bool {
		consultados = append(consultados, s.nome)
		return s.nome != "arkame-agent-parado"
	}
	got := doPrograma("linux", todos, bin, rodando)
	quer := []string{"arkame-agent", "arkame-agent-oci", "backup-legado"}
	if !slices.Equal(got, quer) {
		t.Fatalf("doPrograma = %v, queria %v", got, quer)
	}
	for _, n := range consultados {
		if n == "nginx" || n == "arkame-agent-outro" || n == "sem-programa" {
			t.Fatalf("consultou se roda um serviço de outro programa: %s", n)
		}
	}
	if got := doPrograma("linux", todos, "", rodando); got != nil {
		t.Fatalf("sem programa: %v", got)
	}
}

// O reinício segue o escopo do registro; com o nome nos dois escopos, o que
// roda. No macOS o nome e o label são o mesmo serviço.
func TestEscolherRegistro(t *testing.T) {
	todos := []servicoRegistrado{
		{"arkame-agent", ScopeSystem, "/x"},
		{"arkame-agent-oci", ScopeUser, "/x"},
		{"arkame-agent-oci", ScopeSystem, "/x"},
	}
	rodaSistema := func(s servicoRegistrado) bool { return s.escopo == ScopeSystem }
	if s, ok := escolherRegistro("linux", todos, "arkame-agent-oci", rodaSistema); !ok || s.escopo != ScopeSystem {
		t.Fatalf("nos dois escopos, vale o que roda: %+v %v", s, ok)
	}
	if s, ok := escolherRegistro("darwin", todos, "app.arkame.agent", rodaSistema); !ok || s.nome != "arkame-agent" {
		t.Fatalf("label do launchd: %+v %v", s, ok)
	}
	if _, ok := escolherRegistro("linux", todos, "nao-existe", rodaSistema); ok {
		t.Fatal("serviço que não existe")
	}
}
