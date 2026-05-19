package cli

import (
	"fmt"
	"runtime"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

// newHeartbeatCmd envia um heartbeat one-shot ao painel.
// Útil pra testar autenticação bearer token sem subir o daemon completo.
func newHeartbeatCmd() *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Envia um heartbeat one-shot (útil para testar bearer auth)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if cfg.AgentID == "" {
				return fmt.Errorf("AGENT_ID não configurado (em %s ou env)", configFile)
			}

			bearer, err := cfg.LoadToken()
			if err != nil {
				return fmt.Errorf("lendo bearer token: %w", err)
			}
			if bearer == "" {
				return fmt.Errorf("bearer token ausente — rode 'arkame-agent install' primeiro")
			}

			client, err := api.New(api.Options{
				BaseURL: cfg.PanelURL,
				Bearer:  bearer,
				Timeout: 10 * time.Second,
			})
			if err != nil {
				return err
			}

			req := api.HeartbeatRequest{
				AgentID:      cfg.AgentID,
				AgentVersion: version.Version,
				OS:           runtime.GOOS + "-" + runtime.GOARCH,
				ReportedAt:   time.Now().UTC(),
			}

			var resp struct {
				OK         bool   `json:"ok"`
				ServerTime string `json:"server_time"`
			}
			if err := client.POST(ctx, "/api/agents/"+cfg.AgentID+"/heartbeat", req, &resp); err != nil {
				return fmt.Errorf("heartbeat falhou: %w", err)
			}
			fmt.Println("heartbeat ok @", resp.ServerTime)
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "env-file")
	return cmd
}
