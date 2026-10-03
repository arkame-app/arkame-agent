package service

import (
	"strings"
	"testing"
)

// O uninstall precisa saber que arquivos os outros agentes usam; o caminho
// vem do registro do serviço de cada um.
func TestConfigDoRegistroDoServico(t *testing.T) {
	unit := "[Service]\nType=simple\nExecStart=\"/usr/local/bin/arkame-agent\" run --config /etc/arkame/agent-oci.env\nEnvironmentFile=/etc/arkame/agent-oci.env\n"
	if c := configDaUnit(unit); c != "/etc/arkame/agent-oci.env" {
		t.Errorf("unit: %q", c)
	}
	if c := configDaUnit("[Service]\nExecStart=/bin/arkame-agent run --config /x/a.env\n"); c != "/x/a.env" {
		t.Errorf("unit sem EnvironmentFile: %q", c)
	}
	if c := configDaUnit("[Service]\nExecStart=/bin/arkame-agent run\n"); c != "" {
		t.Errorf("unit sem config: %q", c)
	}
	plist := `<array><string>/usr/local/bin/arkame-agent</string><string>run</string><string>--config</string><string>/Users/a b/&amp;.env</string></array><key>StandardOutPath</key><string>/tmp/x.log</string>`
	if c := configDoPlist(plist); c != "/Users/a b/&.env" {
		t.Errorf("plist: %q", c)
	}
	if c := configDosArgs([]string{`C:\Program Files\Arkame\arkame-agent.exe`, "run", "--config", `C:\etc\arkame\oci.env`}); c != `C:\etc\arkame\oci.env` {
		t.Errorf("SCM: %q", c)
	}
	if c := configDosArgs([]string{"x", "--config=/a.env"}); c != "/a.env" {
		t.Errorf("--config=: %q", c)
	}
}

// No macOS o painel devolve o label em --service-name: o agente se contava
// como "outro agente", e o uninstall deixava agent.env (com a chave do
// bucket), o token, a chave privada e o agent.id no disco.
func TestOutrosAgentesReconheceOProprioPeloLabel(t *testing.T) {
	todos := []string{"arkame-agent", "arkame-agent-aws"}
	for _, nome := range []string{"app.arkame.agent-aws", "arkame-agent-aws"} {
		if o := outrosEntre("darwin", todos, nome); len(o) != 1 || o[0] != "arkame-agent" {
			t.Errorf("darwin, %s: outros = %v; só arkame-agent é outro", nome, o)
		}
	}
	if o := outrosEntre("darwin", todos, "app.arkame.agent"); len(o) != 1 || o[0] != "arkame-agent-aws" {
		t.Errorf("darwin, label do padrão: outros = %v", o)
	}
	if o := outrosEntre("windows", []string{"Arkame-Agent-AWS", "arkame-agent"}, "arkame-agent-aws"); len(o) != 1 || o[0] != "arkame-agent" {
		t.Errorf("windows: outros = %v", o)
	}
	if o := outrosEntre("linux", todos, ""); len(o) != 1 || o[0] != "arkame-agent-aws" {
		t.Errorf("linux, nome vazio: outros = %v", o)
	}
}

// O uninstall reconhece os outros agentes pelo prefixo arkame-agent; um
// serviço com outro nome perderia o programa. O install exige o prefixo.
func TestValidarNomeExigePrefixo(t *testing.T) {
	for _, n := range []string{"arkame-agent", "arkame-agent-aws", "arkame-agent.oci"} {
		if err := ValidarNome(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"backup-oci", "arkame-oci", "agent-arkame-agent", "Arkame-Agent", ""} {
		if err := ValidarNome(n); err == nil {
			t.Errorf("%q deveria ser recusado", n)
		}
	}
	if err := ValidarNome("backup-oci"); err == nil || !strings.Contains(err.Error(), "arkame-agent-backup-oci") {
		t.Errorf("a recusa deveria sugerir o nome: %v", err)
	}
}

// A linha de comando registrada (ExecStart, BinaryPathName) diz qual programa
// o serviço chama.
func TestProgramaDaLinha(t *testing.T) {
	casos := map[string]string{
		`"C:\Program Files\Arkame\arkame-agent.exe" run --config C:\etc\arkame\a.env`: `C:\Program Files\Arkame\arkame-agent.exe`,
		`/usr/local/bin/arkame-agent run --config /etc/arkame/agent.env`:              "/usr/local/bin/arkame-agent",
		`  /bin/x`:  "/bin/x",
		`"/sem/fim`: "/sem/fim",
	}
	for linha, quer := range casos {
		if p := programaDaLinha(linha); p != quer {
			t.Errorf("%q: %q, queria %q", linha, p, quer)
		}
	}
	if !mesmoPrograma("windows", `c:\program files\arkame\ARKAME-AGENT.exe`, `C:\Program Files\Arkame\arkame-agent.exe`) {
		t.Error("no Windows o caminho não distingue maiúsculas")
	}
	if mesmoPrograma("linux", "/usr/local/bin/Arkame-agent", "/usr/local/bin/arkame-agent") {
		t.Error("no Linux distingue")
	}
	if mesmoPrograma("linux", "", "") {
		t.Error("vazio não é o mesmo programa")
	}
}
