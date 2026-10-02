package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pasta do plano que não abre é falha do backup, e não um backup vazio
// "concluído" — era o que acontecia com `C:\Users\…` no Windows (28/09).
func TestWalkPastaInexistenteFalha(t *testing.T) {
	arquivos, erros := Walk(context.Background(), "/", []string{filepath.Join(t.TempDir(), "nao-existe")}, nil)
	for range arquivos {
	}
	err := <-erros
	if err == nil || !strings.Contains(err.Error(), "não consegui ler") {
		t.Fatalf("esperava falha ao ler a pasta do plano, veio %v", err)
	}
}

// Uma pasta que não abre não impede as outras do plano.
func TestWalkUmaPastaRuimNaoBloqueiaAsOutras(t *testing.T) {
	boa := t.TempDir()
	if err := os.WriteFile(filepath.Join(boa, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), "/", []string{filepath.Join(boa, "nao-existe"), boa}, nil)
	n := 0
	for range arquivos {
		n++
	}
	if err := <-erros; err == nil || n != 1 {
		t.Fatalf("esperava 1 arquivo da pasta boa e o erro da ruim; veio %d arquivos, erro %v", n, err)
	}
}

func TestWalkChaveRelativaAoHostRoot(t *testing.T) {
	raiz := t.TempDir()
	if err := os.MkdirAll(filepath.Join(raiz, "var", "dados"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "var", "dados", "A.TMP"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "var", "dados", "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), raiz, []string{"/var/dados"}, []string{"*.tmp"})
	var chaves []string
	for f := range arquivos {
		chaves = append(chaves, f.RelativePath)
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	// No Linux, *.tmp não pega A.TMP (nomes diferenciam maiúsculas); no Windows pega.
	if len(chaves) != 2 || chaves[0] != "var/dados/A.TMP" && chaves[1] != "var/dados/A.TMP" {
		t.Fatalf("chaves = %v", chaves)
	}
}

// Origem que é link (/home no Fedora Atomic): lê o destino, mas a chave no
// bucket segue o caminho escolhido no plano. Link de arquivo dentro da pasta
// copia o arquivo para onde aponta, resolvido no servidor.
func TestWalkOrigemQueELink(t *testing.T) {
	raiz := t.TempDir()
	casa := filepath.Join(raiz, "var", "home", "hugo")
	if err := os.MkdirAll(casa, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(casa, "nota.txt"), []byte("oi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(raiz, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raiz, "etc", "conf"), []byte("servidor"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("var/home", filepath.Join(raiz, "home")); err != nil {
		t.Fatal(err)
	}
	// Link absoluto para /etc/conf: no Docker, tem de ser o /etc do servidor.
	if err := os.Symlink("/etc/conf", filepath.Join(casa, "conf-link")); err != nil {
		t.Fatal(err)
	}
	arquivos, erros := Walk(context.Background(), raiz, []string{"/home/hugo"}, nil)
	got := map[string]string{}
	for f := range arquivos {
		got[f.RelativePath] = f.AbsolutePath
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	if got["home/hugo/nota.txt"] != filepath.Join(casa, "nota.txt") {
		t.Fatalf("arquivo pela origem-link: %v", got)
	}
	if got["home/hugo/conf-link"] != filepath.Join(raiz, "etc", "conf") {
		t.Fatalf("link absoluto deveria apontar para o /etc do servidor: %v", got)
	}
}
