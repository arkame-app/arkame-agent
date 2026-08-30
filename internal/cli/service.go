package cli

import (
	"fmt"
	"os"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
	"github.com/spf13/cobra"
)

// newServiceCmd expõe a gestão do serviço do SO separada do enrollment: serve
// para reinstalar a unit depois de atualizar o binário, mudar o nome quando o
// host passa a ter mais de um agent, ou remover tudo sem mexer no cadastro
// feito no painel.
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Instala, remove ou inspeciona o serviço do sistema operacional",
	}
	cmd.AddCommand(newServiceInstallCmd(), newServiceUninstallCmd(), newServiceStatusCmd())
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	var (
		configFile string
		name       string
		scope      string
		start      bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Registra o agent como serviço (systemd, launchd ou Windows Service)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}

			inst, err := service.Install(cmd.Context(), cfg, service.Options{
				Name:  name,
				Scope: service.Scope(scope),
				Start: start,
			})
			if err != nil {
				return err
			}

			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  ✓ Serviço instalado:", inst.Name, "("+string(inst.Scope)+")")
			fmt.Fprintln(os.Stderr, "    arquivo:  ", inst.UnitPath)
			fmt.Fprintln(os.Stderr, "    reiniciar:", inst.StartCmd)
			fmt.Fprintln(os.Stderr, "    status:   ", inst.StatusCmd)
			fmt.Fprintln(os.Stderr, "    logs:     ", inst.LogsCmd)
			if inst.LingerNote != "" {
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "  ⚠", inst.LingerNote)
			}
			fmt.Fprintln(os.Stderr, "")
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "env-file que o serviço vai carregar")
	cmd.Flags().StringVar(&name, "name", service.DefaultName, "nome do serviço (um por credencial de storage no mesmo host)")
	cmd.Flags().StringVar(&scope, "scope", "", "system (todo o host, exige root) ou user (sem sudo). Padrão: system se root, senão user")
	cmd.Flags().BoolVar(&start, "start", true, "iniciar o serviço logo após instalar")
	return cmd
}

func newServiceUninstallCmd() *cobra.Command {
	var (
		name  string
		scope string
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove o serviço do sistema (não apaga token, chave nem configuração)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := service.Uninstall(cmd.Context(), service.Options{
				Name:  name,
				Scope: service.Scope(scope),
			}); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "  ✓ Serviço removido:", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", service.DefaultName, "nome do serviço a remover")
	cmd.Flags().StringVar(&scope, "scope", "", "system ou user. Padrão: system se root, senão user")
	return cmd
}

func newServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Mostra sob qual serviço este processo está rodando",
		RunE: func(_ *cobra.Command, _ []string) error {
			d := service.Detect()
			if d.Name == "" {
				fmt.Println("Nenhum serviço detectado — este processo está rodando à mão.")
				return nil
			}
			fmt.Printf("Serviço: %s (%s)\n", d.Name, d.Scope)
			return nil
		},
	}
}
