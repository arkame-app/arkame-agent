//go:build linux

package service

import (
	"strings"
	"testing"
)

// O reinício segue o escopo da instalação: o serviço do usuário não é o do
// sistema, e `set-storage-keys --restart` reiniciava o errado.
func TestRestartArgsSegueOEscopo(t *testing.T) {
	if got := strings.Join(RestartArgs("arkame-agent-aws", ScopeUser), " "); got != "systemctl --user restart arkame-agent-aws" {
		t.Fatalf("usuário: %q", got)
	}
	if got := strings.Join(RestartArgs("", ScopeSystem), " "); got != "systemctl restart arkame-agent" {
		t.Fatalf("sistema, nome padrão: %q", got)
	}
	if got := RestartCommand("arkame-agent", ScopeSystem); got != "sudo systemctl restart arkame-agent" {
		t.Fatalf("comando do sistema mostrado ao operador: %q", got)
	}
	if got := RestartCommand("arkame-agent", ScopeUser); got != "systemctl --user restart arkame-agent" {
		t.Fatalf("comando do usuário, sem sudo: %q", got)
	}
}

// O agente no macOS reporta o label do launchd como nome; o painel o devolve
// em --service-name, e o reinício não pode prefixá-lo de novo.
func TestLaunchdLabelAceitaOProprioLabel(t *testing.T) {
	for nome, label := range map[string]string{
		"arkame-agent":     "app.arkame.agent",
		"arkame-agent-aws": "app.arkame.agent-aws",
		"app.arkame.agent": "app.arkame.agent",
	} {
		if got := LaunchdLabel(nome); got != label {
			t.Errorf("LaunchdLabel(%q) = %q, esperado %q", nome, got, label)
		}
	}
}
