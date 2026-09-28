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
	"github.com/arkame-app/agent/internal/setup"
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
		elevado      bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove o agente desta máquina (serviço, configuração com a chave, identidade e programa)",
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if pausar {
				// Aberto pelo Windows numa janela própria: sem isto, ela fecha
				// antes de a pessoa ler o resultado — inclusive o erro.
				defer func() {
					if pausar { // pode ter sido desligado no caminho
						esperarEnter(&err)
					}
				}()
			}
			if reaberto, eerr := aplicativos.ElevarSeNecessario(); eerr != nil {
				return eerr
			} else if reaberto {
				pausar = false // a janela elevada segue daqui; esta não espera
				return nil
			}

			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			// Antes de tocar em qualquer coisa: a configuração existe, e dá para
			// apagá-la. Sem isto, sem sudo ou com o --config errado, o serviço
			// saía, o programa saía, e a chave ficava.
			arquivos := []string{}
			for _, p := range []string{configFile, cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath} {
				if p != "" {
					if _, serr := os.Stat(p); serr == nil {
						arquivos = append(arquivos, p)
					}
				}
			}
			if len(arquivos) == 0 {
				return fmt.Errorf("não encontrei a configuração do agente em %s: nada foi removido (use --config com o caminho da instalação)", configFile)
			}
			if perr := setup.PodeGravar(configFile); perr != nil {
				return fmt.Errorf("%w — nada foi removido", perr)
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
			fmt.Fprintln(os.Stderr, "  ✓ Serviço", serviceName, "removido")

			for _, p := range arquivos {
				if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
					return fmt.Errorf("removendo %s: %w", p, rerr)
				}
			}
			_ = os.Remove(filepath.Dir(configFile)) // só sai se ficou vazia
			fmt.Fprintln(os.Stderr, "  ✓ Configuração, chave e identidade removidas:", strings.Join(arquivos, ", "))
			aplicativos.RemoverEntrada(serviceName)

			fmt.Fprintln(os.Stderr, "\n  No painel, arquive o servidor (Servidores → ⋯ → Arquivar) para ele deixar de ser cobrado.")
			fmt.Fprintln(os.Stderr, "  Os backups dele continuam restauráveis.")

			// O programa é compartilhado pelos agentes desta máquina (um por
			// credencial de storage): só sai com o último.
			if outros := service.OutrosAgentes(serviceName); len(outros) > 0 {
				fmt.Fprintln(os.Stderr, "\n  O programa fica: ele ainda serve", strings.Join(outros, ", "))
				return nil
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("o agente parou e a configuração saiu, mas não achei o programa para apagar: %w", err)
			}
			if pausar {
				// A espera vem antes: com a janela aberta, o programa ainda
				// está em uso e não pode ser apagado.
				pausar = false
				esperarEnter(&err)
			}
			if err := aplicativos.RemoverPrograma(exe); err != nil {
				return fmt.Errorf("o agente parou e a configuração saiu, mas o programa ficou: %w", err)
			}
			if aplicativos.ProgramaSaiDepois {
				fmt.Fprintln(os.Stderr, "  ✓ O programa é apagado assim que esta janela fechar")
			} else {
				fmt.Fprintln(os.Stderr, "  ✓ Programa removido:", exe)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "arquivo de configuração")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço a remover")
	cmd.Flags().StringVar(&serviceScope, "service-scope", "", "system ou user, como na instalação. Padrão: system se root, senão user")
	cmd.Flags().BoolVar(&sim, "yes", false, "não pedir confirmação")
	cmd.Flags().BoolVar(&pausar, "pause", false, "esperar um Enter antes de sair (janela aberta pelo Windows)")
	cmd.Flags().BoolVar(&elevado, "elevado", false, "")
	_ = cmd.Flags().MarkHidden("elevado")
	return cmd
}
