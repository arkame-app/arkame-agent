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
	"slices"
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
// `nomes` são os endereços da rede (IP ou nome) por onde outros servidores
// chegam a ele (`local-storage share`): o certificado precisa cobrir cada um,
// senão o outro servidor recusa a conexão. Faltando algum, gera outro.
//
// É o TLS que impede um usuário comum da máquina de receber o backup: a porta
// em 127.0.0.1 é de todos, e quem escutar nela antes do RustFS (no boot, ou
// na espera depois de uma queda) recebia o conteúdo de tudo o que o agente lê
// como root. Sem a chave, que fica nesta pasta, a conexão não fecha.
func PrepararTLS(pasta string, nomes ...string) (string, error) {
	cert := filepath.Join(pasta, ArquivoDoCertificado)
	chave := filepath.Join(pasta, arquivoDaChaveTLS)
	if certificadoValido(cert) && cobreTodos(cert, nomes) {
		// O par tem de fechar: uma queda entre gravar a chave e o certificado
		// deixava os dois trocados, e o RustFS não subia mais.
		if _, err := tls.LoadX509KeyPair(cert, chave); err == nil {
			return cert, nil
		}
	}
	// O certificado novo leva também os nomes que o antigo cobria: um share
	// com outro --ip não pode tirar o endereço que os clientes já usam.
	if antigo, err := lerCertificado(cert); err == nil {
		for _, ip := range antigo.IPAddresses {
			if !ip.IsLoopback() && !slices.Contains(nomes, ip.String()) {
				nomes = append(nomes, ip.String())
			}
		}
		for _, n := range antigo.DNSNames {
			if !slices.Contains(nomes, n) {
				nomes = append(nomes, n)
			}
		}
	}
	if err := segredo.CriarPasta(pasta); err != nil {
		return "", err
	}
	// A chave que já existe continua: o certificado novo, com a mesma chave e o
	// mesmo nome, é validado pelo antigo que os clientes da rede guardaram
	// (`join`). Sem isto, um IP novo na máquina derrubava todos eles.
	k, err := chaveExistente(chave)
	if err != nil {
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", err
		}
	}
	serie, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", err
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1)}
	var dns []string
	for _, n := range nomes {
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
		} else if n != "" {
			dns = append(dns, n)
		}
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
		IPAddresses:           ips,
		DNSNames:              dns,
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

func lerCertificado(caminho string) (*x509.Certificate, error) {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return nil, err
	}
	bloco, _ := pem.Decode(b)
	if bloco == nil {
		return nil, fmt.Errorf("%s não é um PEM", caminho)
	}
	return x509.ParseCertificate(bloco.Bytes)
}

func certificadoValido(caminho string) bool {
	c, err := lerCertificado(caminho)
	return err == nil && time.Until(c.NotAfter) > renovarAntes
}

// cobreTodos: o certificado vale para cada um dos nomes (IP ou DNS).
func cobreTodos(caminho string, nomes []string) bool {
	c, err := lerCertificado(caminho)
	if err != nil {
		return false
	}
	for _, n := range nomes {
		if n != "" && c.VerifyHostname(n) != nil {
			return false
		}
	}
	return true
}

// NomesDoCertificado: os endereços da rede que o certificado cobre, fora o
// 127.0.0.1 (para o painel mostrar por onde os outros servidores chegam).
func NomesDoCertificado(pasta string) []string {
	if pasta == "" {
		return nil
	}
	c, err := lerCertificado(filepath.Join(pasta, ArquivoDoCertificado))
	if err != nil {
		return nil
	}
	var nomes []string
	for _, ip := range c.IPAddresses {
		if !ip.IsLoopback() {
			nomes = append(nomes, ip.String())
		}
	}
	return append(nomes, c.DNSNames...)
}

// ImpressaoDoCertificado é o SHA-256 do certificado (DER), em hexadecimal: o
// outro servidor confere por ela que chegou a esta cópia local, e não a outra
// máquina se passando por ela.
func ImpressaoDoCertificado(caminho string) (string, error) {
	c, err := lerCertificado(caminho)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:]), nil
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

// chaveExistente lê a chave ECDSA do RustFS, se há uma válida.
func chaveExistente(caminho string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return nil, err
	}
	bloco, _ := pem.Decode(b)
	if bloco == nil {
		return nil, fmt.Errorf("%s não é um PEM", caminho)
	}
	k, err := x509.ParsePKCS8PrivateKey(bloco.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s não é uma chave ECDSA", caminho)
	}
	return ec, nil
}
