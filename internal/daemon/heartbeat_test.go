package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// O heartbeat leva o caminho real do programa (program_path): o instalador
// pode tê-lo posto em /opt/arkame/bin, e o painel monta com ele o comando de
// trocar a chave. Real = sem link: é o arquivo que o serviço executa.
func TestHeartbeatLevaOCaminhoDoPrograma(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("o SO não diz o executável: %v", err)
	}
	esperado, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}

	var mu gosync.Mutex
	var corpo map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(b, &corpo)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})

	enviarHeartbeat(context.Background(), c, &config.Config{AgentID: "a1"},
		service.Detected{Name: "arkame-agent", Scope: "system"}, &estadoDoToken{})

	mu.Lock()
	defer mu.Unlock()
	if corpo == nil {
		t.Fatal("o painel não recebeu o heartbeat")
	}
	if got, _ := corpo["program_path"].(string); got != esperado {
		t.Fatalf("program_path = %q, queria %q", got, esperado)
	}
	// Vai junto com o serviço, no mesmo corpo.
	if corpo["service_name"] != "arkame-agent" || corpo["service_scope"] != "system" {
		t.Fatalf("service_name/service_scope = %v/%v", corpo["service_name"], corpo["service_scope"])
	}
}
