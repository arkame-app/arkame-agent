package sync

import (
	"context"
	"io/fs"
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

// No Windows (Go ≥ 1.23), arquivo do OneDrive é ModeIrregular: tem de entrar no
// backup como arquivo comum. Pasta (junção) não é seguida, e fora do Windows a
// regra não vale.
func TestIrregularLegivelNoWindows(t *testing.T) {
	dir := t.TempDir()
	arq := filepath.Join(dir, "nuvem.docx")
	if err := os.WriteFile(arq, []byte("conteúdo do OneDrive"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, ok := irregularLegivel(true, arq, fs.ModeIrregular|0o666)
	if !ok || st.Size() != int64(len("conteúdo do OneDrive")) {
		t.Fatalf("arquivo irregular legível no Windows deveria entrar: ok=%v st=%v", ok, st)
	}
	if _, ok := irregularLegivel(true, dir, fs.ModeIrregular|0o666); ok {
		t.Fatal("reparse point que dá em pasta (junção) não deveria ser copiado como arquivo")
	}
	if _, ok := irregularLegivel(true, filepath.Join(dir, "sumiu"), fs.ModeIrregular); ok {
		t.Fatal("o que não abre fica de fora")
	}
	if _, ok := irregularLegivel(false, arq, fs.ModeIrregular|0o666); ok {
		t.Fatal("fora do Windows, irregular continua de fora")
	}
	if _, ok := irregularLegivel(true, arq, 0o666); ok {
		t.Fatal("arquivo regular segue o caminho comum")
	}
}

// Subpasta sem permissão dentro do plano: os outros arquivos seguem, mas o
// walker avisa no fim (o daemon marca a sessão parcial). Antes, a subárvore
// sumia do backup e a sessão saía "concluída".
func TestWalkSubpastaIlegivelViraErro(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lê pasta sem permissão")
	}
	raiz := t.TempDir()
	if err := os.WriteFile(filepath.Join(raiz, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	trancada := filepath.Join(raiz, "trancada")
	if err := os.MkdirAll(trancada, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trancada, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })

	arquivos, erros := Walk(context.Background(), "/", []string{raiz}, nil)
	n := 0
	for range arquivos {
		n++
	}
	err := <-erros
	if n != 1 {
		t.Fatalf("esperava o arquivo legível, veio %d", n)
	}
	if err == nil || !strings.Contains(err.Error(), "1 itens não puderam ser lidos") || !strings.Contains(err.Error(), "trancada") {
		t.Fatalf("a subpasta ilegível deveria virar erro (backup parcial), veio %v", err)
	}
}
