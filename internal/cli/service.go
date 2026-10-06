package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
	cmd.AddCommand(newServiceInstallCmd(), newServiceUninstallCmd(), newServiceRefreshCmd(), newServiceStatusCmd())
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
			// Caminho absoluto, como no install: a unit (e o plist, e o SCM)
			// recebe este texto, e um caminho relativo não aponta para nada
			// quando o serviço sobe. Sem --config, o padrão do escopo.
			if !cmd.Flags().Changed("config") {
				configFile = configPadrao(scope)
			}
			if abs, err := filepath.Abs(configFile); err == nil {
				configFile = abs
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}

			inst, err := instalarServico(cmd.Context(), cfg, service.Options{
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

	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "env-file que o serviço vai carregar (no escopo user, padrão $XDG_CONFIG_HOME/arkame/agent.env, ou ~/.config/arkame/agent.env)")
	cmd.Flags().StringVar(&name, "name", service.DefaultName, "nome do serviço (um por credencial de storage no mesmo host)")
	cmd.Flags().StringVar(&scope, "scope", "", "system (todo o host, exige root) ou user (sem sudo). Padrão: system se root, senão user")
	cmd.Flags().BoolVar(&start, "start", true, "iniciar o serviço logo após instalar")
	return cmd
}

// newServiceRefreshCmd regrava o registro de um serviço já instalado com o
// modelo desta versão, sem registrar de novo no painel. O install.sh o chama
// depois de trocar o programa: a unit de sistema instalada antes da
// atualização automática não deixa o serviço gravar na pasta do programa.
func newServiceRefreshCmd() *cobra.Command {
	var (
		name  string
		scope string
	)
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Regrava o registro de um serviço já instalado com o modelo desta versão (sem reiniciar)",
		Long: `Regrava a unit do systemd de um agente já instalado, com o programa e o
arquivo de configuração que ela já tem, e recarrega o systemd. Não habilita,
não inicia e não reinicia o serviço, e não fala com o painel.

É o passo da última atualização manual de quem instalou antes da atualização
automática: a unit de sistema antiga não libera a pasta do programa para
escrita, e o agente não conseguiria trocar o próprio programa. O instalador
(install.sh) já o roda. No macOS e no Windows não há o que regravar.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			configFile, ok := configDoServico(name, service.Scope(scope))
			if !ok || configFile == "" {
				return fmt.Errorf("não achei o serviço %s (ou o registro dele não cita o arquivo de configuração)", name)
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			arquivo, err := regravarServico(cmd.Context(), cfg, service.Options{
				Name:  name,
				Scope: service.Scope(scope),
			})
			if err != nil {
				return err
			}
			if arquivo == "" {
				fmt.Fprintln(os.Stderr, "  Nada a regravar neste sistema.")
				return nil
			}
			fmt.Fprintln(os.Stderr, "  ✓ Registro do serviço regravado:", arquivo)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", service.DefaultName, "nome do serviço")
	cmd.Flags().StringVar(&scope, "scope", "", "system ou user. Padrão: system se root, senão user")
	return cmd
}

// regravarServico é o service.Regravar; os testes o trocam.
var regravarServico = service.Regravar

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
