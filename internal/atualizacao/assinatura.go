package atualizacao

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ArquivoDeChecksums e ArquivoDaAssinatura são os nomes no release.
const (
	ArquivoDeChecksums  = "checksums.txt"
	ArquivoDaAssinatura = "checksums.txt.ed25519"
)

// prefixoDaAssinatura separa esta assinatura de qualquer outro uso da chave.
const prefixoDaAssinatura = "arkame-agent-release\n"

var (
	// ErrAssinatura: o checksums.txt não foi assinado por uma chave que este
	// programa conhece — ou foi assinado para outra versão.
	ErrAssinatura = errors.New("a assinatura do checksums.txt não confere com as chaves deste programa")
	// ErrSemChaves: este programa não tem chave de release embutida.
	ErrSemChaves = errors.New("este programa não tem chave de release embutida e não confia em atualização nenhuma")
)

// MensagemAssinada é o que a chave de release assina: o prefixo, a versão e o
// conteúdo do checksums.txt.
//
// A versão entra porque o checksums.txt não a traz (só resumo e nome de
// arquivo). Sem ela, quem controlasse o painel e o endereço de download
// servia o checksums.txt — com a assinatura válida — de um release antigo e
// vulnerável, dizendo que era a versão nova: a conferência de "versão maior"
// passava e o agente voltava atrás sozinho.
func MensagemAssinada(versao Versao, checksums []byte) []byte {
	m := make([]byte, 0, len(prefixoDaAssinatura)+16+len(checksums))
	m = append(m, prefixoDaAssinatura...)
	m = append(m, versao.String()...)
	m = append(m, '\n')
	return append(m, checksums...)
}

// Assinar devolve o conteúdo do checksums.txt.ed25519: a assinatura em base64,
// numa linha. É o que o release faz (cmd/assinar-release) e o que os testes
// fazem com a chave deles.
func Assinar(chave ed25519.PrivateKey, versao Versao, checksums []byte) []byte {
	sig := ed25519.Sign(chave, MensagemAssinada(versao, checksums))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// VerificarAssinatura confere a assinatura do checksums.txt da versão contra
// as chaves dadas. Basta uma delas conferir.
func VerificarAssinatura(chaves []ed25519.PublicKey, versao Versao, checksums, assinatura []byte) error {
	if len(chaves) == 0 {
		return ErrSemChaves
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(assinatura)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w (o arquivo de assinatura não tem uma assinatura Ed25519)", ErrAssinatura)
	}
	msg := MensagemAssinada(versao, checksums)
	for _, c := range chaves {
		if ed25519.Verify(c, msg, sig) {
			return nil
		}
	}
	return ErrAssinatura
}

// ResumoNoChecksums devolve o SHA-256 (hex minúsculo) da linha de nome no
// checksums.txt do GoReleaser ("<sha256>  <arquivo>").
func ResumoNoChecksums(checksums []byte, nome string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		if len(campos) != 2 || strings.TrimPrefix(campos[1], "*") != nome {
			continue
		}
		resumo := strings.ToLower(campos[0])
		if b, err := hex.DecodeString(resumo); err != nil || len(b) != 32 {
			return "", fmt.Errorf("a linha de %s no checksums.txt não tem um SHA-256", nome)
		}
		return resumo, nil
	}
	return "", fmt.Errorf("%s não está no checksums.txt", nome)
}
