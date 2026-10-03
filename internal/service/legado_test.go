package service

import (
	"strings"
	"testing"
)

// Até a v0.4.3 o install aceitava qualquer nome. Reinstalar em cima
// (`service install --name backup-oci`) era recusado pelo prefixo: o nome de
// um serviço que já existe e chama este programa vale; o de um serviço novo,
// não.
func TestValidarNomeAceitaServicoLegadoDoPrograma(t *testing.T) {
	existentes := map[string]bool{"backup-oci": true}
	existe := func(n string) bool { return existentes[n] }

	if err := validarNome("backup-oci", existe); err != nil {
		t.Fatalf("serviço legado deste programa recusado: %v", err)
	}
	if err := validarNome("backup-aws", existe); err == nil || !strings.Contains(err.Error(), "arkame-agent-backup-aws") {
		t.Fatalf("nome novo sem prefixo deveria ser recusado com sugestão: %v", err)
	}
	if err := validarNome("arkame-agent-aws", existe); err != nil {
		t.Fatalf("nome com prefixo: %v", err)
	}
	// A forma do nome vale sempre, legado ou não.
	if err := validarNome("Backup-OCI", func(string) bool { return true }); err == nil {
		t.Fatal("nome inválido aceito por existir")
	}
}

// O programa de um plist gerado pelo agente é o primeiro item do
// ProgramArguments (não o Label, que vem antes).
func TestProgramaDoPlist(t *testing.T) {
	p := montarPlist("app.arkame.backup-oci", "/usr/local/bin/arkame & cia/arkame-agent", "/etc/arkame/a.env", "/var/log/x.log", ScopeSystem)
	if got := programaDoPlist(p); got != "/usr/local/bin/arkame & cia/arkame-agent" {
		t.Fatalf("programa = %q", got)
	}
	if got := programaDoPlist("<plist><dict><key>Label</key><string>x</string></dict></plist>"); got != "" {
		t.Fatalf("sem ProgramArguments: %q", got)
	}
}
