package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// Os arquivos do OneDrive só na nuvem que ficaram de fora vão ao /complete
// (cloud_only_skipped): o painel não trata a sessão como inventário da origem.
// Sem nenhum, o campo nem vai.
func TestCompleteLevaOsArquivosSoNaNuvem(t *testing.T) {
	for _, n := range []int{3, 0} {
		antes := rodarSync
		rodarSync = func(context.Context, syncengine.EngineOptions) (*syncengine.Result, error) {
			return &syncengine.Result{
				VersionMap:       []api.FileEntry{{Key: "k", VersionID: "v1", Size: 1, SHA256: strings.Repeat("a", 64)}},
				CloudOnlySkipped: n,
			}, nil
		}
		painel, c := novoPainel(t)
		cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
		err := executePlan(context.Background(), c, s3SemUso(t), cfg, api.Plan{ID: "p1", Kind: "backup", StorageRef: api.StorageRef{Bucket: "b"}})
		rodarSync = antes
		if err != nil {
			t.Fatal(err)
		}
		corpo := painel.corpo("/api/agents/a1/sessions/s1/complete")
		var got map[string]any
		if err := json.Unmarshal([]byte(corpo), &got); err != nil {
			t.Fatalf("corpo do /complete: %v", err)
		}
		v, tem := got["cloud_only_skipped"]
		switch {
		case n > 0 && (!tem || v != float64(n)):
			t.Fatalf("cloud_only_skipped = %v (presente=%v), queria %d; corpo: %s", v, tem, n, corpo)
		case n == 0 && tem:
			t.Fatalf("sem arquivo só na nuvem o campo não vai; corpo: %s", corpo)
		}
		if got["status"] != "complete" {
			t.Fatalf("status = %v", got["status"])
		}
	}
}
