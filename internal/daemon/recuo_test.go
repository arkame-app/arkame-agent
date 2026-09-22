package daemon

import (
	"testing"
	"time"
)

// Sem recuo, doze horas de Deep Archive davam 720 conferências por item, cada
// uma com dois chamados à S3 e dois PATCH no painel.
func TestAgendarRecuoDobraAteOTeto(t *testing.T) {
	m := map[string]*esperaDeAquecimento{}

	agendarRecuo(m, "item-1")
	if m["item-1"].recuo != recuoInicial {
		t.Fatalf("primeira conferência: recuo %v, queria %v", m["item-1"].recuo, recuoInicial)
	}
	if !m["item-1"].proxima.After(time.Now()) {
		t.Error("a próxima conferência deveria ficar no futuro")
	}

	esperados := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, recuoMaximo, recuoMaximo, recuoMaximo}
	for i, querido := range esperados {
		agendarRecuo(m, "item-1")
		if m["item-1"].recuo != querido {
			t.Errorf("conferência %d: recuo %v, queria %v", i+2, m["item-1"].recuo, querido)
		}
	}
}

func TestRecuoEPorItem(t *testing.T) {
	m := map[string]*esperaDeAquecimento{}
	for i := 0; i < 5; i++ {
		agendarRecuo(m, "quente")
	}
	agendarRecuo(m, "novo")
	if m["novo"].recuo != recuoInicial {
		t.Errorf("item novo herdou o recuo de outro: %v", m["novo"].recuo)
	}
	if m["quente"].recuo == recuoInicial {
		t.Error("o item antigo deveria ter recuado")
	}
}

// Doze horas de Deep Archive: quantas conferências o recuo custa, contra as 720
// de antes. O número em si não é contrato — o que o teste trava é a ordem de
// grandeza, para ninguém remover o recuo sem perceber o que está devolvendo.
func TestRecuoCortaAsConferenciasDeUmDeepArchive(t *testing.T) {
	m := map[string]*esperaDeAquecimento{}
	agora := time.Now()
	fim := agora.Add(12 * time.Hour)
	relogio := agora
	n := 0
	for relogio.Before(fim) && n < 10_000 {
		agendarRecuo(m, "frio")
		relogio = relogio.Add(m["frio"].recuo)
		n++
	}
	if n > 60 {
		t.Errorf("%d conferências em 12 h — o recuo não está cortando (antes eram 720)", n)
	}
	if n < 10 {
		t.Errorf("só %d conferências em 12 h — recuo grande demais, o cliente espera à toa", n)
	}
	t.Logf("12 h de Deep Archive: %d conferências (eram 720 a 60 s fixos)", n)
}
