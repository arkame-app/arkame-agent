package daemon

import (
	"testing"

	"github.com/arkame-app/agent/internal/service"
)

// O Reiniciar e o Parar do Windows esperam o serviço parar por
// service.EsperaParada. Depois do Stop, a finalização ainda roda até três
// etapas de finalizacaoGraca em sequência; se a espera for menor, o reinício
// da atualização desiste antes de o serviço parar, e ele para sozinho depois,
// sem ninguém o iniciar.
func TestEsperaParadaCobreAFinalizacao(t *testing.T) {
	if min := 3*finalizacaoGraca + finalizacaoGraca/2; service.EsperaParada < min {
		t.Fatalf("service.EsperaParada = %s, menor que 3 × finalizacaoGraca + folga (%s)", service.EsperaParada, min)
	}
}
