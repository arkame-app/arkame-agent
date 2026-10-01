package scheduler

import (
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// Plano agendado roda só quando a próxima execução vence; sem ela, não roda
// (até 01/10, vazio significava rodar a cada consulta).
func TestShouldRunAgendado(t *testing.T) {
	agora := time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)
	antes, depois := agora.Add(-time.Minute), agora.Add(time.Hour)
	p := api.Plan{}
	p.Schedule.Type = "daily"
	if ShouldRun(p, agora) {
		t.Fatal("sem next_run_at não deveria rodar")
	}
	p.NextRunAt = &depois
	if ShouldRun(p, agora) {
		t.Fatal("antes da hora não deveria rodar")
	}
	p.NextRunAt = &antes
	if !ShouldRun(p, agora) {
		t.Fatal("vencido deveria rodar")
	}
}
