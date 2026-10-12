//go:build linux

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// A unit e os drop-ins dela, na ordem do systemd: o drop-in vence a unit.
func TestVariavelDoServicoLeAUnitEOsDropIns(t *testing.T) {
	antes := dirUnitsDoSistema
	t.Cleanup(func() { dirUnitsDoSistema = antes })
	dirUnitsDoSistema = t.TempDir()
	unit := filepath.Join(dirUnitsDoSistema, "arkame-agent.service")
	if err := os.WriteFile(unit, []byte("[Service]\nEnvironment=ARKAME_STATE_DIR=/da/unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := VariavelDoServico("arkame-agent", ScopeSystem, "ARKAME_STATE_DIR"); v != "/da/unit" {
		t.Fatalf("%q", v)
	}
	if err := os.MkdirAll(unit+".d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit+".d", "50-estado.conf"), []byte("[Service]\nEnvironment=ARKAME_STATE_DIR=/do/drop-in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := VariavelDoServico("arkame-agent", ScopeSystem, "ARKAME_STATE_DIR"); v != "/do/drop-in" {
		t.Fatalf("%q", v)
	}
	if v := VariavelDoServico("nao-instalado", ScopeSystem, "ARKAME_STATE_DIR"); v != "" {
		t.Fatalf("%q", v)
	}
	if v := VariavelDoServico("../etc/passwd", ScopeSystem, "ARKAME_STATE_DIR"); v != "" {
		t.Fatalf("nome inválido: %q", v)
	}
}
