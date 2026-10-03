package logarquivo

import (
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
