package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// Sessão parcial: a causa ia embora — nem log, nem painel sabiam por quê.
func TestSessaoParcialLevaACausa(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de um arquivo ilegível (chmod 000, sem root)")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruim := filepath.Join(dir, "trancado.txt")
	if err := os.WriteFile(ruim, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ruim, 0); err != nil {
		t.Fatal(err)
	}

	log := capturarLog(t)
	painel, c := novoPainel(t)
	plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{dir}, StorageRef: api.StorageRef{Bucket: "b"}}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, plan); err != nil {
		t.Fatal(err)
	}

	corpo := painel.corpo("/api/agents/a1/sessions/s1/complete")
	if !strings.Contains(corpo, `"status":"partial"`) || !strings.Contains(corpo, `"error_code":"sync_partial"`) ||
		!strings.Contains(corpo, "trancado.txt") {
		t.Fatalf("o /complete não leva a causa do parcial: %s", corpo)
	}
	if l := log.String(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, "backup parcial") {
		t.Fatalf("sem aviso de backup parcial no log:\n%s", l)
	}
}

func TestCausaDoParcialCorta(t *testing.T) {
	longa := errors.New(strings.Repeat("é", 3000))
	m := causaDoParcial(&syncengine.Result{FilesFailed: 1, PrimeiraFalha: "a.txt: negado"}, longa)
	if len(m) > 4000 || !utf8.ValidString(m) || !strings.HasPrefix(m, "1 arquivo(s) não subiram (ex.: a.txt: negado); ") {
		t.Fatalf("len=%d válido=%v começo=%q", len(m), utf8.ValidString(m), m[:60])
	}
}
