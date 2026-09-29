package fsbrowse

import (
	"os"
	"path/filepath"
	"testing"
)

// No Docker o servidor está em /host: o navegador tem de listar ele, e não o
// container (achado em 28/09).
func TestListDirDentroDoHostRoot(t *testing.T) {
	raiz := t.TempDir()
	if err := os.MkdirAll(filepath.Join(raiz, "srv", "dados"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := ListDir(raiz, "/srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(e) != 1 || e[0].Name != "dados" || !e[0].Dir {
		t.Fatalf("entradas = %+v", e)
	}
	if _, err := ListDir(raiz, "srv"); err == nil {
		t.Fatal("caminho relativo deveria ser recusado")
	}
}
