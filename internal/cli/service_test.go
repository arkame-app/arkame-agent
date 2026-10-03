package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// `service install --config ./agent.env` gravava o caminho relativo na unit,
// que não acha o arquivo quando o serviço sobe (outro diretório de trabalho).
func TestServiceInstallUsaCaminhoAbsoluto(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.env"), []byte("STORAGE_BUCKET=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var recebido string
	antes := instalarServico
	instalarServico = func(_ context.Context, cfg *config.Config, o service.Options) (*service.Installed, error) {
		recebido = cfg.ConfigPath
		return &service.Installed{Name: o.Name, Scope: service.ScopeUser}, nil
	}
	t.Cleanup(func() { instalarServico = antes })

	cmd := newServiceInstallCmd()
	cmd.SetArgs([]string{"--config", "./agent.env", "--start=false"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if !filepath.IsAbs(recebido) {
		t.Fatalf("o serviço recebeu %q, caminho relativo", recebido)
	}
	if got, _ := filepath.EvalSymlinks(recebido); got != filepath.Join(real, "agent.env") {
		t.Fatalf("o serviço recebeu %q, queria %s", recebido, filepath.Join(real, "agent.env"))
	}
}
