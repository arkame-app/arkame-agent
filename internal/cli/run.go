package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/daemon"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		configFile string
		hostRoot   string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inicia o daemon do agent (enrollment já concluído)",
		Long: `Loop principal do agent:
  - Puxa plans do painel (long-poll + fallback a cada 60s)
  - Executa backups conforme schedule + janelas + throttling
  - Reporta heartbeat a cada 60s
  - Responde a comandos on-demand (restore, probe-storage, update)
  - Aplica novas versões quando disponíveis (self-update)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configFile, config.Overrides{
				HostRoot: hostRoot,
			})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if cfg.TokenPath == "" {
				return fmt.Errorf("agent ainda não foi aprovado — rode 'arkame-agent install' primeiro")
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			slog.Info("iniciando daemon",
				"agent_id", cfg.AgentID,
				"panel", cfg.PanelURL,
				"host_root", cfg.HostRoot)

			return daemon.Run(ctx, cfg)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "env-file com credenciais de storage")
	cmd.Flags().StringVar(&hostRoot, "host-root", "/", "raiz do filesystem a proteger (em container Docker: /host)")

	return cmd
}

var _ = context.Background
