package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/instantaneo"
)

func trocarInstantaneo(t *testing.T, i *instantaneo.Instantaneo, modo *string) {
	t.Helper()
	velho := abrirInstantaneo
	abrirInstantaneo = func(_ context.Context, m string, _ []string) *instantaneo.Instantaneo {
		*modo = m
		return i
	}
	t.Cleanup(func() { abrirInstantaneo = velho })
}

type corpoComConsistencia struct {
	Status            string `json:"status"`
	ErrorCode         string `json:"error_code"`
	ErrorMessage      string `json:"error_message"`
	ConsistencyMethod string `json:"consistency_method"`
}

func lerConsistencia(t *testing.T, painel *painelFalso) corpoComConsistencia {
	t.Helper()
	var c corpoComConsistencia
	if err := json.Unmarshal([]byte(painel.corpo("/api/agents/a1/sessions/s1/complete")), &c); err != nil {
		t.Fatalf("corpo do /complete: %v", err)
	}
	return c
}

// Sem instantâneo, o backup segue lido do disco, e a sessão concluída leva o
// aviso e o método "none" — o painel diz que arquivo aberto pode ter saído
// pela metade.
func TestSemInstantaneoConcluiComAviso(t *testing.T) {
	var modo string
	trocarInstantaneo(t, &instantaneo.Instantaneo{Metodo: instantaneo.MetodoNenhum, Aviso: "não consegui criar o instantâneo (VSS) de C:: acesso negado"}, &modo)
	painel, c := novoPainel(t)
	plan := planoComArquivo(t)
	plan.Snapshot = "auto"
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, plan); err != nil {
		t.Fatal(err)
	}
	got := lerConsistencia(t, painel)
	if modo != "auto" {
		t.Fatalf("modo do plano não chegou ao instantâneo: %q", modo)
	}
	if got.Status != "complete" || got.ErrorCode != codigoSemInstantaneo || got.ConsistencyMethod != "none" {
		t.Fatalf("complete = %+v; queria complete/%s/none", got, codigoSemInstantaneo)
	}
	if !strings.Contains(got.ErrorMessage, "acesso negado") || !strings.Contains(got.ErrorMessage, "direto do disco") {
		t.Fatalf("aviso sem o motivo e a consequência: %q", got.ErrorMessage)
	}
}

// Com instantâneo, o método vai ao painel e não há aviso.
func TestComInstantaneoInformaOMetodo(t *testing.T) {
	var modo string
	trocarInstantaneo(t, &instantaneo.Instantaneo{Metodo: instantaneo.MetodoVSS}, &modo)
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, planoComArquivo(t)); err != nil {
		t.Fatal(err)
	}
	if got := lerConsistencia(t, painel); got.Status != "complete" || got.ErrorCode != "" || got.ConsistencyMethod != "vss" {
		t.Fatalf("complete = %+v; queria complete sem aviso, vss", got)
	}
}

// Fora do Windows o campo nem vai: o painel mostra "—", como sempre.
func TestSemTentativaNaoMandaOMetodo(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, planoComArquivo(t)); err != nil {
		t.Fatal(err)
	}
	if corpo := painel.corpo("/api/agents/a1/sessions/s1/complete"); strings.Contains(corpo, "consistency_method") {
		t.Fatalf("complete fora do Windows com consistency_method: %s", corpo)
	}
}
