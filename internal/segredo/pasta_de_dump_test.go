package segredo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Pasta nova: criada fechada; a mesma pasta de novo, com o dump da véspera do
// próprio agente dentro, segue valendo.
func TestPastaDeDumpCriaFechadaEReaproveita(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	dir := filepath.Join(t.TempDir(), "dumps", "postgres")
	if err := PastaDeDump(dir, ""); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("pasta do dump: %v, modo %v", err, st.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(dir, "todos.sql.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PastaDeDump(dir, ""); err != nil {
		t.Fatalf("o dump do próprio agente dentro da pasta não pode impedir: %v", err)
	}
}

// Um arquivo de outro dono dentro da pasta (plantado no instante em que ela
// ainda herdava a permissão de cima) faz recusar: o SQL Server regravaria o
// conteúdo dele, e o dono leria o banco.
func TestPastaDeDumpRecusaArquivoAlheio(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dumps")
	if err := PastaDeDump(dir, ""); err != nil {
		t.Fatal(err)
	}
	plantado := filepath.Join(dir, "Contab.bak")
	if err := os.WriteFile(plantado, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	donoFalso(t, plantado)
	if err := PastaDeDump(dir, ""); !errors.Is(err, ErrDonoNaoConfiavel) || !strings.Contains(err.Error(), "apague") {
		t.Fatalf("arquivo de outro dono aceito: %v", err)
	}
}

// A pasta já existente de outro dono é recusada (PastaPrivada), e um link
// dentro dela também.
func TestPastaDeDumpRecusaPastaAlheiaELink(t *testing.T) {
	base := t.TempDir()
	alheia := filepath.Join(base, "alheia")
	if err := os.Mkdir(alheia, 0o777); err != nil {
		t.Fatal(err)
	}
	donoFalso(t, alheia)
	if err := PastaDeDump(alheia, ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta de outro dono aceita: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	minha := filepath.Join(base, "minha")
	if err := PastaDeDump(minha, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(minha, "Contab.bak")); err != nil {
		t.Fatal(err)
	}
	if err := PastaDeDump(minha, ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("link dentro da pasta do dump aceito: %v", err)
	}
}

// Caminho relativo e --writer fora do Windows são recusados.
func TestPastaDeDumpRecusaRelativoEWriterForaDoWindows(t *testing.T) {
	if err := PastaDeDump("dumps", ""); err == nil {
		t.Fatal("caminho relativo aceito")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := PastaDeDump(filepath.Join(t.TempDir(), "d"), `NT Service\MSSQLSERVER`); err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("--writer fora do Windows: %v", err)
	}
}

// Uma pasta acima de outro dono: ele renomearia a pasta do dump e poria a sua
// no lugar entre um dump e outro. Vale também quando a pasta do dump já existe.
func TestPastaDeDumpRecusaPastaAcimaAlheia(t *testing.T) {
	base := t.TempDir()
	acima := filepath.Join(base, "Dumps")
	dir := filepath.Join(acima, "sql")
	if err := PastaDeDump(dir, ""); err != nil {
		t.Fatal(err)
	}
	donoFalso(t, acima)
	if err := PastaDeDump(dir, ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta acima de outro dono aceita: %v", err)
	}
}
