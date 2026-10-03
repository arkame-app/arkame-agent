package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// prefixoDoStart lê o prefix_root que o agente mandou no /sessions/start. O
// ponteiro distingue "não veio" (agente antigo) de "veio vazio".
func prefixoDoStart(t *testing.T, painel *painelFalso) (*string, string) {
	t.Helper()
	bruto := painel.corpo("/api/agents/a1/sessions/start")
	var corpo struct {
		PrefixRoot *string `json:"prefix_root"`
	}
	if err := json.Unmarshal([]byte(bruto), &corpo); err != nil {
		t.Fatalf("corpo do /start ilegível: %v (%s)", err, bruto)
	}
	return corpo.PrefixRoot, bruto
}

// O /start leva o prefixo de chave com que esta sessão monta as chaves. O
// painel lê a seleção da sessão por ele: sem isso, depois de o cliente trocar
// o prefixo e voltar, as chaves do prefixo atual de um servidor que ainda não
// rodou eram lidas como "removidas do servidor" e saíam na limpeza.
func TestStartLevaOPrefixoDasChaves(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	var puts atomic.Int32
	plan := planoComArquivo(t)
	plan.StorageRef.PrefixRoot = "A/"

	if err := executePlan(context.Background(), c, bucketComVersionamento(t, "Enabled", &puts), cfg, plan); err != nil {
		t.Fatal(err)
	}
	prefixo, bruto := prefixoDoStart(t, painel)
	if prefixo == nil || *prefixo != "A/" {
		t.Fatalf("o /start não levou o prefixo das chaves: %s", bruto)
	}
	// E é esse o prefixo das chaves que a sessão gravou.
	completo := painel.corpo("/api/agents/a1/sessions/s1/complete")
	if !strings.Contains(completo, `"A/data/a1/`) {
		t.Fatalf("as chaves da sessão não estão sob o prefixo do /start: %s", completo)
	}
}

// Prefixo vazio (as chaves na raiz do bucket) vai como "", não ausente:
// ausente é o que o painel vê de agente antigo.
func TestStartComPrefixoVazioMandaStringVazia(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	var puts atomic.Int32

	if err := executePlan(context.Background(), c, bucketComVersionamento(t, "Enabled", &puts), cfg, planoComArquivo(t)); err != nil {
		t.Fatal(err)
	}
	prefixo, bruto := prefixoDoStart(t, painel)
	if prefixo == nil || *prefixo != "" {
		t.Fatalf(`esperava "prefix_root":"" no /start: %s`, bruto)
	}
}
