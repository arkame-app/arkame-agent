package config

import (
	"os"
	"path/filepath"
	"testing"
)

// O irmão gravado pela 0.5.3 (sem ARKAME_ROLE, com off) é reconhecido pelo
// nome do arquivo; o principal com off continua off.
func TestIrmaoDaVersaoAnteriorPeloNomeDoArquivo(t *testing.T) {
	dir := t.TempDir()
	grava := func(nome string) *Config {
		p := filepath.Join(dir, nome)
		if err := os.WriteFile(p, []byte("ARKAME_AUTO_UPDATE=off\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := grava("agent-rede-arkame-local-a1.env"); c.Role != "network_copy" || c.AutoUpdate != "follow" {
		t.Fatalf("cópia da rede: role=%q auto=%q", c.Role, c.AutoUpdate)
	}
	if c := grava("agent-local.env"); c.Role != "" || c.AutoUpdate != "follow" {
		t.Fatalf("cópia local: role=%q auto=%q", c.Role, c.AutoUpdate)
	}
	if c := grava("agent.env"); c.Role != "" || c.AutoUpdate != "off" {
		t.Fatalf("principal: role=%q auto=%q", c.Role, c.AutoUpdate)
	}
}
