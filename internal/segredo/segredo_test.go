package segredo

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Arquivo antigo com permissão larga: o os.WriteFile manteria o 0644; o Gravar
// troca o arquivo e ele fica 0600.
func TestGravarRestringeArquivoQueJaExistia(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	p := filepath.Join(t.TempDir(), "token.jwt")
	if err := os.WriteFile(p, []byte("velho"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Gravar(p, []byte("novo"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("modo %v, queria 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "novo" {
		t.Fatalf("conteúdo %q", b)
	}
	if _, err := os.Stat(p + ".novo"); !os.IsNotExist(err) {
		t.Fatal("o temporário ficou para trás")
	}
}

// A pasta criada pelo agente nasce fechada; a que já existia não é tocada.
func TestCriarPasta(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	base := t.TempDir()
	nova := filepath.Join(base, "etc", "arkame")
	if err := CriarPasta(nova); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(nova); st.Mode().Perm() != 0o700 {
		t.Fatalf("pasta nova com modo %v", st.Mode().Perm())
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CriarPasta(base); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(base); st.Mode().Perm() != 0o755 {
		t.Fatal("pasta que já existia não deveria mudar")
	}
}
