package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/terminal"
	"github.com/spf13/cobra"
)

// newUninstallCmd remove o agente desta máquina: o serviço, a configuração
// (com a chave do bucket), a identidade e o próprio programa. Os backups
// continuam no bucket, e o servidor continua no painel até ser arquivado lá —
// é o painel que decide a cobrança e o histórico.
//
// Não existia: `service uninstall` tirava só o serviço, e o arquivo com a chave
// ficava para trás. No Windows o agente nem aparecia em "Aplicativos
// instalados" (teste do fundador, 27/09).
func newUninstallCmd() *cobra.Command {
	var (
		configFile   string
		serviceName  string
		serviceScope string
		sim          bool
		pausar       bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove o agente desta máquina (serviço, configuração com a chave, identidade e programa)",
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if reaberto, eerr := aplicativos.ElevarSeNecessario(); eerr != nil {
				return eerr
			} else if reaberto {
				return nil // a janela elevada segue daqui
			}
			if pausar {
				defer esperarEnter(&err)
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if !sim {
				t, terr := terminal.Open()
				if terr != nil {
					return errors.New("sem terminal para confirmar: rode com --yes")
				}
				fmt.Fprintln(os.Stderr, "\n  Isto remove o agente Arkame desta máquina: o serviço, a configuração com a chave")
				fmt.Fprintln(os.Stderr, "  do bucket e o programa. Os backups continuam no bucket.")
				resp, rerr := t.Pergunta("Remover? Digite sim para confirmar")
				t.Close()
				if rerr != nil || !strings.EqualFold(strings.TrimSpace(resp), "sim") {
					return errors.New("nada foi removido")
				}
			}

			// Serviço primeiro: com ele de pé, o Windows não deixa apagar o
			// programa, e o agente voltaria a rodar sem configuração.
			if err := service.Uninstall(cmd.Context(), service.Options{Name: serviceName, Scope: service.Scope(serviceScope)}); err != nil {
				return fmt.Errorf("removendo o serviço: %w", err)
			}
			fmt.Fprintln(os.Stderr, "  ✓ Serviço removido")

			// Só os arquivos que o agente cria, e a pasta se ficar vazia.
			for _, p := range []string{configFile, cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath} {
				if p == "" {
					continue
				}
				if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
					return fmt.Errorf("removendo %s: %w", p, rerr)
				}
			}
			_ = os.Remove(filepath.Dir(configFile))
			fmt.Fprintln(os.Stderr, "  ✓ Configuração, chave e identidade removidas de", filepath.Dir(configFile))

			exe, err := os.Executable()
			if err == nil {
				err = aplicativos.Remover(exe)
			}
			if err != nil {
				return fmt.Errorf("o agente parou e a configuração saiu, mas o programa ficou: %w", err)
			}
			fmt.Fprintln(os.Stderr, "  ✓ Programa removido")
			fmt.Fprintln(os.Stderr, "\n  No painel, arquive o servidor (Servidores → ⋯ → Arquivar) para ele deixar de ser cobrado.")
			fmt.Fprintln(os.Stderr, "  Os backups dele continuam restauráveis.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "arquivo de configuração")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço a remover")
	cmd.Flags().StringVar(&serviceScope, "service-scope", "", "system ou user, como na instalação. Padrão: system se root, senão user")
	cmd.Flags().BoolVar(&sim, "yes", false, "não pedir confirmação")
	cmd.Flags().BoolVar(&pausar, "pause", false, "esperar um Enter antes de sair (janela aberta pelo Windows)")
	return cmd
}

// esperarEnter segura a janela aberta pelo Windows (Executar, Aplicativos
// instalados) até a pessoa ler o resultado.
func esperarEnter(err *error) {
	if *err != nil {
		fmt.Fprintln(os.Stderr, "\n  ✗", *err)
	}
	fmt.Fprint(os.Stderr, "\n  Pressione Enter para fechar.")
	if t, terr := terminal.Open(); terr == nil {
		_, _ = t.Pergunta("")
		t.Close()
	}
}
