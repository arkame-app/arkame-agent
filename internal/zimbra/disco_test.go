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

// Sem espaço já no começo: a primeira vez é erro (o plano subiria vazio); com
// exportação anterior, não exporta e o backup leva a anterior (falhar aqui
// derrubaria o plano todo, todo dia).
func TestExportacaoSemEspacoNemComeca(t *testing.T) {
	trocarDisco(t, func(string) (uint64, uint64, error) { return 4 << 30, 49 << 30, nil })
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	_, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "sem espaço") {
		t.Fatalf("a primeira exportação sem espaço não falhou: %v", err)
	}
	if z.contar("GetAccountRequest") != 0 {
		t.Fatal("começou a exportar sem espaço")
	}
	// Um inventário sem nenhuma exportação completa ainda é "nunca exportou".
	os.WriteFile(filepath.Join(pasta, "inventario.json"), []byte(`{"contas":[{"conta":"ana@exemplo.com.br"}]}`), 0o600)
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard); err == nil {
		t.Fatal("sem nenhuma exportação completa, seguiu como se houvesse")
	}
	os.WriteFile(filepath.Join(pasta, "inventario.json"), []byte(`{"contas":[{"conta":"ana@exemplo.com.br","ultimo_sucesso":"2026-10-10T00:00:00Z"}]}`), 0o600)
	for sessao := 1; sessao <= maxSemAvanco; sessao++ {
		var log strings.Builder
		inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, &log)
		if sessao < maxSemAvanco && (err != nil || inv == nil || len(inv.Contas) != 1 || !strings.Contains(log.String(), "NÃO EXPORTEI")) {
			t.Fatalf("sessão %d: com exportação anterior, devia seguir sem exportar: %v %+v %q", sessao, err, inv, log.String())
		}
		// Seguidas, viram erro: senão o plano ficaria verde para sempre com e-mail velho.
		if sessao == maxSemAvanco && (err == nil || !strings.Contains(err.Error(), "sessões")) {
			t.Fatalf("sessão %d sem disco seguida não virou erro: %v", sessao, err)
		}
	}
	if z.contar("GetAccountRequest") != 0 {
		t.Fatal("exportou sem espaço")
	}
}

// Disco grande: a reserva tem teto (20 GiB), senão 150 GB livres num disco de
// 2 TB não exportariam nada.
func TestReservaTemTeto(t *testing.T) {
	if r := reservaDoDisco(2000 << 30); r != 20<<30 {
		t.Fatalf("reserva de um disco de 2 TB = %d", r)
	}
	if r := reservaDoDisco(20 << 30); r != 5<<30 {
		t.Fatalf("reserva de um disco de 20 GB = %d", r)
	}
}

// Servidor em dia: a parada pelo disco não derruba o backup do dia, que leva
// a exportação anterior.
func TestParadaDepoisDeReexportarNaoFalha(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	pasta := filepath.Join(t.TempDir(), "zimbra")
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Segunda sessão: a guarda dispara durante o mês corrente (cada mês leva
	// 200 ms; o limite de 10 GB vence em ~40 ms), sem nada novo exportado.
	var chamadas atomic.Int64
	trocarDisco(t, func(string) (uint64, uint64, error) {
		n := chamadas.Add(1) - 1
		return uint64(30<<30) - uint64(n)*(3<<30), 100 << 30, nil
	})
	z.mu.Lock()
	z.atraso = 200 * time.Millisecond
	z.mu.Unlock()
	var log strings.Builder
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}, &log)
	z.mu.Lock()
	z.atraso = 0
	z.mu.Unlock()
	if !strings.Contains(log.String(), "PAROU") {
		t.Fatalf("a guarda não disparou: o teste não testa nada (%q)", log.String())
	}
	if err != nil {
		t.Fatalf("com a exportação anterior na pasta, a parada pelo disco derrubou o backup: %v", err)
	}
	if inv == nil || inv.Pendentes != 1 {
		t.Fatalf("a conta devia ficar pendente para a próxima sessão: %+v", inv)
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
	// Pendente, contando uma sessão sem avanço: a conta que nunca cabe vira
	// falha depois de algumas, em vez de ficar pendente para sempre.
	if inv == nil || inv.Pendentes != 1 || inv.Falhas != 0 || inv.Contas[0].SemAvanco != 1 {
		t.Fatalf("a conta parada pelo disco devia ficar pendente: %+v", inv)
	}
	if _, err := os.Stat(filepath.Join(pasta, "contas", "ana@exemplo.com.br", MarcadorDeFalha)); err == nil {
		t.Fatal("a conta parada pelo disco ficou marcada como falha")
	}
}

// Um mês antigo que nunca cabe, numa caixa que recebe e-mail todo dia: o mês
// corrente reexportado não é avanço, e a conta vira falha depois de algumas
// sessões, em vez de ficar pendente para sempre com o backup verde (revisão,
// 17/10; cenário montado pelo revisor).
func TestMesQueNuncaCabeViraFalha(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	pasta := filepath.Join(t.TempDir(), "zimbra")
	z.lento(200*time.Millisecond, consultaDoMes(mes(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))))
	var inv *Inventario
	var err error
	for s := 1; s <= maxSemAvanco; s++ {
		var chamadas atomic.Int64
		trocarDisco(t, func(string) (uint64, uint64, error) {
			n := chamadas.Add(1) - 1
			return uint64(30<<30) - uint64(n)*(3<<30), 100 << 30, nil
		})
		z.mu.Lock()
		z.contas["ana@exemplo.com.br"].mensagens = append(z.contas["ana@exemplo.com.br"].mensagens, time.Date(2026, 10, 11, 0, s, 0, 0, time.UTC))
		z.mu.Unlock()
		inv, err = Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}, io.Discard)
	}
	if err == nil && (inv == nil || inv.Falhas == 0) {
		t.Fatalf("depois de %d sessões sem caber, nem falha da conta nem erro: %+v", maxSemAvanco, inv)
	}
}
