package localstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/storage"
)

func TestArtefatoDaPlataforma(t *testing.T) {
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"} {
		os_, arch, _ := strings.Cut(p, "/")
		a, err := artefatoDe(os_, arch)
		if err != nil || len(a.SHA256) != 64 || !strings.Contains(a.Arquivo, Versao) {
			t.Fatalf("%s: %+v %v", p, a, err)
		}
	}
	if _, err := artefatoDe("windows", "arm64"); err == nil {
		t.Fatal("windows/arm64 não tem build do RustFS e devia ser recusado")
	}
}

// zipDe monta um zip com as entradas dadas.
func zipDe(t *testing.T, entradas map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for nome, conteudo := range entradas {
		f, err := w.Create(nome)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(conteudo))
	}
	w.Close()
	return b.Bytes()
}

func servirZip(t *testing.T, corpo []byte, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write(corpo)
	}))
	t.Cleanup(srv.Close)
	antes := baseDaRelease
	baseDaRelease = srv.URL + "/"
	t.Cleanup(func() { baseDaRelease = antes })
}

func TestBaixarConfereEExtraiSoOPrograma(t *testing.T) {
	corpo := zipDe(t, map[string]string{"rustfs": "PROGRAMA", "rustfs-cli": "CLI", "../fora": "X"})
	soma := sha256.Sum256(corpo)
	servirZip(t, corpo, 200)
	pasta := t.TempDir()
	a := Artefato{Arquivo: "r.zip", SHA256: hex.EncodeToString(soma[:]), Programa: "rustfs"}
	caminho, err := Baixar(context.Background(), a, pasta)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(caminho); string(b) != "PROGRAMA" {
		t.Fatalf("conteúdo extraído: %q", b)
	}
	entradas, _ := os.ReadDir(pasta)
	if len(entradas) != 1 || entradas[0].Name() != "rustfs" {
		t.Fatalf("só o programa devia ficar na pasta (sem o zip, sem o cli, nada fora): %v", entradas)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(pasta), "fora")); err == nil {
		t.Fatal("a entrada com .. foi extraída fora da pasta")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(caminho); fi.Mode().Perm()&0o111 == 0 {
			t.Fatal("o programa extraído não é executável")
		}
	}
}

func TestBaixarRecusaHashErradoESemPrograma(t *testing.T) {
	corpo := zipDe(t, map[string]string{"rustfs": "PROGRAMA"})
	servirZip(t, corpo, 200)
	pasta := t.TempDir()
	if _, err := Baixar(context.Background(), Artefato{Arquivo: "r.zip", SHA256: strings.Repeat("0", 64), Programa: "rustfs"}, pasta); err == nil || !strings.Contains(err.Error(), "não confere") {
		t.Fatalf("hash errado aceito: %v", err)
	}
	if e, _ := os.ReadDir(pasta); len(e) != 0 {
		t.Fatalf("o download recusado deixou arquivo: %v", e)
	}
	soma := sha256.Sum256(corpo)
	if _, err := Baixar(context.Background(), Artefato{Arquivo: "r.zip", SHA256: hex.EncodeToString(soma[:]), Programa: "rustfs.exe"}, pasta); err == nil {
		t.Fatal("zip sem o programa aceito")
	}
	servirZip(t, nil, 404)
	if _, err := Baixar(context.Background(), Artefato{Arquivo: "r.zip", SHA256: hex.EncodeToString(soma[:]), Programa: "rustfs"}, pasta); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 aceito: %v", err)
	}
}

func TestAmbienteDoFilhoNaoLevaAChaveDoBucket(t *testing.T) {
	t.Setenv("STORAGE_SECRET_KEY", "segredo-do-bucket")
	t.Setenv("STORAGE_ACCESS_KEY", "chave")
	t.Setenv("PATH", os.Getenv("PATH"))
	env := strings.Join(ambienteDoFilho(), "\n")
	if strings.Contains(env, "segredo-do-bucket") || strings.Contains(env, "STORAGE_") {
		t.Fatalf("o ambiente do RustFS leva a chave do bucket: %s", env)
	}
	if !strings.Contains(env, "PATH=") || !strings.Contains(env, "RUSTFS_CONSOLE_ENABLE=false") {
		t.Fatalf("faltou PATH ou o console desligado: %s", env)
	}
}

// Um programa que sai na hora: o supervisor sobe de novo, com espera.
func TestSupervisorSobeDeNovoQuandoCai(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("o programa falso é um script de shell")
	}
	pasta := t.TempDir()
	contador := filepath.Join(pasta, "vezes")
	script := filepath.Join(pasta, "falso")
	os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+contador+"\nexit 1\n"), 0o755)
	antes := esperaMinima
	esperaMinima = 200 * time.Millisecond
	t.Cleanup(func() { esperaMinima = antes })
	s := &Supervisor{Programa: script, Dados: pasta, Endereco: portaLivre(t), ArquivoDaChave: "a", ArquivoDoSegredo: "b", PastaTLS: tlsDeTeste(t), SHA256: hashDe(t, script)}
	ctx, cancelar := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancelar()
	s.Rodar(ctx)
	b, _ := os.ReadFile(contador)
	if n := strings.Count(string(b), "x"); n < 3 || n > 6 {
		t.Fatalf("subiu %d vezes em 1,5 s com espera de 200 ms dobrando: esperado 3 a 6", n)
	}
}

func tlsDeTeste(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tls")
	if _, err := PrepararTLS(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func hashDe(t *testing.T, p string) string {
	t.Helper()
	h, err := HashDoArquivo(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Outro programa na porta (de outro usuário da máquina) não é a cópia local:
// nem em texto puro, nem com um certificado que não é o dela. Adotá-lo
// entregaria o backup inteiro a ele.
func TestServidorAlheioNaPortaNaoEAdotado(t *testing.T) {
	saude := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for nome, srv := range map[string]*httptest.Server{"http": httptest.NewServer(saude), "https com outro certificado": httptest.NewTLSServer(saude)} {
		t.Cleanup(srv.Close)
		s := &Supervisor{Endereco: strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"), PastaTLS: tlsDeTeste(t)}
		if s.Responde(context.Background()) {
			t.Errorf("%s: o supervisor adotou um servidor que não tem a chave da cópia local", nome)
		}
	}
}

// O programa roda como root/SYSTEM: trocado depois da instalação, não sobe.
func TestProgramaTrocadoNaoSobe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("o programa falso é um script de shell")
	}
	pasta := t.TempDir()
	script := filepath.Join(pasta, "rustfs")
	marca := filepath.Join(pasta, "rodou")
	os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	s := &Supervisor{Programa: script, Dados: pasta, Endereco: portaLivre(t), PastaTLS: tlsDeTeste(t), SHA256: hashDe(t, script)}
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marca+"\n"), 0o755)
	err := s.uma(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mudou desde a instalação") {
		t.Fatalf("programa trocado aceito: %v", err)
	}
	if _, err := os.Stat(marca); err == nil {
		t.Fatal("o programa trocado chegou a rodar")
	}
	s.SHA256 = ""
	if err := s.uma(context.Background()); err == nil {
		t.Fatal("sem SHA-256 registrado, subiu assim mesmo")
	}
	s.SHA256, s.PastaTLS = hashDe(t, script), ""
	if err := s.uma(context.Background()); err == nil {
		t.Fatal("sem TLS, subiu assim mesmo")
	}
}

// O log não segue link simbólico (o agente roda como root).
func TestLogNaoSegueLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sem O_NOFOLLOW")
	}
	pasta := t.TempDir()
	alvo := filepath.Join(pasta, "do-sistema")
	os.WriteFile(alvo, []byte("importante"), 0o644)
	link := filepath.Join(pasta, "rustfs.log")
	os.Symlink(alvo, link)
	if f, err := abrirLog(link); err == nil {
		f.Close()
		t.Fatal("o log abriu através de um link simbólico")
	}
	if b, _ := os.ReadFile(alvo); string(b) != "importante" {
		t.Fatal("o alvo do link foi mexido")
	}
}

// O certificado: vale para 127.0.0.1, e reinstalar mantém o mesmo.
func TestPrepararTLSMantemOCertificado(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tls")
	c1, err := PrepararTLS(p)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(c1)
	c2, err := PrepararTLS(p)
	if err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(c2); string(b1) != string(b2) {
		t.Fatal("reinstalar trocou o certificado (o processo da cópia local que está rodando deixaria de confiar)")
	}
	// Uma chave que não é a do certificado (queda no meio da gravação): gera outro par.
	outra := filepath.Join(t.TempDir(), "outra")
	PrepararTLS(outra)
	chaveOutra, _ := os.ReadFile(filepath.Join(outra, "rustfs_key.pem"))
	os.WriteFile(filepath.Join(p, "rustfs_key.pem"), chaveOutra, 0o600)
	if _, err := PrepararTLS(p); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(p, ArquivoDoCertificado), filepath.Join(p, "rustfs_key.pem")); err != nil {
		t.Fatalf("o par trocado foi mantido: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(p, "rustfs_key.pem")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("chave TLS com modo %v", fi.Mode().Perm())
		}
	}
}

func portaLivre(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return "127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// Com o RustFS de verdade (ARKAME_TESTE_RUSTFS=<programa>): sobe, responde,
// o bucket fica versionado, e para quando o contexto acaba. Um segundo
// supervisor no mesmo endereço adota o que está de pé em vez de brigar pela porta.
func TestSupervisorComORustFSDeVerdade(t *testing.T) {
	programa := os.Getenv("ARKAME_TESTE_RUSTFS")
	if programa == "" {
		t.Skip("ARKAME_TESTE_RUSTFS não definido")
	}
	pasta := t.TempDir()
	os.MkdirAll(filepath.Join(pasta, "dados"), 0o700)
	os.WriteFile(filepath.Join(pasta, "ak"), []byte("arkametesteak1234567"), 0o600)
	os.WriteFile(filepath.Join(pasta, "sk"), []byte("segredo-de-teste-da-copia-local-40-carac"), 0o600)
	pastaTLS := tlsDeTeste(t)
	s := &Supervisor{Programa: programa, Dados: filepath.Join(pasta, "dados"), Endereco: portaLivre(t), ArquivoDaChave: filepath.Join(pasta, "ak"), ArquivoDoSegredo: filepath.Join(pasta, "sk"), Log: filepath.Join(pasta, "rustfs.log"), PastaTLS: pastaTLS, SHA256: hashDe(t, programa)}
	ctx, cancelar := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { s.Rodar(ctx); close(fim) }()
	if err := s.Pronto(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StorageAccessKey: "arkametesteak1234567", StorageSecretKey: "segredo-de-teste-da-copia-local-40-carac", StorageEndpoint: "https://" + s.Endereco, StorageCAFile: filepath.Join(pastaTLS, ArquivoDoCertificado), StorageRegion: "us-east-1"}
	c, err := storage.NewS3Client(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// O que a segurança da cópia local supõe do RustFS 1.0.1: sem texto puro
	// na porta, sem console em :9001, e a credencial de fábrica recusada.
	if res, err := http.Get("http://" + s.Endereco + "/health"); err == nil {
		res.Body.Close()
		t.Fatal("o RustFS respondeu em texto puro")
	}
	if l, err := net.DialTimeout("tcp", "127.0.0.1:9001", time.Second); err == nil {
		l.Close()
		t.Log("aviso: há algo em 127.0.0.1:9001 (conferir se não é o console deste RustFS)")
	}
	padrao := *cfg
	padrao.StorageAccessKey, padrao.StorageSecretKey = "rustfsadmin", "rustfsadmin"
	if cp, err := storage.NewS3Client(context.Background(), &padrao); err == nil {
		if _, err := cp.ListBuckets(context.Background(), nil); err == nil {
			t.Fatal("a credencial de fábrica do RustFS (rustfsadmin) foi aceita")
		}
	}
	semCA := *cfg
	semCA.StorageCAFile = ""
	if cs, err := storage.NewS3Client(context.Background(), &semCA); err == nil {
		if _, err := cs.ListBuckets(context.Background(), nil); err == nil {
			t.Fatal("um cliente sem o certificado fixado falou com o RustFS (o certificado devia ser só da cópia local)")
		}
	}
	for i := 0; i < 2; i++ { // duas vezes: idempotente
		if err := PrepararBucket(context.Background(), c, "arkame-local-teste"); err != nil {
			t.Fatal(err)
		}
	}
	// O segundo supervisor adota: roda 2 s e não sobe outro (a porta já está tomada; subir daria erro no log dele).
	s2 := *s
	s2.Log = filepath.Join(pasta, "segundo.log")
	ctx2, cancelar2 := context.WithTimeout(context.Background(), 2*time.Second)
	s2.Rodar(ctx2)
	cancelar2()
	if fi, err := os.Stat(s2.Log); err == nil && fi.Size() > 0 {
		t.Fatal("o segundo supervisor subiu outro RustFS em vez de adotar o que responde")
	}
	cancelar()
	select {
	case <-fim:
	case <-time.After(20 * time.Second):
		t.Fatal("o supervisor não parou o RustFS em 20 s")
	}
	if s.Responde(context.Background()) {
		t.Fatal("o RustFS continuou de pé depois de o agente parar")
	}
}
