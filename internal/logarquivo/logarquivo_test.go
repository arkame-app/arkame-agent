package logarquivo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// O log do serviço cresce para sempre se não houver rodízio: passado o teto,
// o atual vira .1 e o arquivo recomeça.
func TestRodizioPorTamanho(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.log")
	a, err := Abrir(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	linha := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 6; i++ {
		if _, err := a.Write([]byte(linha)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() > 100 {
		t.Fatalf("arquivo atual passou do teto: %v %v", st, err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("o anterior deveria estar em .1: %v", err)
	}
}

// Reabrir acrescenta, não apaga o que já estava.
func TestReabrirAcrescenta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.log")
	a, _ := Abrir(p, TamanhoPadrao)
	_, _ = a.Write([]byte("um\n"))
	a.Close()
	a, _ = Abrir(p, TamanhoPadrao)
	_, _ = a.Write([]byte("dois\n"))
	a.Close()
	if b, _ := os.ReadFile(p); string(b) != "um\ndois\n" {
		t.Fatalf("conteúdo %q", b)
	}
}

// renameFalhando simula o Windows com o log aberto por `Get-Content -Wait`:
// o rename falha sempre. Devolve quantas vezes foi chamado.
func renameFalhando(t *testing.T) *int {
	t.Helper()
	antes := renomear
	n := 0
	renomear = func(string, string) error {
		n++
		return &os.LinkError{Op: "rename", Err: errors.New("o arquivo está sendo usado por outro processo")}
	}
	t.Cleanup(func() { renomear = antes })
	return &n
}

// Rename falhando: o arquivo crescia sem teto (cada escrita tentava e falhava
// de novo). Agora copia para o .1 e trunca; nenhuma linha se perde.
func TestRodizioComRenameFalhandoNaoCresceSemTeto(t *testing.T) {
	renameFalhando(t)
	p := filepath.Join(t.TempDir(), "agent.log")
	a, err := Abrir(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var linhas []string
	for i := 0; i < 50; i++ {
		l := fmt.Sprintf("linha %03d\n", i) // 10 bytes
		linhas = append(linhas, l)
		if n, err := a.Write([]byte(l)); err != nil || n != len(l) {
			t.Fatalf("escrita %d: n=%d err=%v", i, n, err)
		}
	}
	atual, _ := os.ReadFile(p)
	anterior, _ := os.ReadFile(p + ".1")
	if len(atual) > 100 {
		t.Fatalf("arquivo atual passou do teto com o rename falhando: %d bytes", len(atual))
	}
	if len(anterior) == 0 {
		t.Fatal("o conteúdo anterior deveria estar no .1")
	}
	// As últimas linhas estão inteiras, na ordem, em .1 + atual.
	junto := string(anterior) + string(atual)
	if !strings.HasSuffix(junto, strings.Join(linhas[len(linhas)-15:], "")) {
		t.Fatalf("linhas perdidas no rodízio: %q", junto)
	}
}

// Nem rename nem cópia (o .1 não pode ser recriado): não insiste a cada
// escrita, e as linhas continuam sendo gravadas.
func TestRodizioImpossivelRecuaENaoPerdeLinhas(t *testing.T) {
	chamadas := renameFalhando(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.log")
	// .1 como pasta com conteúdo: não se apaga nem se abre para escrita.
	if err := os.MkdirAll(filepath.Join(p+".1", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := Abrir(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 100; i++ {
		l := fmt.Sprintf("linha %03d\n", i)
		if n, err := a.Write([]byte(l)); err != nil || n != len(l) {
			t.Fatalf("escrita %d: n=%d err=%v", i, n, err)
		}
	}
	if b, _ := os.ReadFile(p); len(b) != 1000 {
		t.Fatalf("todas as linhas deveriam estar no arquivo: %d bytes", len(b))
	}
	// 900 bytes além do teto, recuo de 25 bytes: no máximo ~36 tentativas.
	// Sem recuo, eram 90 (uma por escrita).
	if *chamadas > 40 {
		t.Fatalf("rename tentado %d vezes; deveria recuar entre as tentativas", *chamadas)
	}
}
