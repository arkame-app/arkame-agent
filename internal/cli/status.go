package cli

import (
	"fmt"

	"github.com/arkame-app/agent/internal/config"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Mostra estado local do agent (fingerprint, enrollment, último heartbeat)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return err
			}
			fmt.Println("Agent ID:     ", orNA(cfg.AgentID))
			fmt.Println("Painel:       ", cfg.PanelURL)
			fmt.Println("Fingerprint:  ", orNA(cfg.Fingerprint))
			fmt.Println("Token path:   ", orNA(cfg.TokenPath))
			fmt.Println("Host root:    ", cfg.HostRoot)
			fmt.Println("Storage:      ", cfg.StorageID)
			if cfg.EnrollmentToken != "" {
				fmt.Println("Enrollment:   PENDENTE — aguardando aprovação no painel")
			} else if cfg.TokenPath != "" {
				fmt.Println("Enrollment:   APROVADO (bearer token presente)")
			} else {
				fmt.Println("Enrollment:   NÃO INICIADO — rode 'arkame-agent install'")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "env-file")
	return cmd
}

func orNA(s string) string {
	if s == "" {
		return "(não configurado)"
	}
	return s
}
