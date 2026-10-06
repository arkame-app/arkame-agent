package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
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
