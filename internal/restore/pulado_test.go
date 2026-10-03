package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Conflito com a estratégia "skip": nada é gravado, e o Run diz isso
// (ErrPulado). Antes devolvia nil, e o item ia ao painel como "complete".
func TestSkipComDestinoExistenteDevolveErrPulado(t *testing.T) {
	conteudo := []byte("conteúdo do backup")
	dir := t.TempDir()
	atual := []byte("versão atual, diferente")
	if err := os.WriteFile(filepath.Join(dir, "app.conf"), atual, 0o644); err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	opts := Options{S3: bucketContado(t, conteudo, &gets), HostRoot: "/"}

	err := Run(context.Background(), opts, itemDe(dir, "app.conf", conteudo, "skip"))
	if !errors.Is(err, ErrPulado) {
		t.Fatalf("esperava ErrPulado, veio %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app.conf")); string(b) != string(atual) {
		t.Fatalf("o skip mexeu no arquivo: %q", b)
	}
	if got := nomesEm(t, dir); len(got) != 1 {
		t.Fatalf("o skip gravou outro arquivo: %v", got)
	}

	// Sem conflito, o skip restaura normalmente.
	if err := Run(context.Background(), opts, itemDe(dir, "novo.conf", conteudo, "skip")); err != nil {
		t.Fatalf("sem conflito: %v", err)
	}
	// Já restaurado (mesmo conteúdo no destino): concluído, não pulado.
	if err := Run(context.Background(), opts, itemDe(dir, "novo.conf", conteudo, "skip")); err != nil {
		t.Fatalf("item já no destino é concluído: %v", err)
	}
}
