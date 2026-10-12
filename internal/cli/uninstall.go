package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/daemon"
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

			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
			if err != nil {
				return fmt.Errorf("%w — nada foi removido", err)
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			// Antes de tocar em qualquer coisa: a configuração existe, e dá para
			// apagá-la. Sem isto, sem sudo ou com o --config errado, o serviço
			// saía, o programa saía, e a chave ficava.
			existentes := []string{}
			for _, p := range []string{configFile, cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath} {
				if p != "" {
					if _, serr := os.Stat(p); serr == nil {
						existentes = append(existentes, p)
					}
				}
			}
			if len(existentes) == 0 {
				return fmt.Errorf("não encontrei a configuração do agente em %s: nada foi removido (use --config com o caminho da instalação)", configFile)
			}
			if perr := setup.PodeGravar(configFile); perr != nil {
				return fmt.Errorf("%w — nada foi removido", perr)
			}
			// Os outros agentes desta máquina podem usar o mesmo token, chave
			// e agent.id (o padrão é /etc/arkame/ para todos): apagá-los
			// matava o vizinho.
			outros, completo := configsDosOutros(serviceName)
			arquivos, mantidos := separarArquivos(configFile, existentes, outros, completo)
			// O estado local do backup (nomes e datas dos arquivos do
			// servidor), com a mesma cautela: o que outro agente usa fica.
			estados, pastasDoEstado := daemon.ArquivosDoEstado(cfg)
			estados, estadosMantidos := separarEstado(estados, outros, completo)

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
			// O log do serviço do Windows (e o anterior, do rodízio).
			if logs := service.ArquivoDeLog(configFile); logs != configFile {
				_ = os.Remove(logs)
				_ = os.Remove(logs + ".1")
			}
			_ = os.Remove(filepath.Dir(configFile)) // só sai se ficou vazia
			fmt.Fprintln(os.Stderr, "  ✓ Removidos:", strings.Join(arquivos, ", "))
			if len(mantidos) > 0 {
				fmt.Fprintln(os.Stderr, "  Ficam, porque outro agente desta máquina pode usá-los:", strings.Join(mantidos, ", "))
			}
			// O estado é derivável e não tem segredo: o que não sair não
			// desfaz a desinstalação, mas é dito.
			semVizinhos := completo && len(outros) == 0 && len(service.OutrosAgentes(serviceName)) == 0
			if saiu, ficou := removerEstado(estados, pastasDoEstado, semVizinhos); len(saiu) > 0 || len(ficou) > 0 {
				if len(saiu) > 0 {
					fmt.Fprintln(os.Stderr, "  ✓ Estado local do backup removido:", strings.Join(saiu, ", "))
				}
				if len(ficou) > 0 {
					fmt.Fprintln(os.Stderr, "  ! Não consegui remover o estado local (apague à mão; não tem segredo, mas tem os nomes dos arquivos):", strings.Join(ficou, ", "))
				}
			}
			if len(estadosMantidos) > 0 {
				fmt.Fprintln(os.Stderr, "  Fica o estado local, porque outro agente desta máquina pode usá-lo:", strings.Join(estadosMantidos, ", "))
			}
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

// configsDosOutros carrega a configuração de cada outro agente desta máquina.
// completo é false quando a de algum não pôde ser lida.
func configsDosOutros(serviceName string) ([]*config.Config, bool) {
	caminhos, completo := service.ConfigsDosOutros(serviceName)
	var cfgs []*config.Config
	for _, c := range caminhos {
		cfg, err := config.Load(c, config.Overrides{})
		if err != nil {
			completo = false
			continue
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, completo
}

// separarArquivos decide o que o uninstall apaga. Arquivo que a configuração
// de outro agente cita fica. Sem saber de algum outro agente (completo=false),
// a identidade (tudo menos o próprio arquivo de configuração) fica toda:
// apagar no escuro é o que derrubava o vizinho.
func separarArquivos(configFile string, candidatos []string, outros []*config.Config, completo bool) (remover, manter []string) {
	usados := map[string]bool{}
	for _, o := range outros {
		for _, p := range []string{o.ConfigPath, o.TokenPath, o.PrivateKeyPath, o.AgentIDPath} {
			if p != "" {
				usados[chaveDeCaminho(p)] = true
			}
		}
	}
	proprio := chaveDeCaminho(configFile)
	for _, p := range candidatos {
		switch {
		case usados[chaveDeCaminho(p)]:
			manter = append(manter, p)
		case !completo && chaveDeCaminho(p) != proprio:
			manter = append(manter, p)
		default:
			remover = append(remover, p)
		}
	}
	return remover, manter
}

// separarEstado decide o que o uninstall apaga do estado local: os arquivos
// deste agente + armazenamento que existem. Fica o que a configuração de outro
// agente da máquina também usaria (mesmo agente, mesmo armazenamento, mesma
// pasta); e, sem saber de algum outro agente (completo=false), fica tudo.
func separarEstado(meus []string, outros []*config.Config, completo bool) (remover, manter []string) {
	usados := map[string]bool{}
	for _, o := range outros {
		deles, _ := daemon.ArquivosDoEstado(o)
		for _, p := range deles {
			usados[chaveDeCaminho(p)] = true
		}
	}
	for _, p := range meus {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if !completo || usados[chaveDeCaminho(p)] {
			manter = append(manter, p)
			continue
		}
		remover = append(remover, p)
	}
	return remover, manter
}

// removerEstado apaga os arquivos do estado local e, se ficarem vazias, as
// pastas dele (e a `arkame` acima, quando é só delas). semVizinhos: não há
// outro agente na máquina — aí saem também os temporários de índice que uma
// execução morta possa ter deixado (no Windows o arquivo aberto não sai do
// diretório).
func removerEstado(arquivos, pastas []string, semVizinhos bool) (saiu, ficou []string) {
	for _, p := range arquivos {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			ficou = append(ficou, p)
			continue
		}
		saiu = append(saiu, p)
	}
	for _, dir := range pastas {
		if semVizinhos {
			restos, _ := filepath.Glob(filepath.Join(dir, "indice-*.lotes"))
			for _, r := range restos {
				_ = os.Remove(r)
			}
		}
		// os.Remove só tira pasta vazia.
		if os.Remove(dir) == nil {
			if acima := filepath.Dir(dir); strings.EqualFold(filepath.Base(acima), "arkame") {
				_ = os.Remove(acima)
			}
		}
	}
	return saiu, ficou
}

func chaveDeCaminho(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}
