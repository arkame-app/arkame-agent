//go:build !windows

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

// Pasta de destino trocada por um link (para /root/.ssh, no ataque): o item
// falha com error_code dest_symlink, e nada é gravado no alvo do link.
func TestRestoreComLinkNoDestinoFalhaComDestSymlink(t *testing.T) {
	dest, alvo := t.TempDir(), t.TempDir()
	// Pasta que aceita escrita dos outros: link nela nunca é do sistema,
	// mesmo com o teste rodando sem root.
	if err := os.Chmod(dest, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(alvo, filepath.Join(dest, "ssh")); err != nil {
		t.Fatal(err)
	}
	item := api.RestoreItem{ItemID: "i1", Bucket: "b", Status: "queued", SourceKey: "k", SourceVersionID: "v1",
		DestPath: dest, DestFilename: "ssh/authorized_keys", ConflictStrategy: "overwrite"}

	var (
		mu      gosync.Mutex
		updates []api.RestoreItemUpdate
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
			updates = append(updates, u)
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
	if len(updates) != 2 || updates[1].Status != "failed" || updates[1].ErrorCode != "dest_symlink" {
		t.Fatalf("PATCHs = %+v, queria running e failed/dest_symlink", updates)
	}
	if es, _ := os.ReadDir(alvo); len(es) != 0 {
		t.Fatalf("gravou no alvo do link: %v", es)
	}
}
