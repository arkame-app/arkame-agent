//go:build release

package atualizacao

import "testing"

// No build de release a chave de produção está preenchida, decodifica, e só
// as de produção valem (não há chave de teste). Roda na release, antes de
// publicar: `go test -tags release ./internal/atualizacao/`.
func TestChaveDeProducaoPreenchida(t *testing.T) {
	if _, err := LerChavePublica(chavePublicaAtual); err != nil {
		t.Fatalf("chavePublicaAtual: %v", err)
	}
	quer := 1
	if chavePublicaProxima != "" {
		if _, err := LerChavePublica(chavePublicaProxima); err != nil {
			t.Fatalf("chavePublicaProxima: %v", err)
		}
		if chavePublicaProxima == chavePublicaAtual {
			t.Fatal("chavePublicaProxima é igual à atual: a posição da próxima existe para a chave seguinte")
		}
		quer = 2
	}
	if got := len(ChavesConfiaveis()); got != quer {
		t.Fatalf("%d chaves valendo no release, queria %d", got, quer)
	}
}
