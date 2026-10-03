package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Item com conflito e estratégia "skip": vai ao painel como "skipped". Antes
// ia "complete", e o painel dizia restaurado um arquivo que não foi escrito.
func TestRestoreSkipVaiComoSkipped(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "app.conf"), []byte("atual"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := api.RestoreItem{ItemID: "i1", Bucket: "b", Status: "queued", SourceKey: "k", SourceVersionID: "v1",
		DestPath: dest, DestFilename: "app.conf", ConflictStrategy: "skip"}

	var (
		mu     gosync.Mutex
		status []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/restore-items") {
			_ = json.NewEncoder(w).Encode(api.ListRestoreItemsResponse{Items: []api.RestoreItem{item}})
			return
		}
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			var u api.RestoreItemUpdate
			_ = json.Unmarshal(b, &u)
			mu.Lock()
			status = append(status, u.Status)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageBucket: "b"}
	processarFilaDeRestore(context.Background(), c, s3SemUso(t), cfg, map[string]*esperaDeAquecimento{})

	mu.Lock()
	defer mu.Unlock()
	if len(status) != 2 || status[0] != "running" || status[1] != "skipped" {
		t.Fatalf("PATCHs = %v, queria [running skipped]", status)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "app.conf")); string(b) != "atual" {
		t.Fatalf("o skip mexeu no arquivo: %q", b)
	}
}
