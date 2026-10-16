package localstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/arkame-app/agent/internal/segredo"
	"github.com/arkame-app/agent/internal/storage"
)

// Os nomes que o RustFS procura na pasta de `--tls-path`.
const (
	ArquivoDoCertificado = "rustfs_cert.pem"
	arquivoDaChaveTLS    = "rustfs_key.pem"
)

// validadeDoCertificado: longa, porque quem confia nele é só o agente desta
// máquina (o certificado é fixado, não há CA a revogar). Faltando menos de
// renovarAntes, o install gera outro.
const (
	validadeDoCertificado = 20 * 365 * 24 * time.Hour
	renovarAntes          = 365 * 24 * time.Hour
)

// PrepararTLS garante na pasta (criada só para o administrador) o certificado
// e a chave do RustFS para 127.0.0.1, e devolve o caminho do certificado. O
// que já existe e ainda vale é mantido: o processo da cópia local que está
// rodando confia nele.
//
// É o TLS que impede um usuário comum da máquina de receber o backup: a porta
// em 127.0.0.1 é de todos, e quem escutar nela antes do RustFS (no boot, ou
// na espera depois de uma queda) recebia o conteúdo de tudo o que o agente lê
// como root. Sem a chave, que fica nesta pasta, a conexão não fecha.
func PrepararTLS(pasta string) (string, error) {
	cert := filepath.Join(pasta, ArquivoDoCertificado)
	chave := filepath.Join(pasta, arquivoDaChaveTLS)
	if certificadoValido(cert) {
		// O par tem de fechar: uma queda entre gravar a chave e o certificado
		// deixava os dois trocados, e o RustFS não subia mais.
		if _, err := tls.LoadX509KeyPair(cert, chave); err == nil {
			return cert, nil
		}
	}
	if err := segredo.CriarPasta(pasta); err != nil {
		return "", err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serie, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", err
	}
	agora := time.Now()
	modelo := &x509.Certificate{
		SerialNumber:          serie,
		Subject:               pkix.Name{CommonName: "arkame-local"},
		NotBefore:             agora.Add(-time.Hour),
		NotAfter:              agora.Add(validadeDoCertificado),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // autoassinado e fixado: ele mesmo é a raiz em que o agente confia
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, modelo, modelo, &k.PublicKey, k)
	if err != nil {
		return "", err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}
	// A chave antes do certificado: um certificado novo sem a chave dele
	// derrubaria o RustFS no próximo início.
	if err := segredo.Gravar(chave, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return "", err
	}
	if err := segredo.Gravar(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return "", err
	}
	return cert, nil
}

func certificadoValido(caminho string) bool {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return false
	}
	bloco, _ := pem.Decode(b)
	if bloco == nil {
		return false
	}
	c, err := x509.ParseCertificate(bloco.Bytes)
	return err == nil && time.Until(c.NotAfter) > renovarAntes
}

// clienteFixado fala com o RustFS confiando só no certificado da pasta.
func clienteFixado(pastaTLS string) (*http.Client, error) {
	tc, err := storage.ConfigTLSFixado(filepath.Join(pastaTLS, ArquivoDoCertificado))
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}, nil
}

// HashDoArquivo é o SHA-256 do arquivo, em hexadecimal.
func HashDoArquivo(caminho string) (string, error) {
	f, err := os.Open(caminho)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxPrograma+1)); err != nil {
		return "", fmt.Errorf("lendo %s: %w", caminho, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
