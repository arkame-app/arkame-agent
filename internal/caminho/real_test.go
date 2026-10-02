//go:build !windows

package caminho

import (
	"os"
	"path/filepath"
	"testing"
)

// servidorAtomic monta, num diretório que faz o papel de /host, o que o Fedora
// Atomic tem: /home -> var/home (relativo), /opt -> /var/opt (absoluto).
func servidorAtomic(t *testing.T) string {
	t.Helper()
	raiz := t.TempDir()
	for _, d := range []string{"var/home/hugo", "var/opt/app"} {
		if err := os.MkdirAll(filepath.Join(raiz, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("var/home", filepath.Join(raiz, "home")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/opt", filepath.Join(raiz, "opt")); err != nil {
		t.Fatal(err)
	}
	return raiz
}

func TestRealSegueLinksDentroDoServidor(t *testing.T) {
	raiz := servidorAtomic(t)
	casos := map[string]string{
		"/home/hugo":        "var/home/hugo",
		"/opt/app":          "var/opt/app", // absoluto: recomeça no servidor, não no container
		"/var/home":         "var/home",
		"/home/../etc":      "var/etc", // como o kernel: `..` depois do link sobe a partir do destino dele
		"/../../../etc":     "etc",     // não sobe acima do servidor
		"/home/hugo/nao-ha": "var/home/hugo/nao-ha",
	}
	for p, quer := range casos {
		if got := real(raiz, p); got != filepath.Join(raiz, quer) {
			t.Errorf("real(%q) = %q, quero %q", p, got, filepath.Join(raiz, quer))
		}
	}
}

func TestRealNaoLacaComLinkCircular(t *testing.T) {
	raiz := t.TempDir()
	if err := os.Symlink("b", filepath.Join(raiz, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(raiz, "b")); err != nil {
		t.Fatal(err)
	}
	_ = real(raiz, "/a/x") // basta terminar
}

// Restauração no lugar de origem: a pasta final passa por um link absoluto
// (/opt -> /var/opt) e tem de ficar no servidor, não no container.
func TestRealNoDiscoDaRestauracao(t *testing.T) {
	raiz := servidorAtomic(t)
	got := RealNoDisco(raiz, filepath.Join(raiz, "opt", "app", "conf"))
	if got != filepath.Join(raiz, "var", "opt", "app", "conf") {
		t.Fatalf("RealNoDisco = %q", got)
	}
}
