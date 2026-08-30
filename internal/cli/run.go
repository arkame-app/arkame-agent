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
	"github.com/arkame-app/agent/internal/service"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		configFile  string
		hostRoot    string
		serviceName string
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
			if !cfg.TokenExists() {
				return fmt.Errorf("agent ainda não foi aprovado — rode 'arkame-agent install' primeiro")
			}

			start := func(ctx context.Context) error {
				slog.Info("iniciando daemon",
					"agent_id", cfg.AgentID,
					"panel", cfg.PanelURL,
					"host_root", cfg.HostRoot)
				return daemon.Run(ctx, cfg)
			}

			// No Windows, quando o processo é iniciado pelo Service Control
			// Manager, o daemon precisa rodar sob o protocolo de serviço —
			// senão o SCM encerra tudo com o erro 1053. Fora desse caso (e em
			// qualquer outro sistema) seguimos o caminho normal de terminal.
			if handled, err := service.RunAsService(serviceName, start); handled {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return start(ctx)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "env-file com credenciais de storage")
	cmd.Flags().StringVar(&hostRoot, "host-root", "/", "raiz do filesystem a proteger (em container Docker: /host)")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço (usado só quando iniciado pelo gerenciador de serviços do Windows)")

	return cmd
}

var _ = context.Background
