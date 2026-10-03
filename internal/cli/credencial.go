package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
//   - Com chave: pergunta ao painel também. Se o código aponta para outro
//     armazenamento (bucket, região ou endereço), testa a chave do arquivo no
//     armazenamento novo e grava o armazenamento novo inteiro; senão, testa no
//     do arquivo. Recusada, pede de novo (havendo terminal) ou para com a
//     causa.
//
// Antes, com chave, só o bucket do arquivo era testado; aprovado o servidor,
// o enrollment gravava o STORAGE_ID/STORAGE_BUCKET novos por cima, com a
// região, o endereço e a chave do armazenamento antigo — o primeiro backup
// falhava.
//
// Devolve se o arquivo mudou, para o chamador reler só nesse caso.
func garantirCredencial(ctx context.Context, cfg *config.Config, caminho string) (bool, error) {
	semChave := cfg.StorageAccessKey == "" || cfg.StorageSecretKey == ""

	if !semChave {
		alvo, linhas, err := armazenamentoDoCodigo(ctx, cfg)
		if err != nil {
			return false, err
		}
		if linhas != nil {
			if err := setup.PodeGravar(caminho); err != nil {
				return false, err
			}
		}
		err = storage.Check(ctx, alvo)
		if err == nil {
			fmt.Fprintln(os.Stderr, "  ✓ O bucket", alvo.StorageBucket, "aceitou a chave de", caminho)
			if linhas == nil {
				return false, nil
			}
			if err := setup.Gravar(caminho, linhas); err != nil {
				return false, err
			}
			fmt.Fprintln(os.Stderr, "  ✓ Armazenamento novo gravado em", caminho)
			return true, nil
		}
		t, terr := abrirTerminal()
		if terr != nil || storage.Classe(err) != "chave" {
			// Sem terminal, ou problema que outra chave não resolve.
			if linhas != nil {
				return false, fmt.Errorf("a chave de %s não serve para o bucket %s, que o código de instalação indica: %s (%v). "+
					"Rode o comando num terminal para digitar a chave desse bucket, ou grave-a em %s antes", caminho, alvo.StorageBucket, storage.Causa(err), err, caminho)
			}
			return false, fmt.Errorf("%s (%v). Arquivo: %s", storage.Causa(err), err, caminho)
		}
		defer t.Close()
		fmt.Fprintf(os.Stderr, "\n  ✗ A chave de %s não funciona no bucket %s: %s.\n", caminho, alvo.StorageBucket, storage.Causa(err))
		ak, sk, err := setup.PerguntarETestar(ctx, t, alvo)
		if err != nil {
			return false, err
		}
		return true, setup.Gravar(caminho, append(linhas, setup.Chaves(ak, sk)...))
	}

	if cfg.EnrollmentToken == "" {
		return false, fmt.Errorf("sem chave do bucket em %s", caminho)
	}
	if err := setup.PodeGravar(caminho); err != nil {
		return false, err
	}
	t, err := abrirTerminal()
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

// abrirTerminal é o terminal.Open; os testes o trocam.
var abrirTerminal = terminal.Open

// armazenamentoDoCodigo decide em que armazenamento testar a chave que já está
// no arquivo: no que o código de instalação indica, quando é outro (com as
// linhas para gravá-lo), ou no do arquivo (linhas nil). O AGENT_ID não vai
// junto: a identidade só muda com a aprovação (enrollment.Concluir).
func armazenamentoDoCodigo(ctx context.Context, cfg *config.Config) (*config.Config, []string, error) {
	if cfg.EnrollmentToken == "" {
		return cfg, nil, nil
	}
	p, err := setup.BuscarNoPainel(ctx, cfg.PanelURL, cfg.EnrollmentToken)
	if errors.Is(err, setup.ErrSemArmazenamento) {
		// Código sem armazenamento: vale o do arquivo.
		return cfg, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	novo := setup.NaConfig(p, cfg)
	if mesmoArmazenamento(cfg, novo) {
		return cfg, nil, nil
	}
	fmt.Fprintf(os.Stderr, "\n  O código de instalação aponta para o armazenamento %s (bucket %s); o arquivo tinha o bucket %s.\n",
		p.Armazenamento.DisplayName, novo.StorageBucket, orNA(cfg.StorageBucket))
	return novo, setup.LinhasDoArmazenamento(p), nil
}

// mesmoArmazenamento compara onde as duas configurações chegam: bucket,
// região e endereço.
func mesmoArmazenamento(a, b *config.Config) bool {
	return a.StorageBucket == b.StorageBucket &&
		a.StorageRegion == b.StorageRegion &&
		strings.TrimRight(a.StorageEndpoint, "/") == strings.TrimRight(b.StorageEndpoint, "/")
}

func newCheckStorageCmd() *cobra.Command {
	var configFile, serviceName, serviceScope string
	cmd := &cobra.Command{
		Use:   "check-storage",
		Short: "Testa se esta máquina alcança o bucket com a chave do arquivo de configuração",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Como no status: sem --config, o arquivo do serviço. Lia só o
			// padrão, e no agente sem root ou num segundo agente testava a
			// chave de outro (ou de nenhum) arquivo.
			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
			if err != nil {
				return err
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "  Arquivo:", configFile)
			if err := checarStorage(cmd.Context(), cfg); err != nil {
				return fmt.Errorf("✗ %s\n  (%v)", storage.Causa(err), err)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "  ✓ O bucket", cfg.StorageBucket, "aceitou a chave.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "arquivo de configuração")
	flagsDoServico(cmd, &serviceName, &serviceScope)
	return cmd
}

// checarStorage é o teste do bucket do check-storage; os testes o trocam.
var checarStorage = storage.Check

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
			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
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

// configDoAgente decide de qual agente é o arquivo de configuração. Sem
// --config, o arquivo é o que o serviço pedido usa (unit, plist, SCM), no
// escopo pedido. Antes ficava o padrão — o do agente principal: com
// --service-name de outro agente, set-storage-keys testava e gravava a chave
// no agente errado, e o uninstall apagava a configuração do vizinho.
//
// Vale também para o nome padrão: o agente rootless (--service-scope user)
// usa ~/.config/arkame/agent.env (config.UserPath), e o comando do painel não
// leva --config — lia /etc/arkame/agent.env, que não existe, e falhava. Só o
// nome padrão sem registro cai no arquivo padrão do escopo (configPadrao).
func configDoAgente(cmd *cobra.Command, configFile, serviceName, serviceScope string) (string, error) {
	if cmd.Flags().Changed("config") {
		return configFile, nil
	}
	if serviceName == "" {
		serviceName = service.DefaultName
	}
	if c, ok := configDoServico(serviceName, service.Scope(serviceScope)); ok && c != "" {
		return c, nil
	}
	if serviceName == service.DefaultName {
		return configPadrao(serviceScope), nil
	}
	return "", fmt.Errorf("não achei o arquivo de configuração do serviço %s: use --config com o caminho da instalação dele", serviceName)
}

// configPadrao é o arquivo de configuração sem --config. No escopo user (o
// padrão sem root, fora do Windows) é o config.UserPath —
// $XDG_CONFIG_HOME/arkame/agent.env ou ~/.config/arkame/agent.env —, e não
// /etc/arkame/agent.env: a instalação sem sudo parava em "permission denied"
// nele. No escopo system, config.DefaultPath.
func configPadrao(serviceScope string) string {
	if runtime.GOOS != "windows" && service.EscopoEfetivo(service.Scope(serviceScope)) == service.ScopeUser {
		if p, err := config.UserPath(); err == nil {
			return p
		}
	}
	return config.DefaultPath
}
