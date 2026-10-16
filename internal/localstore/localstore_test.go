package localstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
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
	s := &Supervisor{Programa: script, Dados: pasta, Endereco: portaLivre(t), ArquivoDaChave: "a", ArquivoDoSegredo: "b"}
	ctx, cancelar := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancelar()
	s.Rodar(ctx)
	b, _ := os.ReadFile(contador)
	if n := strings.Count(string(b), "x"); n < 3 || n > 6 {
		t.Fatalf("subiu %d vezes em 1,5 s com espera de 200 ms dobrando: esperado 3 a 6", n)
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
	s := &Supervisor{Programa: programa, Dados: filepath.Join(pasta, "dados"), Endereco: portaLivre(t), ArquivoDaChave: filepath.Join(pasta, "ak"), ArquivoDoSegredo: filepath.Join(pasta, "sk"), Log: filepath.Join(pasta, "rustfs.log")}
	ctx, cancelar := context.WithCancel(context.Background())
	fim := make(chan struct{})
	go func() { s.Rodar(ctx); close(fim) }()
	if err := s.Pronto(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StorageAccessKey: "arkametesteak1234567", StorageSecretKey: "segredo-de-teste-da-copia-local-40-carac", StorageEndpoint: "http://" + s.Endereco, StorageRegion: "us-east-1"}
	c, err := storage.NewS3Client(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
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
