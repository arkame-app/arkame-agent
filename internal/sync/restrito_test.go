package sync

import (
	"context"
	"errors"
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

// A pasta vira link para /root/.ssh entre a varredura e a leitura: o caminho
// que a varredura viu é inocente, mas o arquivo aberto é a chave. A leitura
// confere o que foi de fato aberto (revisão, 18/10).
func TestLeituraRestritaConfereOArquivoAberto(t *testing.T) {
	if caminho.Windows {
		t.Skip("links simbólicos do Unix")
	}
	raiz := t.TempDir()
	for _, rel := range []string{"home/bob/d/id_ed25519", "root/.ssh/id_ed25519"} {
		p := filepath.Join(raiz, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("chave"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	visto := filepath.Join(raiz, "home/bob/d/id_ed25519")
	abrir := func() error {
		f, err := os.Open(visto)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return conferirAberto(raiz, f)
	}
	if err := abrir(); err != nil {
		t.Fatalf("a pasta comum foi recusada: %v", err)
	}
	// A troca, depois da varredura.
	if err := os.RemoveAll(filepath.Join(raiz, "home/bob/d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(raiz, "root/.ssh"), filepath.Join(raiz, "home/bob/d")); err != nil {
		t.Fatal(err)
	}
	if err := abrir(); !errors.Is(err, ErrCaminhoRestrito) {
		t.Fatalf("a chave trocada no meio passou: %v", err)
	}
	_, c := novoS3Falso(t)
	o := EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: raiz, Restrito: true}
	if _, _, err := processFile(context.Background(), o, FileInfo{AbsolutePath: visto, RelativePath: "home/bob/d/id_ed25519", Size: 5}); !errors.Is(err, ErrCaminhoRestrito) {
		t.Fatalf("processFile enviou a chave: %v", err)
	}
}

// O Run conta o que o plano restrito deixou de fora, para o /complete levar
// ao painel, e a sessão segue completa (é como uma exclusão).
func TestRunContaOsRestritos(t *testing.T) {
	if caminho.Windows {
		t.Skip("caminhos do Unix")
	}
	raiz := t.TempDir()
	for _, rel := range []string{"home/ana/doc.txt", "home/ana/.ssh/id_ed25519", "home/ana/.aws/credentials"} {
		p := filepath.Join(raiz, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, c := novoS3Falso(t)
	r, err := Run(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/", HostRoot: raiz, SourcePaths: []string{"/home"}, Restrito: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.RestrictedSkipped != 2 || r.Stats.FilesTotal != 1 || r.FilesFailed != 0 {
		t.Fatalf("restritos=%d arquivos=%d falhas=%d", r.RestrictedSkipped, r.Stats.FilesTotal, r.FilesFailed)
	}
}
