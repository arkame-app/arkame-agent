package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// O caso que a vitrine promete, do começo ao fim: o comando de antes gera um
// arquivo, o backup copiaria esse arquivo, e o de depois o remove.
func TestCicloCompletoComoNoCasoDeBanco(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "app.sql")

	// antes: gera o "dump"
	if _, err := Run(context.Background(), "echo 'CREATE TABLE x;' > "+dump, 10*time.Second); err != nil {
		t.Fatalf("comando de antes falhou: %v", err)
	}
	conteudo, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("o dump não foi criado: %v", err)
	}
	if !strings.Contains(string(conteudo), "CREATE TABLE") {
		t.Fatalf("dump com conteúdo errado: %q", conteudo)
	}

	// depois: limpa
	if _, err := Run(context.Background(), "rm -f "+dump, 10*time.Second); err != nil {
		t.Fatalf("comando de depois falhou: %v", err)
	}
	if _, err := os.Stat(dump); !os.IsNotExist(err) {
		t.Fatal("o dump ficou para trás — o disco do cliente encheria com o tempo")
	}
}

// A regra mais importante: comando de antes que falha precisa ser distinguível,
// porque é ela que decide abortar o backup em vez de salvar o dump da véspera.
func TestFalhaDoComandoDeAntesEhDetectavel(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "app.sql")

	// Simula o pg_dump que não conecta: não gera arquivo e sai com erro.
	r, err := Run(context.Background(), "echo 'could not connect to server' >&2; exit 2", 10*time.Second)
	if err == nil {
		t.Fatal("o erro precisa chegar: sem ele o backup seguiria e salvaria o dump antigo")
	}
	if r.ExitCode != 2 {
		t.Fatalf("código de saída perdido: %d", r.ExitCode)
	}
	if !strings.Contains(r.Output, "could not connect") {
		t.Fatalf("a mensagem do banco não chegou ao painel: %q", r.Output)
	}
	if _, err := os.Stat(dump); !os.IsNotExist(err) {
		t.Fatal("não devia haver dump")
	}
}
