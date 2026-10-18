//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// A restauração que não terminou inteira: o agente apaga a pasta temporária
// de dentro das restaurações, sem importar; a de fora fica; a que já sumiu
// conta como feita.
func TestLimpezaDaRestauracaoQueNaoTerminou(t *testing.T) {
	base := filepath.Join(t.TempDir(), nomeDaPastaDeRestauracao)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	soAPropria(t)
	dentro := filepath.Join(base, "j1", "maria@empresa.com.br")
	if err := os.MkdirAll(dentro, 0o700); err != nil {
		t.Fatal(err)
	}
	fora := filepath.Join(t.TempDir(), "x", "maria@empresa.com.br")
	if err := os.MkdirAll(fora, 0o700); err != nil {
		t.Fatal(err)
	}
	sumiu := filepath.Join(base, "j3", "jose@empresa.com.br")
	acoes := []posAcao{
		{JobID: "j1", Type: tipoDaLimpeza, Path: dentro, Account: "maria@empresa.com.br"},
		{JobID: "j2", Type: tipoDaLimpeza, Path: fora, Account: "maria@empresa.com.br"},
		{JobID: "j3", Type: tipoDaLimpeza, Path: sumiu, Account: "jose@empresa.com.br"},
	}
	var mu sync.Mutex
	relatos := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"actions": acoes})
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		relatos[filepath.Base(r.URL.Path)] = m
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	importou := false
	processarPosAcoes(context.Background(), c, &config.Config{AgentID: "ag"}, nil, func(context.Context, posAcao, *bytes.Buffer) error {
		importou = true
		return nil
	})
	if importou {
		t.Fatal("a limpeza importou a conta")
	}
	if _, err := os.Stat(filepath.Dir(dentro)); !os.IsNotExist(err) {
		t.Fatal("a pasta da restauração que não terminou ficou no disco")
	}
	if _, err := os.Stat(fora); err != nil {
		t.Fatal("a limpeza apagou uma pasta de fora das restaurações")
	}
	for job, ok := range map[string]bool{"j1": true, "j2": false, "j3": true} {
		if r := relatos[job]; r == nil || r["ok"] != ok {
			t.Fatalf("relato de %s = %v, quer ok=%v", job, relatos[job], ok)
		}
	}
}
