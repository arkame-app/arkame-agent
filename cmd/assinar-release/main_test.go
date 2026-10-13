package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/atualizacao"
)

// A assinatura que a release grava é a que o agente confere: mesma mensagem
// (com a versão), mesmo formato de arquivo.
func TestAssinarEVerificarComoNaRelease(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envChavePrivada, base64.StdEncoding.EncodeToString(priv.Seed()))
	t.Setenv(envChavePublica, base64.StdEncoding.EncodeToString(pub))
	embutir(t, pub)

	arquivo := filepath.Join(t.TempDir(), "checksums.txt")
	conteudo := []byte("abc  arkame-agent_linux_amd64\n")
	if err := os.WriteFile(arquivo, conteudo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rodar([]string{"assinar", "v0.5.0", arquivo}); err != nil {
		t.Fatalf("assinar: %v", err)
	}
	assinatura, err := os.ReadFile(arquivo + ".ed25519")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := atualizacao.LerVersao("0.5.0")
	if err := atualizacao.VerificarAssinatura([]ed25519.PublicKey{pub}, v, conteudo, assinatura); err != nil {
		t.Fatalf("o agente não aceita a assinatura da release: %v", err)
	}
	if err := rodar([]string{"verificar", "0.5.0", arquivo}); err != nil {
		t.Fatalf("verificar: %v", err)
	}
	// Outra versão, ou o arquivo alterado depois de assinado: não confere.
	if err := rodar([]string{"verificar", "0.5.1", arquivo}); err == nil {
		t.Fatal("a assinatura conferiu para outra versão")
	}
	_ = os.WriteFile(arquivo, append(conteudo, '\n'), 0o644)
	if err := rodar([]string{"verificar", "0.5.0", arquivo}); err == nil {
		t.Fatal("a assinatura conferiu com o arquivo alterado")
	}
}

// Sem a chave no ambiente, com a chave torta ou com a versão torta, não assina.
func TestAssinarSemChaveFalha(t *testing.T) {
	arquivo := filepath.Join(t.TempDir(), "checksums.txt")
	_ = os.WriteFile(arquivo, []byte("x"), 0o644)
	t.Setenv(envChavePrivada, "")
	if err := rodar([]string{"assinar", "0.5.0", arquivo}); err == nil {
		t.Fatal("assinou sem chave")
	}
	t.Setenv(envChavePrivada, "não é base64")
	if err := rodar([]string{"assinar", "0.5.0", arquivo}); err == nil {
		t.Fatal("assinou com chave torta")
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv(envChavePrivada, base64.StdEncoding.EncodeToString(priv.Seed()))
	if err := rodar([]string{"assinar", "snapshot", arquivo}); err == nil {
		t.Fatal("assinou uma versão que não é X.Y.Z")
	}
	if _, err := os.Stat(arquivo + ".ed25519"); err == nil {
		t.Fatal("ficou um arquivo de assinatura depois da falha")
	}
}

// `gerar` grava a chave privada num arquivo 0600 e não a imprime: no terminal
// saem só a pública e o caminho. Não grava por cima de arquivo que exista.
func TestGerarGravaAPrivadaEmArquivoENaoAImprime(t *testing.T) {
	saida := filepath.Join(t.TempDir(), "release.key")
	var tela bytes.Buffer
	if err := gerar(saida, &tela); err != nil {
		t.Fatal(err)
	}
	conteudo, err := os.ReadFile(saida)
	if err != nil {
		t.Fatal(err)
	}
	privada := strings.TrimSpace(string(conteudo))
	seed, err := base64.StdEncoding.DecodeString(privada)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("o arquivo não traz o seed Ed25519 em base64: %q", conteudo)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(saida)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("modo do arquivo da chave = %v, queria 0600", fi.Mode().Perm())
		}
	}
	if strings.Contains(tela.String(), privada) {
		t.Fatalf("a chave privada foi impressa:\n%s", tela.String())
	}
	publica := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	if !strings.Contains(tela.String(), publica) || !strings.Contains(tela.String(), saida) {
		t.Fatalf("a saída não traz a pública e o caminho:\n%s", tela.String())
	}
	// O arquivo gravado é o que o `assinar` lê do ambiente.
	t.Setenv(envChavePrivada, string(conteudo))
	t.Setenv(envChavePublica, publica)
	embutir(t, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	checksums := filepath.Join(t.TempDir(), "checksums.txt")
	_ = os.WriteFile(checksums, []byte("abc  arkame-agent_linux_amd64\n"), 0o644)
	if err := rodar([]string{"assinar", "0.5.0", checksums}); err != nil {
		t.Fatalf("assinar com a chave gerada: %v", err)
	}
	if err := rodar([]string{"verificar", "0.5.0", checksums}); err != nil {
		t.Fatalf("verificar com a pública gerada: %v", err)
	}

	// Por cima de arquivo que existe: recusa, e o arquivo fica como estava.
	if err := gerar(saida, &tela); err == nil {
		t.Fatal("gravou uma chave nova por cima da que existia")
	}
	if depois, _ := os.ReadFile(saida); string(depois) != string(conteudo) {
		t.Fatal("o arquivo da chave mudou na segunda tentativa")
	}
	// Sem --saida, não gera nada.
	for _, args := range [][]string{{"gerar"}, {"gerar", "--saida"}, {"gerar", "--saida="}, {"gerar", "arquivo"}} {
		if err := rodar(args); err == nil {
			t.Fatalf("%v gerou uma chave sem ter onde gravar", args)
		}
	}
	if s, err := saidaDoGerar([]string{"--saida=" + saida}); err != nil || s != saida {
		t.Fatalf("--saida=: %q %v", s, err)
	}
	if s, err := saidaDoGerar([]string{"--saida", saida}); err != nil || s != saida {
		t.Fatalf("--saida: %q %v", s, err)
	}
}

// embutir faz de conta, pela duração do teste, que os agentes embutem só estas
// chaves — a de produção (chaves.go) não tem privada disponível para teste.
func embutir(t *testing.T, chaves ...ed25519.PublicKey) {
	t.Helper()
	antes := chavesEmbutidas
	chavesEmbutidas = func() []ed25519.PublicKey { return chaves }
	t.Cleanup(func() { chavesEmbutidas = antes })
}

// Assinar com uma chave que os agentes não embutem publicaria um release que
// nenhum deles aceita: o `assinar` recusa e não grava a assinatura.
func TestAssinarComChaveQueOsAgentesNaoEmbutemERecusado(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	outra, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envChavePrivada, base64.StdEncoding.EncodeToString(priv.Seed()))
	embutir(t, outra)

	arquivo := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(arquivo, []byte("abc  arkame-agent_linux_amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rodar([]string{"assinar", "v0.5.0", arquivo}); err == nil {
		t.Fatal("assinou com chave que os agentes não embutem")
	}
	if _, err := os.Stat(arquivo + ".ed25519"); err == nil {
		t.Fatal("gravou a assinatura recusada")
	}
}

// A chave embutida de verdade vale: sem ela o agente não se atualiza sozinho.
func TestAChaveDeProducaoEstaEmbutida(t *testing.T) {
	if len(atualizacao.ChavesConfiaveis()) == 0 {
		t.Fatal("nenhuma chave de release embutida")
	}
}
