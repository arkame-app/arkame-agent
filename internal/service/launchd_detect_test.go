package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

// O agente com --service-name ou no escopo do usuário reportava sempre
// app.arkame.agent (ou nada): o plist agora diz ao processo o label e o escopo.
func TestPlistLevaLabelEEscopoAoProcesso(t *testing.T) {
	p := montarPlist("app.arkame.agent-aws", "/usr/local/bin/arkame-agent", "/Users/a b/&.env", "/tmp/x.log", ScopeUser)
	if err := xml.Unmarshal([]byte(p), new(struct{})); err != nil {
		t.Fatalf("plist não é XML válido: %v", err)
	}
	for _, trecho := range []string{
		"<key>ARKAME_SERVICE_NAME</key>\n\t\t<string>app.arkame.agent-aws</string>",
		"<key>ARKAME_SERVICE_SCOPE</key>\n\t\t<string>user</string>",
	} {
		if !strings.Contains(p, trecho) {
			t.Errorf("plist sem %q:\n%s", trecho, p)
		}
	}
	// O uninstall de outro agente continua achando o env-file no plist.
	if c := configDoPlist(p); c != "/Users/a b/&.env" {
		t.Errorf("configDoPlist: %q", c)
	}
}

func TestDetectLaunchd(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	nenhum := func(string) bool { return false }
	padrao := func(p string) bool { return p == "/Library/LaunchDaemons/app.arkame.agent.plist" }

	casos := []struct {
		nome   string
		getenv func(string) string
		existe func(string) bool
		quer   Detected
	}{
		{"--service-name no sistema", env(map[string]string{envNomeDoServico: "app.arkame.agent-aws", envEscopoDoServico: "system"}), padrao,
			Detected{Name: "app.arkame.agent-aws", Scope: "launchd"}},
		{"LaunchAgent do usuário", env(map[string]string{envNomeDoServico: "app.arkame.agent", envEscopoDoServico: "user"}), nenhum,
			Detected{Name: "app.arkame.agent", Scope: "launchd-user"}},
		{"plist antigo, LaunchDaemon padrão", env(nil), padrao, Detected{Name: "app.arkame.agent", Scope: "launchd"}},
		{"à mão", env(nil), nenhum, Detected{}},
	}
	for _, c := range casos {
		if got := detectLaunchd(c.getenv, c.existe); got != c.quer {
			t.Errorf("%s: %+v, queria %+v", c.nome, got, c.quer)
		}
	}
}
