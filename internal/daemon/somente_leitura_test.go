//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	gosync "sync"
	"syscall"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Restaurar no lugar original em /etc, /usr ou /boot, no serviço nativo
// (ProtectSystem=full), falha com EROFS: o item vai com o error_code
// read_only_destination e a explicação, e não só "read-only file system".
// Precisa de uma pasta só de leitura de verdade: o /usr do Fedora Atomic e de
// outros sistemas imutáveis; noutros, o teste é pulado.
func TestRestoreEmDestinoSoDeLeituraFalhaComReadOnlyDestination(t *testing.T) {
	const dest = "/usr/arkame-teste-somente-leitura"
	if err := os.Mkdir(dest, 0o700); !errors.Is(err, syscall.EROFS) {
		if err == nil {
			_ = os.Remove(dest)
		}
		t.Skipf("/usr não é só de leitura aqui (%v)", err)
	}
	item := api.RestoreItem{ItemID: "i1", Bucket: "b", Status: "queued", SourceKey: "k", SourceVersionID: "v1",
		DestPath: dest, DestFilename: "nginx.conf", ConflictStrategy: "overwrite"}

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
	if len(updates) != 2 || updates[1].Status != "failed" || updates[1].ErrorCode != "read_only_destination" {
		t.Fatalf("PATCHs = %+v, queria running e failed/read_only_destination", updates)
	}
	if !strings.Contains(updates[1].ErrorMessage, "ProtectSystem") || !strings.Contains(updates[1].ErrorMessage, "Docker") {
		t.Fatalf("a mensagem não explica a falha: %q", updates[1].ErrorMessage)
	}
}
