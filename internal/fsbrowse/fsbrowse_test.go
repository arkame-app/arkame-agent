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

// Fedora Atomic: /home é link para var/home. O navegador o mostrava como
// arquivo de 8 bytes (fundador, 02/10); tem de ser pasta, e abrir.
func TestListDirLinkDePastaEPasta(t *testing.T) {
	raiz := t.TempDir()
	if err := os.MkdirAll(filepath.Join(raiz, "var", "home", "hugo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("var/home", filepath.Join(raiz, "home")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/home", filepath.Join(raiz, "home-abs")); err != nil {
		t.Fatal(err)
	}
	e, err := ListDir(raiz, "/")
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for _, x := range e {
		dirs[x.Name] = x.Dir
	}
	if !dirs["home"] || !dirs["home-abs"] {
		t.Fatalf("links de pasta deveriam ser pastas: %+v", e)
	}
	dentro, err := ListDir(raiz, "/home")
	if err != nil || len(dentro) != 1 || dentro[0].Name != "hugo" {
		t.Fatalf("abrir /home: %+v, %v", dentro, err)
	}
}
