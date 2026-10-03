package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// corpoDoStart lê o que o agente mandou no /sessions/start. O campo é
// ponteiro para distinguir "não veio" de "veio vazio".
func corpoDoStart(t *testing.T, painel *painelFalso) (exclusoes *[]string, bruto string) {
	t.Helper()
	bruto = painel.corpo("/api/agents/a1/sessions/start")
	var corpo struct {
		ExcludeGlobs *[]string `json:"exclude_globs"`
	}
	if err := json.Unmarshal([]byte(bruto), &corpo); err != nil {
		t.Fatalf("corpo do /start ilegível: %v (%s)", err, bruto)
	}
	return corpo.ExcludeGlobs, bruto
}

// O /start leva as exclusões que o walker usa nesta sessão, e não outras: o
// painel grava essas na sessão. Se gravasse as do plano em vigor no /start, um
// `*.log` tirado do plano no meio de outra execução fazia a sessão dizer
// "sem exclusões" sem ter nenhum .log — e a falta virava remoção.
func TestStartLevaAsExclusoesDoWalker(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	var puts atomic.Int32

	dir := t.TempDir()
	for _, nome := range []string{"a.txt", "b.log"} {
		if err := os.WriteFile(filepath.Join(dir, nome), []byte(nome), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := api.Plan{
		ID:           "p1",
		Kind:         "backup",
		SourcePaths:  []string{dir},
		ExcludeGlobs: []string{"*.log"},
		StorageRef:   api.StorageRef{Bucket: "b"},
	}

	if err := executePlan(context.Background(), c, bucketComVersionamento(t, "Enabled", &puts), cfg, plan); err != nil {
		t.Fatal(err)
	}
	exclusoes, bruto := corpoDoStart(t, painel)
	if exclusoes == nil || !slices.Equal(*exclusoes, []string{"*.log"}) {
		t.Fatalf("o /start não levou as exclusões do walker: %s", bruto)
	}
	// E o walker usou essas mesmas: o .log ficou de fora, o .txt foi.
	if n := puts.Load(); n != 1 {
		t.Fatalf("PUTs = %d, queria 1 (só a.txt)", n)
	}
}

// Plano sem exclusões: o campo vai como lista vazia, não null nem ausente —
// ausente é o que o painel vê de agente antigo.
func TestStartSemExclusoesMandaListaVazia(t *testing.T) {
	painel, c := novoPainel(t)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	var puts atomic.Int32

	if err := executePlan(context.Background(), c, bucketComVersionamento(t, "Enabled", &puts), cfg, planoComArquivo(t)); err != nil {
		t.Fatal(err)
	}
	exclusoes, bruto := corpoDoStart(t, painel)
	if exclusoes == nil || len(*exclusoes) != 0 {
		t.Fatalf("esperava \"exclude_globs\":[] no /start: %s", bruto)
	}
}
