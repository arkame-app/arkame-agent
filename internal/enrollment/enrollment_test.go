package enrollment

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
)

// O token e a chave privada saem sempre restritos ao administrador — inclusive
// por cima de um arquivo antigo com permissão larga (o os.WriteFile mantinha
// o modo do arquivo existente; no Windows, o 0600 nem restringe).
func TestTokenEChaveGravadosProtegidos(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a proteção é a DACL, não o modo")
	}
	dir := t.TempDir()
	cfg := &config.Config{TokenPath: filepath.Join(dir, "token.jwt")}
	if err := os.WriteFile(cfg.TokenPath, []byte("antigo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PersistToken(cfg, "jwt"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(cfg.TokenPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("token com modo %v, queria 0600", st.Mode().Perm())
	}

	chave := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(chave, []byte("antiga"), 0o644); err != nil {
		t.Fatal(err)
	}
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := kp.SaveToDisk(chave); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(chave); st.Mode().Perm() != 0o600 {
		t.Fatalf("chave privada com modo %v, queria 0600", st.Mode().Perm())
	}
}
