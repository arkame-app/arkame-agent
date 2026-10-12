package service

import "testing"

// A variável posta à mão na unit do systemd — onde o administrador põe a
// ARKAME_STATE_DIR quando não quer mexer no arquivo de configuração.
func TestVariavelDaUnit(t *testing.T) {
	unit := `[Unit]
Description=Arkame

[Service]
EnvironmentFile=/etc/arkame/agent.env
Environment=HTTPS_PROXY=http://proxy:3128 ARKAME_STATE_DIR=/dados/estado
Environment="OUTRA=com espaço"
ExecStart=/usr/local/bin/arkame-agent run
`
	if v, tem := variavelDaUnit(unit, "ARKAME_STATE_DIR"); !tem || v != "/dados/estado" {
		t.Fatalf("%q %v", v, tem)
	}
	if v, tem := variavelDaUnit(unit, "OUTRA"); !tem || v != "com espaço" {
		t.Fatalf("%q %v", v, tem)
	}
	if _, tem := variavelDaUnit(unit, "ARKAME_STATE"); tem {
		t.Fatal("ARKAME_STATE não está na unit (ARKAME_STATE_DIR não é ela)")
	}
	// A última atribuição vale; `Environment=` vazio apaga as anteriores.
	if v, _ := variavelDaUnit(unit+"Environment=ARKAME_STATE_DIR=/outro\n", "ARKAME_STATE_DIR"); v != "/outro" {
		t.Fatalf("%q", v)
	}
	if v, tem := variavelDaUnit(unit+"Environment=\n", "ARKAME_STATE_DIR"); v != "" || !tem {
		t.Fatalf("%q %v", v, tem)
	}
}
