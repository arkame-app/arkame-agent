package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A troca do programa: o novo entra, o antigo sai, e nada pela metade fica.
func TestCopiarPrograma(t *testing.T) {
	d := t.TempDir()
	de, para := filepath.Join(d, "baixado.exe"), filepath.Join(d, "Arkame", "arkame-agent.exe")
	if err := os.WriteFile(de, []byte("novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copiarPrograma(de, para); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(de, []byte("mais novo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copiarPrograma(de, para); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(para); string(b) != "mais novo" {
		t.Fatalf("programa = %q", b)
	}
	for _, sobra := range []string{para + ".novo", para + ".old"} {
		if _, err := os.Stat(sobra); !os.IsNotExist(err) {
			t.Fatalf("sobrou %s", sobra)
		}
	}
}
