package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/hooks"
)

type corpoDoComplete struct {
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

func lerComplete(t *testing.T, painel *painelFalso) corpoDoComplete {
	t.Helper()
	var c corpoDoComplete
	if err := json.Unmarshal([]byte(painel.corpo("/api/agents/a1/sessions/s1/complete")), &c); err != nil {
		t.Fatalf("corpo do /complete: %v", err)
	}
	return c
}

// Comando de depois que falha num backup bom: a saída dele era montada e
// jogada fora. Agora vai no /complete como post_hook_failed, até 8 KB.
func TestComandoDeDepoisFalhoVaiAoPainel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell do teste é POSIX")
	}
	painel, c := novoPainel(t)
	plan := planoComArquivo(t)
	plan.PostHook = "echo 'rm: não removeu /tmp/dump.sql'; head -c 20000 /dev/zero | tr '\\0' x; exit 3"
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, plan); err != nil {
		t.Fatal(err)
	}
	got := lerComplete(t, painel)
	if got.Status != "complete" || got.ErrorCode != "post_hook_failed" {
		t.Fatalf("status=%q error_code=%q; queria complete/post_hook_failed", got.Status, got.ErrorCode)
	}
	m := got.ErrorMessage
	if !strings.Contains(m, "código 3") || !strings.Contains(m, "não removeu /tmp/dump.sql") {
		t.Fatalf("error_message sem o erro e a saída do comando: %.200q", m)
	}
	if len(m) > hooks.MaxOutputBytes || !utf8.ValidString(m) {
		t.Fatalf("error_message com %d bytes (válido=%v); limite %d", len(m), utf8.ValidString(m), hooks.MaxOutputBytes)
	}
}

// Comando de depois que dá certo: nada de error_code.
func TestComandoDeDepoisBemSucedidoNaoMarcaErro(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("comando de shell do teste é POSIX")
	}
	painel, c := novoPainel(t)
	plan := planoComArquivo(t)
	plan.PostHook = "echo limpo"
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, plan); err != nil {
		t.Fatal(err)
	}
	if got := lerComplete(t, painel); got.Status != "complete" || got.ErrorCode != "" || got.ErrorMessage != "" {
		t.Fatalf("backup bom marcado com erro: %+v", got)
	}
}

// Sessão parcial com o comando de depois falho: a causa do parcial fica, e
// ganha só a nota de que o comando de depois também falhou.
func TestParcialComComandoDeDepoisFalhoMantemACausa(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de um arquivo ilegível (chmod 000, sem root) e de shell POSIX")
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
	painel, c := novoPainel(t)
	plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{dir}, StorageRef: api.StorageRef{Bucket: "b"},
		PostHook: "echo falhei; exit 1"}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}
	if err := executePlan(context.Background(), c, bucketDeTeste(t, true), cfg, plan); err != nil {
		t.Fatal(err)
	}
	got := lerComplete(t, painel)
	if got.Status != "partial" || got.ErrorCode != "sync_partial" {
		t.Fatalf("status=%q error_code=%q; queria partial/sync_partial", got.Status, got.ErrorCode)
	}
	if !strings.Contains(got.ErrorMessage, "trancado.txt") || !strings.HasSuffix(got.ErrorMessage, "; comando de depois falhou") {
		t.Fatalf("error_message = %q", got.ErrorMessage)
	}
}
