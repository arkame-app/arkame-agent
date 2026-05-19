package crypto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"strings"
)

// Fingerprint retorna SHA-256 do public_key no formato "AA:BB:CC:...:FF" (uppercase hex, pares separados por :).
// Esse é o formato que o usuário vê no painel para aprovar o agente.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
