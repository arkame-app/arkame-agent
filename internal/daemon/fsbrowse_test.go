package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/config"
)

// Painel que responde 204 na hora (sem segurar o long-poll): o agente não
// pode virar um laço apertado de requisições.
func TestFsBrowseNaoMarteladaCom204Imediato(t *testing.T) {
	antes := intervaloMinimoFs
	intervaloMinimoFs = 100 * time.Millisecond
	defer func() { intervaloMinimoFs = antes }()

	painel, c := novoPainel(t)
	painel.resposta = func(string, int) (int, string) { return 204, "" }

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	fsBrowseLoop(ctx, c, &config.Config{AgentID: "a1"})

	if n := painel.recebeu("/fs-requests"); n > 6 {
		t.Fatalf("%d requisições em 350 ms com 204 imediato — laço apertado", n)
	}
}
