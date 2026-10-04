//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Overwrite: o temporário vai ao disco inteiro antes do rename, e a pasta
// depois dele. Sem o fsync, um corte de energia deixava o arquivo do cliente
// com 0 bytes, o original já substituído e o painel com "concluído".
func TestRestauracaoSincronizaAntesEDepoisDoRename(t *testing.T) {
	conteudo := []byte("conteúdo do backup")
	dir := t.TempDir()
	destino := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(destino, []byte("atual"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ordem []string
	arqAntes, pastaAntes := sincronizarArquivo, sincronizarPasta
	t.Cleanup(func() { sincronizarArquivo, sincronizarPasta = arqAntes, pastaAntes })
	sincronizarArquivo = func(f *os.File) error {
		b, _ := os.ReadFile(f.Name())
		atual, _ := os.ReadFile(destino)
		ordem = append(ordem, "arquivo:"+string(b)+"|destino:"+string(atual))
		return f.Sync()
	}
	sincronizarPasta = func(p *pasta) error {
		atual, _ := os.ReadFile(destino)
		ordem = append(ordem, "pasta|destino:"+string(atual))
		return p.sincronizar()
	}
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(dir, "app.conf", conteudo, "overwrite")); err != nil {
		t.Fatal(err)
	}
	quer := []string{
		"arquivo:" + string(conteudo) + "|destino:atual",
		"pasta|destino:" + string(conteudo),
	}
	if len(ordem) != len(quer) || ordem[0] != quer[0] || ordem[1] != quer[1] {
		t.Fatalf("sincronizações %q, queria %q", ordem, quer)
	}
}
