package sync

import (
	"net/http"
	gosync "sync"
	"testing"
	"time"
)

// O teto da reconferência rotativa acompanha o tempo passado desde a última
// execução confirmada: o que vence entre uma execução e outra tem de caber.
func TestTetoDaReconferenciaAcompanhaOTempoDecorrido(t *testing.T) {
	agora := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	ha := func(d time.Duration) time.Time { return agora.Add(-d) }
	const dia = 24 * time.Hour
	const milhao = 1_000_000
	casos := []struct {
		nome       string
		arquivos   int64
		ultima     time.Time
		minimo     int64 // o que vence no intervalo
		maximo     int64
		exatamente int64
	}{
		{nome: "diário", arquivos: milhao, ultima: ha(dia), minimo: milhao / 30, maximo: milhao / 30 * 12 / 10},
		{nome: "semanal", arquivos: milhao, ultima: ha(7 * dia), minimo: milhao * 7 / 30, maximo: milhao * 7 / 30 * 12 / 10},
		{nome: "de hora em hora", arquivos: 24 * milhao, ultima: ha(time.Hour), minimo: milhao / 30, maximo: milhao / 30 * 12 / 10},
		{nome: "parado há dois meses: o ciclo inteiro, não mais", arquivos: milhao, ultima: ha(60 * dia), minimo: milhao, maximo: milhao * 12 / 10},
		{nome: "sem data (primeira vez): um dia", arquivos: milhao, minimo: milhao / 30, maximo: milhao / 30 * 12 / 10},
		{nome: "relógio voltou: um dia", arquivos: milhao, ultima: agora.Add(90 * dia), minimo: milhao / 30, maximo: milhao / 30 * 12 / 10},
		{nome: "plano pequeno: o piso", arquivos: 500, ultima: ha(dia), exatamente: 1000},
		{nome: "contagem zerada: o piso", arquivos: 0, ultima: ha(7 * dia), exatamente: 1000},
	}
	for _, c := range casos {
		teto := tetoDaReconferencia(c.arquivos, c.ultima, agora, 30)
		if c.exatamente != 0 {
			if teto != c.exatamente {
				t.Errorf("%s: teto = %d; queria %d", c.nome, teto, c.exatamente)
			}
			continue
		}
		if teto < c.minimo || teto > c.maximo {
			t.Errorf("%s: teto = %d; queria entre %d e %d", c.nome, teto, c.minimo, c.maximo)
		}
	}
}

// Plano semanal (achado A2 da revisão): entre uma execução e outra vencem
// 7/30 dos arquivos. Com o teto fixo de 3,5% por execução, o ciclo levava 29
// semanas; agora todo arquivo é reconferido em ~30 dias (aqui, seis semanas
// de folga para a primeira volta, em que as datas nascem espalhadas).
func TestPlanoSemanalFechaOCicloDaReconferencia(t *testing.T) {
	antes := pisoDaReconferencia
	pisoDaReconferencia = 5 // o piso de 1.000 esconderia o teto num plano de teste
	t.Cleanup(func() { pisoDaReconferencia = antes })

	m := novaMesa(t)
	m.semear(600)
	m.rodar()
	var mu gosync.Mutex
	conferidos := map[string]bool{}
	m.s3.Antes(func(op string, r *http.Request) {
		if op == "HeadObject" {
			mu.Lock()
			conferidos[r.URL.Path] = true
			mu.Unlock()
		}
	})
	maior := int64(0)
	for semana := 1; semana <= 6; semana++ {
		m.agora = m.agora.Add(7 * 24 * time.Hour)
		r, _ := m.rodar()
		maior = max(maior, r.Verificados)
		if r.AmostraFalhou != "" {
			t.Fatalf("semana %d: %s", semana, r.AmostraFalhou)
		}
	}
	if len(conferidos) != 600 {
		t.Fatalf("em seis semanas os 600 arquivos deviam ter sido reconferidos; foram %d", len(conferidos))
	}
	// E o teto segura: nenhuma execução relê muito além do que vence em 7 dias.
	if maior > 600*7/30*13/10 {
		t.Fatalf("uma execução reconferiu %d arquivos; o que vence por semana são %d", maior, 600*7/30)
	}
}
