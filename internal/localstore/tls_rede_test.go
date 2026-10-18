package localstore

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

// O certificado passa a cobrir os endereços da rede; pedir de novo os mesmos
// não troca o certificado (os clientes já conferidos continuam valendo).
func TestCertificadoCobreOsEnderecosDaRede(t *testing.T) {
	dir := t.TempDir()
	cert, err := PrepararTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	antes, _ := ImpressaoDoCertificado(cert)
	if len(NomesDoCertificado(dir)) != 0 {
		t.Fatal("o certificado só local já cobria a rede")
	}
	if _, err := PrepararTLS(dir, "192.168.1.10", "backup.empresa.lan"); err != nil {
		t.Fatal(err)
	}
	depois, _ := ImpressaoDoCertificado(filepath.Join(dir, ArquivoDoCertificado))
	if depois == antes {
		t.Fatal("o certificado não foi refeito com os endereços da rede")
	}
	nomes := NomesDoCertificado(dir)
	if len(nomes) != 2 || nomes[0] != "192.168.1.10" || nomes[1] != "backup.empresa.lan" {
		t.Fatalf("nomes: %v", nomes)
	}
	if !cobreTodos(filepath.Join(dir, ArquivoDoCertificado), []string{"127.0.0.1", "192.168.1.10", "backup.empresa.lan"}) {
		t.Fatal("o certificado não cobre os endereços pedidos")
	}
	if _, err := PrepararTLS(dir, "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if mesmo, _ := ImpressaoDoCertificado(filepath.Join(dir, ArquivoDoCertificado)); mesmo != depois {
		t.Fatal("pedir um endereço que já está coberto trocou o certificado")
	}
}

// Um share com outro endereço refaz o certificado, mas sem tirar os que os
// clientes já usam.
func TestCertificadoNovoMantemOsEnderecosAntigos(t *testing.T) {
	dir := t.TempDir()
	if _, err := PrepararTLS(dir, "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepararTLS(dir, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	if !cobreTodos(filepath.Join(dir, ArquivoDoCertificado), []string{"192.168.1.10", "10.0.0.5", "127.0.0.1"}) {
		t.Fatalf("o certificado novo perdeu um endereço antigo: %v", NomesDoCertificado(dir))
	}
}

// O certificado refeito com um endereço novo continua valendo para o cliente
// que guardou o antigo: mesma chave, mesmo nome, e o antigo valida o novo.
func TestCertificadoNovoValeParaQuemGuardouOAntigo(t *testing.T) {
	dir := t.TempDir()
	cert, err := PrepararTLS(dir, "192.168.1.10")
	if err != nil {
		t.Fatal(err)
	}
	antigo, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepararTLS(dir, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	novo, err := lerCertificado(cert)
	if err != nil {
		t.Fatal(err)
	}
	raizes := x509.NewCertPool()
	if !raizes.AppendCertsFromPEM(antigo) {
		t.Fatal("PEM antigo inválido")
	}
	if _, err := novo.Verify(x509.VerifyOptions{Roots: raizes, DNSName: "10.0.0.5"}); err != nil {
		t.Fatalf("o cliente com o certificado antigo recusa o novo: %v", err)
	}
}
