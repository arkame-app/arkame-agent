package sync

import (
	"testing"
	"time"
)

// Relógio do servidor adiantado (achado B1): as entradas gravadas nesse dia
// levam uma data de reconferência no futuro distante. Com o relógio de volta,
// estão vencidas — antes, ficavam sem reconferência até o relógio chegar lá.
func TestRelogioAdiantadoNaoAdiaAReconferencia(t *testing.T) {
	m := novaMesa(t)
	m.semear(60)
	m.rodar()
	certo := m.agora
	m.agora = certo.Add(400 * 24 * time.Hour) // o relógio pula um ano
	if r, _ := m.rodar(); r.Verificados != 60 {
		t.Fatalf("com o relógio adiantado todas vencem: %d", r.Verificados)
	}
	m.agora = certo.Add(24 * time.Hour) // e volta
	m.s3.Zerar()
	r, _ := m.rodar()
	if r.Verificados != 60 || m.s3.N("HeadObject") != 60 {
		t.Fatalf("data de reconferência além do ciclo é data vencida: reconferidos=%d s3=%v", r.Verificados, m.s3.Requisicoes())
	}
	// Reconferidas, voltam ao ciclo normal.
	m.agora = m.agora.Add(24 * time.Hour)
	if r, _ := m.rodar(); r.Verificados != 0 {
		t.Fatalf("no dia seguinte nada vence: %d", r.Verificados)
	}
	m.conferir()
}
