package daemon

import (
	"context"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// Limites do lote anunciados pelo painel (achado B6): valiam como viessem.
// `max_entries: 1` virava um POST por arquivo, e um valor enorme, um lote
// inteiro na memória. Agora valem dentro de uma faixa.
func TestLimitesDoLoteTemPisoETeto(t *testing.T) {
	c, _ := api.New(api.Options{BaseURL: "http://127.0.0.1:1", Bearer: "t"})
	casos := []struct {
		entradas, bytes         int
		queroEntradas, queroByt int
	}{
		{0, 0, 5000, 2 << 20},                // o painel não disse: os do contrato
		{5000, 2 << 20, 5000, 2 << 20},       // os do contrato
		{40, 1 << 20, 40, 1 << 20},           // dentro da faixa: vale o do painel
		{1, 1, 10, 64 << 10},                 // absurdo para baixo: o piso
		{-7, -1, 5000, 2 << 20},              // negativo: como se não tivesse dito
		{1 << 40, 1 << 40, 50_000, 16 << 20}, // absurdo para cima: o teto
	}
	for _, caso := range casos {
		env, err := novoEnviadorDeLotes(context.Background(), c, "/files",
			&api.IndexOffer{Mode: "batches", MaxEntries: caso.entradas, MaxBytes: caso.bytes}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if env.maxEntradas != caso.queroEntradas || env.maxBytes != caso.queroByt {
			t.Errorf("oferta de %d entradas / %d bytes: valendo %d / %d; queria %d / %d",
				caso.entradas, caso.bytes, env.maxEntradas, env.maxBytes, caso.queroEntradas, caso.queroByt)
		}
		env.fechar()
	}
}
