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
	antes := pastasTemporarias
	pastasTemporarias = nil // a pasta do teste fica na temporária
	t.Cleanup(func() { pastasTemporarias = antes })
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
		ruins["em /tmp (o sistema apaga o que fica velho)"] = func(o *opcoesDaCopiaLocal) { o.pasta = "/tmp/arkame-local" }
		ruins["em /var/tmp"] = func(o *opcoesDaCopiaLocal) { o.pasta = "/var/tmp/copia" }
	}
	for nome, estraga := range ruins {
		o := opcoesBoas(t)
		pastasTemporarias = []string{"/tmp", "/var/tmp"}
		estraga(&o)
		if err := o.validar(); err == nil {
			t.Errorf("%s: aceito", nome)
		}
	}
}

func TestLinhasDaCopiaLocalUsamAMesmaIdentidade(t *testing.T) {
	main := &config.Config{PanelURL: "https://save.arkame.app", AgentID: "ag-1", TokenPath: "/etc/arkame/token.jwt", PrivateKeyPath: "/etc/arkame/key.pem", AgentIDPath: "/etc/arkame/agent.id", StorageBucket: "nuvem", SiblingBuckets: []string{"outro", "arkame-local-a1b2c3"}}
	o := opcoesBoas(t)
	prog := programaDaCopia{caminho: "/opt/arkame/rustfs/rustfs", sha256: "abc123"}
	l := strings.Join(linhasDaCopiaLocal(main, o, "AK", "SK", prog, "/etc/arkame/rustfs", "/etc/arkame/rustfs/tls", "/var/log/arkame/rustfs.log"), "\n")
	for _, quer := range []string{
		"AGENT_ID=ag-1", "TOKEN_PATH=/etc/arkame/token.jwt", "PRIVATE_KEY_PATH=/etc/arkame/key.pem",
		"STORAGE_BUCKET=arkame-local-a1b2c3", "STORAGE_ENDPOINT=https://127.0.0.1:9900", "STORAGE_ID=" + o.storageID,
		"STORAGE_CA_FILE=" + filepath.Join("/etc/arkame/rustfs/tls", "rustfs_cert.pem"),
		"ARKAME_LOCAL_STORE_TLS=/etc/arkame/rustfs/tls", "ARKAME_LOCAL_STORE_PROGRAM_SHA256=abc123",
		"ARKAME_AUTO_UPDATE=off", "ARKAME_LOCAL_STORE_LOG=/var/log/arkame/rustfs.log",
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

// As opções do principal vão para a cópia local (proxy, estado, intervalos);
// a identidade, o armazenamento e a atualização automática, não.
func TestOpcoesDoPrincipalVaoParaACopiaLocal(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "agent.env")
	os.WriteFile(cfg, []byte("AWS_CA_BUNDLE=/etc/ca-corporativa.pem\nPANEL_URL=https://save.arkame.app\nAGENT_ID=ag-1\nSTORAGE_SECRET_KEY=segredo-da-nuvem\nSTORAGE_BUCKET=nuvem\nSIBLING_BUCKETS=x\nARKAME_AUTO_UPDATE=on\nENROLLMENT_TOKEN=usado\n# comentário\nHTTPS_PROXY=http://proxy:3128\nNO_PROXY=127.0.0.1\nARKAME_STATE_DIR=/srv/estado\nUPLOAD_CONCURRENCY=2\n"), 0o600)
	l, err := opcoesDoPrincipal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	junto := strings.Join(l, "\n")
	for _, quer := range []string{"HTTPS_PROXY=http://proxy:3128", "NO_PROXY=127.0.0.1", "ARKAME_STATE_DIR=/srv/estado", "UPLOAD_CONCURRENCY=2"} {
		if !strings.Contains(junto, quer) {
			t.Errorf("faltou %q", quer)
		}
	}
	for _, nao := range []string{"AWS_CA_BUNDLE", "segredo-da-nuvem", "STORAGE_", "SIBLING", "AUTO_UPDATE", "ENROLLMENT", "AGENT_ID", "PANEL_URL", "#"} {
		if strings.Contains(junto, nao) {
			t.Errorf("%q não devia ir para a cópia local:\n%s", nao, junto)
		}
	}
}

// A pasta dos dados: a nova nasce fechada; a que já existia aberta e vazia (ou
// só com o que é da cópia local) é fechada; com outra coisa dentro, recusada.
func TestFecharPastaDosDados(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modo de arquivo")
	}
	base := t.TempDir()
	nova := filepath.Join(base, "nova", "copia")
	if err := fecharPastaDosDados(nova, "b"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(nova); fi.Mode().Perm() != 0o700 {
		t.Fatalf("nova: %v", fi.Mode().Perm())
	}
	// As pastas de cima que faltavam também nascem fechadas: criadas abertas
	// (0755) pelo agente, o dono do caminho acima as renomeava e punha outra no
	// lugar (o PastaPrivada só confere a primeira que já existia).
	if fi, _ := os.Stat(filepath.Dir(nova)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("a pasta de cima criada pelo agente: %v", fi.Mode().Perm())
	}
	aberta := filepath.Join(base, "aberta")
	os.MkdirAll(filepath.Join(aberta, ".rustfs.sys"), 0o755)
	os.MkdirAll(filepath.Join(aberta, "b"), 0o755)
	os.Chmod(aberta, 0o755)
	if err := fecharPastaDosDados(aberta, "b"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(aberta); fi.Mode().Perm() != 0o700 {
		t.Fatalf("a pasta da cópia local que estava aberta continuou %v", fi.Mode().Perm())
	}
	alheia := filepath.Join(base, "alheia")
	os.MkdirAll(alheia, 0o755)
	os.Chmod(alheia, 0o755)
	os.WriteFile(filepath.Join(alheia, "planilha.xlsx"), []byte("x"), 0o644)
	if err := fecharPastaDosDados(alheia, "b"); err == nil || !strings.Contains(err.Error(), "pasta nova ou vazia") {
		t.Fatalf("pasta aberta com arquivos de outro aceita: %v", err)
	}
	if fi, _ := os.Stat(alheia); fi.Mode().Perm() != 0o755 {
		t.Fatal("a recusa mexeu na permissão da pasta alheia")
	}
	// Um link com o nome do bucket, plantado numa pasta aberta, não passa.
	plantada := filepath.Join(base, "plantada")
	os.MkdirAll(plantada, 0o777)
	os.Chmod(plantada, 0o777)
	os.Symlink(t.TempDir(), filepath.Join(plantada, "b"))
	if err := fecharPastaDosDados(plantada, "b"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("link com o nome do bucket aceito: %v", err)
	}
}
