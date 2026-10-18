package sync

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/caminho"
)

// Plano do MCP com /home: a origem passa, mas /home/ana/.ssh e o link para a
// chave do Let's Encrypt ficam de fora. Sem a marca, tudo entra (revisão de
// segurança, 18/10).
func TestWalkRestritoPulaOSensivelDentroDaOrigem(t *testing.T) {
	if caminho.Windows {
		t.Skip("links simbólicos do Unix")
	}
	raiz := t.TempDir()
	arquivo := func(rel string) {
		p := filepath.Join(raiz, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	arquivo("home/ana/doc.txt")
	arquivo("home/ana/.ssh/id_ed25519")
	arquivo("etc/letsencrypt/live/site/privkey.pem")
	if err := os.Symlink("/etc/letsencrypt/live/site/privkey.pem", filepath.Join(raiz, "home/ana/cert.pem")); err != nil {
		t.Fatal(err)
	}
	ler := func(restrito bool) []string {
		arquivos, erros := walkSem(context.Background(), raiz, []string{"/home"}, nil, nil, nil, nil, nil, restrito)
		var vistos []string
		for f := range arquivos {
			vistos = append(vistos, strings.TrimPrefix(filepath.ToSlash(f.RelativePath), "/"))
		}
		if err := <-erros; err != nil {
			t.Fatal(err)
		}
		sort.Strings(vistos)
		return vistos
	}
	if got := strings.Join(ler(true), ","); got != "home/ana/doc.txt" {
		t.Errorf("restrito: %s", got)
	}
	if got := strings.Join(ler(false), ","); got != "home/ana/.ssh/id_ed25519,home/ana/cert.pem,home/ana/doc.txt" {
		t.Errorf("sem restrição: %s", got)
	}
}
