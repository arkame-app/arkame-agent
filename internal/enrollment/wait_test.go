package enrollment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
)

// 409 token_already_issued no wait-token: o agente reabria para sempre, com
// o motivo só no log de Debug. Agora para na primeira resposta e diz o que
// fazer.
func TestWaitTokenJaEmitidoParaComOrientacao(t *testing.T) {
	var chamadas atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chamadas.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"token_already_issued"}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	kp, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PanelURL:       srv.URL,
		PrivateKeyPath: filepath.Join(dir, "key.pem"),
		AgentIDPath:    filepath.Join(dir, "agent.id"),
	}
	if err := kp.SaveToDisk(cfg.PrivateKeyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AgentIDPath, []byte("ag-1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = WaitForApproval(ctx, cfg, "/api/agents/ag-1/wait-token")
	if err == nil || ctx.Err() != nil {
		t.Fatalf("deveria parar no 409, veio err=%v ctx=%v", err, ctx.Err())
	}
	if !strings.Contains(err.Error(), "Reinstalar") {
		t.Fatalf("a mensagem não orienta a gerar um código novo: %v", err)
	}
	if n := chamadas.Load(); n != 1 {
		t.Fatalf("%d chamadas ao wait-token; o 409 não deveria ser repetido", n)
	}
}
