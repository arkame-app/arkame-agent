//go:build !windows

package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// comoRoot faz o teste ver o agente como root: aí só link do root é do
// sistema, e os que o teste cria (do usuário dele) são de usuário comum.
func comoRoot(t *testing.T) {
	t.Helper()
	antes := euid
	euid = func() int { return 0 }
	t.Cleanup(func() { euid = antes })
}

// Quem manda na pasta de origem troca uma subpasta por um link para
// /root/.ssh; a restauração (root) gravava lá. Agora recusa com
// ErrDestinoLink, e nada é gravado no alvo.
func TestRestauracaoRecusaLinkNoCaminho(t *testing.T) {
	comoRoot(t)
	conteudo := []byte("ssh-ed25519 AAAA atacante")
	opts := Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}

	casos := map[string]func(origem, alvo string) (destPath, destFilename string){
		"subpasta do dest_filename": func(origem, alvo string) (string, string) {
			if err := os.Symlink(alvo, filepath.Join(origem, "ssh")); err != nil {
				t.Fatal(err)
			}
			return origem, "ssh/authorized_keys"
		},
		"subpasta funda": func(origem, alvo string) (string, string) {
			if err := os.MkdirAll(filepath.Join(origem, "a", "b"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(alvo, filepath.Join(origem, "a", "b", "c")); err != nil {
				t.Fatal(err)
			}
			return origem, "a/b/c/authorized_keys"
		},
		"a própria pasta de destino": func(origem, alvo string) (string, string) {
			link := filepath.Join(origem, "restaurados")
			if err := os.Symlink(alvo, link); err != nil {
				t.Fatal(err)
			}
			return link, "authorized_keys"
		},
		"link relativo": func(origem, alvo string) (string, string) {
			rel, err := filepath.Rel(origem, alvo)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, filepath.Join(origem, "ssh")); err != nil {
				t.Fatal(err)
			}
			return origem, "ssh/authorized_keys"
		},
	}
	for nome, montar := range casos {
		t.Run(nome, func(t *testing.T) {
			origem, alvo := t.TempDir(), t.TempDir()
			destPath, destFilename := montar(origem, alvo)
			item := itemDe(destPath, "authorized_keys", conteudo, "overwrite")
			item.DestFilename = destFilename
			err := Run(context.Background(), opts, item)
			if !errors.Is(err, ErrDestinoLink) {
				t.Fatalf("esperava ErrDestinoLink, veio %v", err)
			}
			if es, _ := os.ReadDir(alvo); len(es) != 0 {
				t.Fatalf("gravou no alvo do link: %v", es)
			}
		})
	}
}

// Caminho comum, com pastas que ainda não existem: restaura.
func TestRestauracaoEmCaminhoFundoSemLink(t *testing.T) {
	comoRoot(t)
	conteudo := []byte("conteúdo do backup")
	dir := t.TempDir()
	item := itemDe(dir, "x.txt", conteudo, "suffix-version")
	item.DestFilename = "home/ana/projeto/src/x.txt"
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"}, item); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "home", "ana", "projeto", "src", "x.txt"))
	if err != nil || string(b) != string(conteudo) {
		t.Fatalf("arquivo restaurado: %q, %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "home", "ana", "projeto", "src", "x.txt")); st.Mode().Perm() != modoDeArquivoNovo {
		t.Fatalf("modo = %v", st.Mode().Perm())
	}
}

// Os links do sistema continuam valendo, e no Docker recomeçam no /host: no
// Fedora Atomic /home -> var/home e /opt -> /var/opt. Aqui o "sistema" é o
// próprio usuário do teste (o agente sem root confia nos links dele).
func TestRestauracaoSegueLinkDoSistemaDentroDoHostRoot(t *testing.T) {
	raiz := t.TempDir() // faz o papel do /host
	if err := os.Chmod(raiz, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"var/home/hugo", "var/opt"} {
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
	conteudo := []byte("c")
	opts := Options{S3: bucketComObjeto(t, conteudo), HostRoot: raiz}

	for destPath, quer := range map[string]string{
		"/home/hugo/restaurados": "var/home/hugo/restaurados/a.txt",
		"/opt/app":               "var/opt/app/a.txt", // absoluto: recomeça no /host
	} {
		if err := Run(context.Background(), opts, itemDe(destPath, "a.txt", conteudo, "overwrite")); err != nil {
			t.Fatalf("%s: %v", destPath, err)
		}
		if b, err := os.ReadFile(filepath.Join(raiz, quer)); err != nil || string(b) != "c" {
			t.Fatalf("%s: esperava em %s: %v", destPath, quer, err)
		}
	}

	// Link do sistema numa pasta que aceita escrita dos outros (/tmp): não é
	// do sistema — qualquer um poderia tê-lo posto ali.
	if err := os.Chmod(filepath.Join(raiz, "var"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/opt", filepath.Join(raiz, "var", "lnk")); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), opts, itemDe("/var/lnk", "a.txt", conteudo, "overwrite"))
	if !errors.Is(err, ErrDestinoLink) {
		t.Fatalf("link em pasta aberta a todos: esperava ErrDestinoLink, veio %v", err)
	}
}

// O nome final sendo um link (overwrite): o rename troca o link, não grava no
// alvo dele.
func TestOverwriteSobreLinkNoNomeFinalNaoSegue(t *testing.T) {
	comoRoot(t)
	conteudo := []byte("novo")
	dir, alvo := t.TempDir(), t.TempDir()
	alvoArq := filepath.Join(alvo, "authorized_keys")
	if err := os.WriteFile(alvoArq, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(alvoArq, filepath.Join(dir, "authorized_keys")); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(dir, "authorized_keys", conteudo, "overwrite")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(alvoArq); string(b) != "original" {
		t.Fatalf("gravou no alvo do link: %q", b)
	}
	if st, err := os.Lstat(filepath.Join(dir, "authorized_keys")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("o nome final deveria ser arquivo comum: %v %v", st, err)
	}
}
