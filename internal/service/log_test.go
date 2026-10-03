package service

import (
	"path/filepath"
	"testing"
)

// Um log por configuração (um por agente da máquina), ao lado dela.
func TestArquivoDeLog(t *testing.T) {
	dir := filepath.Join("etc", "arkame")
	if got := ArquivoDeLog(filepath.Join(dir, "agent.env")); got != filepath.Join(dir, "agent.log") {
		t.Fatalf("got %q", got)
	}
	if got := ArquivoDeLog(filepath.Join(dir, "aws.env")); got != filepath.Join(dir, "aws.log") {
		t.Fatalf("got %q", got)
	}
}
