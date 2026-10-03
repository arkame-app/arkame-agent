package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Cem itens frios aquecendo no topo da fila, todos no recuo: sem ?limit o
// painel devolvia só os cem primeiros (o padrão dele), o agente pulava todos e
// o item 101 em diante nunca chegava.
func TestFilaDeRestoreAlcancaOsItensDepoisDosQueAquecem(t *testing.T) {
	const frios, novos = 100, 20
	var itens []api.RestoreItem
	for i := 0; i < frios; i++ {
		itens = append(itens, api.RestoreItem{ItemID: fmt.Sprintf("frio-%d", i), Bucket: "b", Status: "running"})
	}
	dest := t.TempDir()
	for i := 0; i < novos; i++ {
		itens = append(itens, api.RestoreItem{ItemID: fmt.Sprintf("novo-%d", i), Bucket: "b", Status: "queued",
			DestPath: dest, DestFilename: fmt.Sprintf("n%d.txt", i), SourceKey: "k", SourceVersionID: "v"})
	}

	var (
		mu       gosync.Mutex
		patches  = map[string]int{}
		pedidoDe string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/restore-items") {
			// Como o painel: padrão 100, teto 500.
			limite := 100
			if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
				limite = min(v, 500)
			}
			mu.Lock()
			pedidoDe = r.URL.RawQuery
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.ListRestoreItemsResponse{Items: itens[:min(limite, len(itens))]})
			return
		}
		if r.Method == http.MethodPatch {
			mu.Lock()
			patches[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]++
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})

	aquecendo := map[string]*esperaDeAquecimento{}
	for i := 0; i < frios; i++ {
		aquecendo[fmt.Sprintf("frio-%d", i)] = &esperaDeAquecimento{proxima: time.Now().Add(time.Hour), recuo: recuoMaximo}
	}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageBucket: "b"}
	processarFilaDeRestore(context.Background(), c, s3SemUso(t), cfg, aquecendo)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(pedidoDe, "limit=500") {
		t.Errorf("o agente não pediu o lote inteiro: query %q", pedidoDe)
	}
	for i := 0; i < frios; i++ {
		if n := patches[fmt.Sprintf("frio-%d", i)]; n != 0 {
			t.Fatalf("frio-%d ainda no recuo foi tocado (%d PATCH)", i, n)
		}
	}
	for i := 0; i < novos; i++ {
		// running + resultado
		if n := patches[fmt.Sprintf("novo-%d", i)]; n != 2 {
			t.Fatalf("novo-%d, atrás dos que aquecem, não foi processado (%d PATCH); query %q", i, n, pedidoDe)
		}
	}
}
