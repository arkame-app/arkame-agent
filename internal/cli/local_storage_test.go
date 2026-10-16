package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

func opcoesBoas(t *testing.T) opcoesDaCopiaLocal {
	return opcoesDaCopiaLocal{storageID: "0b6f0c1e-5f1a-4f6e-9d1e-2a7c3e9b8d41", bucket: "arkame-local-a1b2c3", pasta: filepath.Join(t.TempDir(), "copia"), porta: 9900}
}

func TestValidarCopiaLocal(t *testing.T) {
	if err := opcoesBoas(t).validar(); err != nil {
		t.Fatal(err)
	}
	ruins := map[string]func(*opcoesDaCopiaLocal){
		"storage-id que não é UUID": func(o *opcoesDaCopiaLocal) { o.storageID = "x; rm -rf /" },
		"bucket com maiúscula":      func(o *opcoesDaCopiaLocal) { o.bucket = "Arkame" },
		"porta privilegiada":        func(o *opcoesDaCopiaLocal) { o.porta = 80 },
		"pasta relativa":            func(o *opcoesDaCopiaLocal) { o.pasta = "copia" },
	}
	if runtime.GOOS != "windows" {
		ruins["pasta em /etc (só leitura para o serviço)"] = func(o *opcoesDaCopiaLocal) { o.pasta = "/etc/arkame/copia" }
		ruins["a raiz"] = func(o *opcoesDaCopiaLocal) { o.pasta = "/" }
	}
	for nome, estraga := range ruins {
		o := opcoesBoas(t)
		estraga(&o)
		if err := o.validar(); err == nil {
			t.Errorf("%s: aceito", nome)
		}
	}
}

func TestLinhasDaCopiaLocalUsamAMesmaIdentidade(t *testing.T) {
	main := &config.Config{PanelURL: "https://save.arkame.app", AgentID: "ag-1", TokenPath: "/etc/arkame/token.jwt", PrivateKeyPath: "/etc/arkame/key.pem", AgentIDPath: "/etc/arkame/agent.id", StorageBucket: "nuvem", SiblingBuckets: []string{"outro", "arkame-local-a1b2c3"}}
	o := opcoesBoas(t)
	l := strings.Join(linhasDaCopiaLocal(main, o, "AK", "SK", "/opt/arkame/rustfs/rustfs", "/etc/arkame/rustfs"), "\n")
	for _, quer := range []string{
		"AGENT_ID=ag-1", "TOKEN_PATH=/etc/arkame/token.jwt", "PRIVATE_KEY_PATH=/etc/arkame/key.pem",
		"STORAGE_BUCKET=arkame-local-a1b2c3", "STORAGE_ENDPOINT=http://127.0.0.1:9900", "STORAGE_ID=" + o.storageID,
		"SIBLING_BUCKETS=nuvem,outro", "ARKAME_LOCAL_STORE_ADDRESS=127.0.0.1:9900",
		"ARKAME_LOCAL_STORE_ACCESS_KEY_FILE=" + filepath.Join("/etc/arkame/rustfs", "access.key"),
	} {
		if !strings.Contains(l, quer+"\n") && !strings.HasSuffix(l, quer) {
			t.Errorf("faltou %q em:\n%s", quer, l)
		}
	}
	if strings.Contains(l, "0.0.0.0") {
		t.Fatal("a cópia local não pode escutar fora de 127.0.0.1")
	}
	if got := irmaosDoPrincipal(main, "arkame-local-a1b2c3"); got != "outro,arkame-local-a1b2c3" {
		t.Fatalf("irmãos do principal: %q (sem repetir o local)", got)
	}
	main.SiblingBuckets = nil
	if got := irmaosDoPrincipal(main, "arkame-local-a1b2c3"); got != "arkame-local-a1b2c3" {
		t.Fatalf("irmãos do principal sem irmãos antes: %q", got)
	}
}

func TestCredencialLocalReaproveitaNaReinstalacao(t *testing.T) {
	pasta := filepath.Join(t.TempDir(), "rustfs")
	a1, s1, err := credencialLocal(pasta)
	if err != nil {
		t.Fatal(err)
	}
	if len(a1) != 20 || len(s1) != 40 || strings.ContainsAny(a1+s1, "-_=+/ \n") {
		t.Fatalf("credencial fora do formato: %q %q", a1, s1)
	}
	a2, s2, err := credencialLocal(pasta)
	if err != nil || a1 != a2 || s1 != s2 {
		t.Fatalf("reinstalar trocou a credencial (o bucket local ficaria inacessível): %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(pasta, "secret.key"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("segredo com modo %v", fi.Mode().Perm())
		}
	}
}
