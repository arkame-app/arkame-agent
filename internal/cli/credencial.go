package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/internal/terminal"
	"github.com/spf13/cobra"
)

// garantirCredencial deixa o arquivo de configuração com uma chave que o bucket
// aceita, antes do registro no painel.
//
//   - Sem chave no arquivo: pergunta ao painel qual bucket (pelo código de
//     instalação), pede a chave no terminal, testa e grava.
//   - Com chave: testa; recusada, pede de novo (havendo terminal) ou para com
//     a causa.
//
// Devolve se o arquivo mudou, para o chamador reler só nesse caso.
func garantirCredencial(ctx context.Context, cfg *config.Config, caminho string) (bool, error) {
	semChave := cfg.StorageAccessKey == "" || cfg.StorageSecretKey == ""

	if !semChave {
		err := storage.Check(ctx, cfg)
		if err == nil {
			fmt.Fprintln(os.Stderr, "  ✓ O bucket", cfg.StorageBucket, "aceitou a chave de", caminho)
			return false, nil
		}
		t, terr := terminal.Open()
		if terr != nil || storage.Classe(err) != "chave" {
			// Sem terminal, ou problema que outra chave não resolve.
			return false, fmt.Errorf("%s (%v). Arquivo: %s", storage.Causa(err), err, caminho)
		}
		defer t.Close()
		fmt.Fprintf(os.Stderr, "\n  ✗ A chave de %s não funciona: %s.\n", caminho, storage.Causa(err))
		ak, sk, err := setup.PerguntarETestar(ctx, t, cfg)
		if err != nil {
			return false, err
		}
		return true, setup.Gravar(caminho, setup.Chaves(ak, sk))
	}

	if cfg.EnrollmentToken == "" {
		return false, fmt.Errorf("sem chave do bucket em %s", caminho)
	}
	if err := setup.PodeGravar(caminho); err != nil {
		return false, err
	}
	t, err := terminal.Open()
	if err != nil {
		return false, fmt.Errorf("sem chave do bucket em %s e sem terminal para perguntar: rode o comando num terminal interativo (no Docker, com -it)", caminho)
	}
	defer t.Close()

	p, err := setup.BuscarNoPainel(ctx, cfg.PanelURL, cfg.EnrollmentToken)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "\n  Servidor:       %s\n  Armazenamento:  %s (%s)\n", p.DisplayName, p.Armazenamento.DisplayName, p.Armazenamento.Bucket)

	ak, sk, err := setup.PerguntarETestar(ctx, t, setup.NaConfig(p, cfg))
	if err != nil {
		return false, err
	}
	if err := setup.Gravar(caminho, append(setup.Linhas(p, cfg.PanelURL), setup.Chaves(ak, sk)...)); err != nil {
		return false, err
	}
	fmt.Fprintln(os.Stderr, "  ✓ Gravado em", caminho)
	return true, nil
}

func newCheckStorageCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:   "check-storage",
		Short: "Testa se esta máquina alcança o bucket com a chave do arquivo de configuração",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if err := storage.Check(cmd.Context(), cfg); err != nil {
				return fmt.Errorf("✗ %s\n  (%v)", storage.Causa(err), err)
			}
			fmt.Fprintln(os.Stderr, "  ✓ O bucket", cfg.StorageBucket, "aceitou a chave.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "arquivo de configuração")
	return cmd
}

func newSetStorageKeysCmd() *cobra.Command {
	var (
		configFile   string
		reiniciar    bool
		pausar       bool
		serviceName  string
		serviceScope string
	)
	cmd := &cobra.Command{
		Use:   "set-storage-keys",
		Short: "Troca a chave do bucket: pergunta, testa e só grava se o bucket aceitar",
		Long: `Pergunta a chave de acesso e a senha, testa no bucket do arquivo de
configuração e só então grava. Depois, reinicie o serviço para ele usar a
chave nova (o comando aparece no fim).`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if pausar {
				defer func() {
					if pausar { // pode ter sido desligado no caminho
						esperarEnter(&err)
					}
				}()
			}
			// Trocar a chave e reiniciar o serviço exigem administrador: no
			// Windows, o comando se reabre elevado, como o `uninstall`.
			if reaberto, eerr := aplicativos.ElevarSeNecessario(); eerr != nil {
				return eerr
			} else if reaberto {
				pausar = false
				return nil
			}
			configFile, err := configDoAgente(cmd, configFile, serviceName)
			if err != nil {
				return err
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if cfg.StorageBucket == "" {
				return fmt.Errorf("%s não tem STORAGE_BUCKET: instale o agente pelo comando do painel", configFile)
			}
			t, err := terminal.Open()
			if err != nil {
				return err
			}
			defer t.Close()
			ak, sk, err := setup.PerguntarETestar(cmd.Context(), t, cfg)
			if err != nil {
				return err
			}
			if err := setup.Gravar(configFile, setup.Chaves(ak, sk)); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "  ✓ Gravado em", configFile)
			escopo := service.Scope(serviceScope)
			if !reiniciar {
				fmt.Fprintln(os.Stderr, "  Reinicie o serviço para usar a chave nova:", service.RestartCommand(serviceName, escopo))
				return nil
			}
			a := service.RestartArgs(serviceName, escopo)
			if out, err := exec.CommandContext(cmd.Context(), a[0], a[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("a chave foi gravada, mas o serviço não reiniciou (%v: %s): rode %s",
					err, strings.TrimSpace(string(out)), service.RestartCommand(serviceName, escopo))
			}
			fmt.Fprintln(os.Stderr, "  ✓ Serviço reiniciado com a chave nova.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "arquivo de configuração")
	cmd.Flags().BoolVar(&reiniciar, "restart", false, "reiniciar o serviço depois de gravar")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço a reiniciar")
	cmd.Flags().StringVar(&serviceScope, "service-scope", "", "system ou user, como na instalação. Padrão: system se root, senão user")
	cmd.Flags().BoolVar(&pausar, "pause", false, "esperar um Enter antes de sair (janela aberta pelo painel)")
	var elevado bool
	cmd.Flags().BoolVar(&elevado, "elevado", false, "")
	_ = cmd.Flags().MarkHidden("elevado")
	return cmd
}

// configDoServico é o leitor do registro do serviço; os testes o trocam.
var configDoServico = service.ConfigDoServico

// configDoAgente decide de qual agente é o arquivo de configuração. Com
// --service-name de outro agente e sem --config, o arquivo é o que o serviço
// dele usa (unit, plist, SCM). Antes ficava o padrão — o do agente principal:
// set-storage-keys testava e gravava a chave no agente errado, e o uninstall
// apagava a configuração do vizinho.
func configDoAgente(cmd *cobra.Command, configFile, serviceName string) (string, error) {
	if cmd.Flags().Changed("config") || serviceName == "" || serviceName == service.DefaultName {
		return configFile, nil
	}
	c, ok := configDoServico(serviceName)
	if !ok || c == "" {
		return "", fmt.Errorf("não achei o arquivo de configuração do serviço %s: use --config com o caminho da instalação dele", serviceName)
	}
	return c, nil
}
