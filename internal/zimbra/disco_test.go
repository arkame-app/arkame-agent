//go:build !windows

package zimbra

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Os testes não dependem do disco de quem os roda: um disco grande e vazio,
// menos nos que trocam a medida.
func init() {
	espacoNoDisco = func(string) (uint64, uint64, error) { return 900 << 30, 1000 << 30, nil }
}

func trocarDisco(t *testing.T, f func(string) (uint64, uint64, error)) {
	t.Helper()
	antes, intervalo := espacoNoDisco, intervaloDaGuarda
	espacoNoDisco, intervaloDaGuarda = f, 10*time.Millisecond
	t.Cleanup(func() { espacoNoDisco, intervaloDaGuarda = antes, intervalo })
}

// Zimbra com Let's Encrypt (Zimbra Adubos, 17/10): a porta de administração
// apresenta o certificado comercial, não o autoassinado da instalação.
func TestConectarComOCertificadoComercial(t *testing.T) {
	z := novoZimbra(t)
	chave, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	modelo := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "zimbra"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, modelo, modelo, &chave.PublicKey, chave)
	ssl := filepath.Join(z.pasta, "ssl", "zimbra")
	// O autoassinado é outro; o que a porta apresenta está no comercial.
	os.WriteFile(filepath.Join(ssl, "server", "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.MkdirAll(filepath.Join(ssl, "commercial"), 0o755)
	os.WriteFile(filepath.Join(ssl, "commercial", "commercial.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: z.srv.Certificate().Raw}), 0o644)
	if _, err := Conectar(context.Background(), z.pasta); err != nil {
		t.Fatalf("recusou o certificado comercial instalado: %v", err)
	}
	// Sem o comercial, o de antes: recusa, sem mandar a senha.
	os.Remove(filepath.Join(ssl, "commercial", "commercial.crt"))
	antes := z.contar("AuthRequest")
	if _, err := Conectar(context.Background(), z.pasta); err == nil {
		t.Fatal("aceitou um certificado que não está em nenhum arquivo")
	}
	if z.contar("AuthRequest") != antes {
		t.Fatal("a senha foi enviada antes de conferir o certificado")
	}
}

// Sem espaço já no começo: nem começa, e diz por quê.
func TestExportacaoSemEspacoNemComeca(t *testing.T) {
	trocarDisco(t, func(string) (uint64, uint64, error) { return 4 << 30, 49 << 30, nil })
	z := servidorDeExemplo(t)
	_, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: filepath.Join(t.TempDir(), "zimbra"), Agora: agora}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "sem espaço") {
		t.Fatalf("exportou sem espaço no disco: %v", err)
	}
	if z.contar("GetAccountRequest") != 0 {
		t.Fatal("começou a exportar sem espaço")
	}
}

// O disco chega ao limite no meio: a exportação para, a conta fica pendente
// (sem marcador de falha nem sessão sem avanço) e, se nada avançou, o comando
// falha dizendo que não coube.
func TestExportacaoParaAntesDeEncherODisco(t *testing.T) {
	var chamadas atomic.Int64
	// 100 GB de disco (reserva de 10 GB), 30 livres: o limite é 10 GB. Cada
	// medida depois da primeira acha 3 GB a menos.
	trocarDisco(t, func(string) (uint64, uint64, error) {
		n := chamadas.Add(1) - 1
		return uint64(30<<30) - uint64(n)*(3<<30), 100 << 30, nil
	})
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	z.mu.Lock()
	z.atraso = 100 * time.Millisecond
	z.mu.Unlock()
	pasta := filepath.Join(t.TempDir(), "zimbra")
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}, io.Discard)
	z.mu.Lock()
	z.atraso = 0
	z.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "não coube no disco") {
		t.Fatalf("a exportação que não coube não falhou: %v", err)
	}
	if inv == nil || inv.Pendentes != 1 || inv.Falhas != 0 || inv.Contas[0].SemAvanco != 0 {
		t.Fatalf("a conta parada pelo disco devia ficar pendente: %+v", inv)
	}
	if _, err := os.Stat(filepath.Join(pasta, "contas", "ana@exemplo.com.br", MarcadorDeFalha)); err == nil {
		t.Fatal("a conta parada pelo disco ficou marcada como falha")
	}
}
