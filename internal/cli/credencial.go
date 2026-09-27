package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

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
func garantirCredencial(ctx context.Context, cfg *config.Config, caminho string) error {
	semChave := cfg.StorageAccessKey == "" || cfg.StorageSecretKey == ""

	if !semChave {
		err := storage.Check(ctx, cfg)
		if err == nil {
			fmt.Fprintln(os.Stderr, "  ✓ O bucket", cfg.StorageBucket, "aceitou a chave de", caminho)
			return nil
		}
		t, terr := terminal.Open()
		if terr != nil {
			return fmt.Errorf("%s (%v). Corrija a chave em %s, ou rode `arkame-agent set-storage-keys` num terminal", storage.Causa(err), err, caminho)
		}
		defer t.Close()
		fmt.Fprintf(os.Stderr, "\n  ✗ A chave de %s não funciona: %s.\n", caminho, storage.Causa(err))
		ak, sk, err := setup.PerguntarETestar(ctx, t, cfg, os.Stderr)
		if err != nil {
			return err
		}
		return setup.TrocarChaves(caminho, ak, sk)
	}

	if cfg.EnrollmentToken == "" {
		return fmt.Errorf("sem chave do bucket em %s", caminho)
	}
	t, err := terminal.Open()
	if err != nil {
		return fmt.Errorf("sem chave do bucket em %s e sem terminal para perguntar: rode o comando num terminal interativo (no Docker, com -it)", caminho)
	}
	defer t.Close()

	p, err := setup.BuscarNoPainel(ctx, cfg.PanelURL, cfg.EnrollmentToken)
	if err != nil {
		return err
	}
	base := *cfg
	base.StorageBucket = p.Armazenamento.Bucket
	base.StorageEndpoint = ""
	if p.Armazenamento.Endpoint != nil {
		base.StorageEndpoint = *p.Armazenamento.Endpoint
	}
	base.StorageRegion = "us-east-1"
	if p.Armazenamento.Region != nil && *p.Armazenamento.Region != "" {
		base.StorageRegion = *p.Armazenamento.Region
	}
	fmt.Fprintf(os.Stderr, "\n  Servidor:       %s\n  Armazenamento:  %s (%s)\n", p.DisplayName, p.Armazenamento.DisplayName, p.Armazenamento.Bucket)

	ak, sk, err := setup.PerguntarETestar(ctx, t, &base, os.Stderr)
	if err != nil {
		return err
	}
	if err := setup.Gravar(caminho, setup.Linhas(p, cfg.PanelURL), ak, sk); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "  ✓ Gravado em", caminho)
	return nil
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
	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "arquivo de configuração")
	return cmd
}

func newSetStorageKeysCmd() *cobra.Command {
	var (
		configFile  string
		reiniciar   bool
		pausar      bool
		serviceName string
	)
	cmd := &cobra.Command{
		Use:   "set-storage-keys",
		Short: "Troca a chave do bucket: pergunta, testa e só grava se o bucket aceitar",
		Long: `Pergunta a chave de acesso e a senha, testa no bucket do arquivo de
configuração e só então grava. Depois, reinicie o serviço para ele usar a
chave nova (o comando aparece no fim).`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if pausar {
				// Aberto pelo painel numa janela própria (Windows + R): sem
				// isto a janela fecha antes de a pessoa ler o resultado.
				defer func() {
					if err != nil {
						fmt.Fprintln(os.Stderr, "\n  ✗", err)
					}
					fmt.Fprint(os.Stderr, "\n  Pressione Enter para fechar.")
					if t, terr := terminal.Open(); terr == nil {
						_, _ = t.Pergunta("")
						t.Close()
					}
				}()
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
			ak, sk, err := setup.PerguntarETestar(cmd.Context(), t, cfg, os.Stderr)
			if err != nil {
				return err
			}
			if err := setup.TrocarChaves(configFile, ak, sk); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "  ✓ Gravado em", configFile)
			if !reiniciar {
				fmt.Fprintln(os.Stderr, "  Reinicie o serviço para usar a chave nova:", comandoDeReinicio(serviceName))
				return nil
			}
			if err := reiniciarServico(cmd.Context(), serviceName); err != nil {
				return fmt.Errorf("a chave foi gravada, mas o serviço não reiniciou (%v): rode %s", err, comandoDeReinicio(serviceName))
			}
			fmt.Fprintln(os.Stderr, "  ✓ Serviço reiniciado com a chave nova.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "arquivo de configuração")
	cmd.Flags().BoolVar(&reiniciar, "restart", false, "reiniciar o serviço depois de gravar")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço a reiniciar")
	cmd.Flags().BoolVar(&pausar, "pause", false, "esperar um Enter antes de sair (janela aberta pelo painel)")
	return cmd
}

// argumentosDeReinicio do serviço, por sistema — o mesmo que `service install`
// mostra no fim.
func argumentosDeReinicio(nome string) []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"powershell", "-NoProfile", "-Command", "Restart-Service " + nome}
	case "darwin":
		return []string{"launchctl", "kickstart", "-k", "system/" + service.LaunchdLabel(nome)}
	default:
		return []string{"systemctl", "restart", nome}
	}
}

func comandoDeReinicio(nome string) string {
	a := argumentosDeReinicio(nome)
	if runtime.GOOS == "windows" {
		return a[len(a)-1]
	}
	return "sudo " + strings.Join(a, " ")
}

func reiniciarServico(ctx context.Context, nome string) error {
	a := argumentosDeReinicio(nome)
	out, err := exec.CommandContext(ctx, a[0], a[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
