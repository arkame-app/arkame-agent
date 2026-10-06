//go:build !release

package atualizacao

import (
	"encoding/base64"
	"testing"
)

// As chaves embutidas: vazias não valem; a de teste, quando habilitada no
// build de desenvolvimento, é a única aceita.
func TestChavesConfiaveis(t *testing.T) {
	antes := chavePublicaDeTeste
	t.Cleanup(func() { chavePublicaDeTeste = antes })

	chavePublicaDeTeste = ""
	producao := 0
	for _, c := range []string{chavePublicaAtual, chavePublicaProxima} {
		if c != "" {
			producao++
		}
	}
	if got := len(ChavesConfiaveis()); got != producao {
		t.Fatalf("sem chave de teste: %d chaves, queria as %d de produção preenchidas", got, producao)
	}

	pub, _ := chavesDeTesteGeradas(t)
	chavePublicaDeTeste = base64.StdEncoding.EncodeToString(pub)
	chaves := ChavesConfiaveis()
	if len(chaves) != 1 || !chaves[0].Equal(pub) {
		t.Fatalf("com a chave de teste: %d chaves", len(chaves))
	}

	chavePublicaDeTeste = "isto não é uma chave"
	if got := len(ChavesConfiaveis()); got != 0 {
		t.Fatalf("chave de teste inválida deixou %d chaves valendo", got)
	}
	for _, ruim := range []string{"", "abc", base64.StdEncoding.EncodeToString([]byte("curta"))} {
		if _, err := LerChavePublica(ruim); err == nil {
			t.Errorf("LerChavePublica(%q) aceitou", ruim)
		}
	}
}
