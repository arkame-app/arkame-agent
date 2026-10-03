package cli

import (
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// Flag com valor padrão sempre "vem preenchida" e vence o ambiente e o
// env-file (CLI > env). --host-root "/" anulava HOST_ROOT; --panel-url anulava
// o PANEL_URL de um painel de parceiro.
func TestFlagsNaoAnulamOAmbiente(t *testing.T) {
	t.Setenv("HOST_ROOT", "/host")
	t.Setenv("PANEL_URL", "https://painel.parceiro")

	run := newRunCmd()
	hr := run.Flags().Lookup("host-root").DefValue
	cfg, err := config.Load("", config.Overrides{HostRoot: hr})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostRoot != "/host" {
		t.Fatalf("HostRoot = %q: o padrão da flag (%q) anulou HOST_ROOT", cfg.HostRoot, hr)
	}

	inst := newInstallCmd()
	pu := inst.Flags().Lookup("panel-url").DefValue
	if cfg, _ = config.Load("", config.Overrides{PanelURL: pu}); cfg.PanelURL != "https://painel.parceiro" {
		t.Fatalf("PanelURL = %q: o padrão da flag (%q) anulou PANEL_URL", cfg.PanelURL, pu)
	}

	// Sem nada no ambiente, os padrões continuam.
	t.Setenv("HOST_ROOT", "")
	t.Setenv("PANEL_URL", "")
	if cfg, _ = config.Load("", config.Overrides{HostRoot: hr, PanelURL: pu}); cfg.HostRoot != "/" || cfg.PanelURL != "https://save.arkame.app" {
		t.Fatalf("padrões: HostRoot=%q PanelURL=%q", cfg.HostRoot, cfg.PanelURL)
	}
}
