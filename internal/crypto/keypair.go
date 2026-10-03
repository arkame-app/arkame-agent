// Package crypto encapsula geração de keypair Ed25519 e fingerprint.
//
// Decisão arquitetural (PLAN.md round 14):
//   - Ed25519 keypair é POR AGENTE (não por tenant).
//   - Chave privada fica em /etc/arkame/key.pem (permissão 600).
//   - Em re-enrollment, uma nova keypair é gerada — isolamento maior.
package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/arkame-app/agent/internal/segredo"
)

// Keypair representa um par Ed25519.
type Keypair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// Generate cria um novo keypair Ed25519.
func Generate() (*Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gerando keypair ed25519: %w", err)
	}
	return &Keypair{Public: pub, Private: priv}, nil
}

// SaveToDisk grava a private key em PEM, legível só pelo administrador (0600;
// no Windows, Administradores e SYSTEM — o 0600 sozinho não restringe nada lá).
func (k *Keypair) SaveToDisk(privatePath string) error {
	der, err := x509.MarshalPKCS8PrivateKey(k.Private)
	if err != nil {
		return err
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	pemData := pem.EncodeToMemory(block)
	return segredo.Gravar(privatePath, pemData, 0o600)
}

// LoadPrivate carrega keypair a partir de um arquivo PEM.
func LoadPrivate(path string) (*Keypair, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("PEM inválido em %s", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("tipo de chave inesperado: %T (esperava ed25519)", key)
	}
	return &Keypair{Public: priv.Public().(ed25519.PublicKey), Private: priv}, nil
}

// Sign assina o payload com a chave privada.
func (k *Keypair) Sign(payload []byte) []byte {
	return ed25519.Sign(k.Private, payload)
}
