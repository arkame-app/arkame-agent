package storage

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ConfigTLSFixado confia só no certificado de arquivo: nem nas CAs do
// sistema, nem em outro. É o da cópia local — o RustFS em 127.0.0.1 com o
// certificado que o `local-storage install` gera e guarda numa pasta só do
// administrador. Qualquer outro programa da máquina pode escutar na porta (a
// 127.0.0.1 é de todos os usuários), mas não tem a chave: a conexão falha em
// vez de o agente lhe entregar o backup.
func ConfigTLSFixado(certFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("lendo o certificado da cópia local: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("o certificado da cópia local (%s) não é um PEM válido", certFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
