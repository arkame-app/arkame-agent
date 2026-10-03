package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/enrollment"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	var (
		configFile      string
		enrollmentToken string
		panelURL        string
		installService  bool
		serviceName     string
		serviceScope    string
		hostName        string
		waitApproval    bool
		checkStorage    bool
		pausar          bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Registra o agente no painel (first-time ou re-enrollment)",
		Long: `Fluxo de enrollment:
  1. Gera um keypair Ed25519 local
  2. Envia public_key + fingerprint ao painel com o --token (enrollment_token)
  3. Imprime fingerprint e link do painel para o usuário aprovar
  4. Faz long-poll aguardando aprovação e recebe um JWT bearer
  5. Aprovado, grava a identidade nova de uma vez: a chave privada, o
     agent.id, o AGENT_ID no env-file e o JWT em /etc/arkame/token.jwt (0600).
     Sem aprovação, nada disso muda no disco
  6. Com o token gravado, instala como serviço do SO (--install-service,
     default: true)

A espera pela aprovação faz parte do install: a identidade nova só existe em
memória até lá, e não há como concluí-la depois noutro processo.

Um host pode rodar mais de um agent — um por conjunto de credenciais de
storage. Nesse caso dê um nome a cada um com --service-name (arkame-agent-aws,
arkame-agent-oci, ...) e aponte --config para o env-file correspondente.

Em re-enrollment (trocar servidor físico / reinstalar), o token novo é
gerado no painel clicando "Reinstalar" em /agents/:id — ele é amarrado ao
agent_id existente, preservando histórico e path no bucket.`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if pausar {
				// Aberto pelo `setup` do Windows numa janela própria: ela espera
				// o Enter, com o resultado — inclusive o erro — à vista.
				defer func() {
					if err == nil {
						fmt.Fprintln(os.Stderr, "\n  ✓ Pronto. O painel mostra o servidor e o teste do bucket.")
					}
					esperarEnter(&err)
				}()
			}
			// --wait=false deixava um enrollment que nada concluía (a identidade
			// nova só existe em memória até a aprovação) e, com o serviço,
			// subia um daemon sem token, em laço de "não aprovado". Numa
			// reinstalação, aprovar esse código revogava o token antigo e
			// derrubava o agente em execução.
			if !waitApproval {
				return errSemEspera
			}
			// Nome do serviço conferido antes do registro: recusado só no fim,
			// depois da aprovação, deixaria um servidor aprovado sem serviço.
			if installService {
				if err := service.ValidarNome(serviceName); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			// Caminho absoluto: no Windows o padrão (/etc/arkame/agent.env) não
			// tem unidade, e o serviço recebe exatamente este texto.
			if abs, err := filepath.Abs(configFile); err == nil {
				configFile = abs
			}

			carregar := func() (*config.Config, error) {
				c, err := config.Load(configFile, config.Overrides{
					EnrollmentToken: enrollmentToken,
					PanelURL:        panelURL,
				})
				if err != nil {
					return nil, fmt.Errorf("carregando config: %w", err)
				}
				return c, nil
			}
			cfg, err := carregar()
			if err != nil {
				return err
			}

			if cfg.EnrollmentToken == "" {
				return fmt.Errorf("enrollment token é obrigatório (--token ou ENROLLMENT_TOKEN no env-file)")
			}
			if cfg.PanelURL == "" {
				return fmt.Errorf("panel URL é obrigatório (--panel-url ou PANEL_URL)")
			}

			// A chave do bucket, testada, antes de registrar: servidor
			// registrado com chave errada falharia no primeiro backup, calado.
			if checkStorage {
				mudou, err := garantirCredencial(ctx, cfg, configFile)
				if err != nil {
					return err
				}
				if mudou {
					if cfg, err = carregar(); err != nil {
						return err
					}
				}
			}

			// Agente com --config próprio (mais de um na máquina): token, chave
			// e agent.id ao lado do arquivo dele, e não nos padrões que o
			// agente da configuração padrão também usa.
			if mudou, err := identidadePropria(configFile, cfg); err != nil {
				return err
			} else if mudou {
				if cfg, err = carregar(); err != nil {
					return err
				}
			}

			result, err := enrollment.Run(ctx, cfg, enrollment.Options{
				Hostname:      hostName,
				OS:            runtime.GOOS + "-" + runtime.GOARCH,
				InstallMethod: metodoDeInstalacao(),
			})
			if err != nil {
				return fmt.Errorf("enrollment: %w", err)
			}

			slog.Info("enrollment pendente — aguardando aprovação no painel",
				"fingerprint", result.Fingerprint,
				"agent_id", result.AgentID)
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  Fingerprint:", result.Fingerprint)
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  Aprove em", cfg.PanelURL+"/agents")
			fmt.Fprintln(os.Stderr, "")

			fmt.Fprintln(os.Stderr, "  Aguardando aprovação (Ctrl-C cancela)...")
			tok, err := enrollment.WaitForApproval(ctx, cfg, result)
			if err != nil {
				return fmt.Errorf("aguardando aprovação: %w", err)
			}
			if err := enrollment.Concluir(cfg, result, tok.AgentToken); err != nil {
				return err
			}
			slog.Info("aprovado — token persistido", "expires_at", tok.ExpiresAt)
			fmt.Fprintln(os.Stderr, "  ✓ Aprovado. Token válido até", tok.ExpiresAt.Format("2006-01-02"))

			if installService {
				// Serviço sem token só gira em "não aprovado": não instala.
				if !cfg.TokenExists() {
					return fmt.Errorf("sem token em %s: o serviço não foi instalado", cfg.TokenPath)
				}
				inst, err := instalarServico(ctx, cfg, service.Options{
					Name:  serviceName,
					Scope: service.Scope(serviceScope),
					Start: true,
				})
				if err != nil {
					return fmt.Errorf("instalando serviço do SO: %w", err)
				}
				// No Windows, aparece em "Aplicativos instalados", com o
				// Desinstalar chamando `uninstall`.
				if exe, err := os.Executable(); err == nil {
					var argsDoUninstall []string
					if serviceName != service.DefaultName {
						argsDoUninstall = append(argsDoUninstall, "--service-name", serviceName)
					}
					if configFile != config.DefaultPath {
						argsDoUninstall = append(argsDoUninstall, "--config", configFile)
					}
					if err := aplicativos.Registrar(exe, version.Version, serviceName, argsDoUninstall); err != nil {
						slog.Warn("não consegui registrar em Aplicativos instalados", "err", err)
					}
				}
				slog.Info("serviço instalado e iniciado",
					"os", runtime.GOOS, "name", inst.Name, "scope", string(inst.Scope))

				// O operador precisa sair daqui sabendo o comando exato de
				// reinício — caçar o nome do serviço depois já custou caro.
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
				if passo := passoDoAcessoTotal(runtime.GOOS, inst); passo != "" {
					fmt.Fprintln(os.Stderr, "")
					fmt.Fprintln(os.Stderr, passo)
				}
				fmt.Fprintln(os.Stderr, "")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "caminho do env-file com credenciais de storage")
	cmd.Flags().StringVar(&enrollmentToken, "token", "", "enrollment_token gerado no painel (ex: atk_...)")
	// Padrão vazio: com a URL aqui, a flag sempre vencia o PANEL_URL do
	// env-file (painel de parceiro whitelabel). O padrão vem do config.
	cmd.Flags().StringVar(&panelURL, "panel-url", "", "URL base do painel Arkame. Padrão: PANEL_URL, senão https://save.arkame.app")
	cmd.Flags().BoolVar(&installService, "install-service", true, "instalar como serviço systemd/launchd/Windows Service (set false para só enrollar)")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço — use um por credencial de storage no mesmo host (ex.: arkame-agent-aws)")
	cmd.Flags().StringVar(&serviceScope, "service-scope", "", "system (todo o host, exige root) ou user (só o seu usuário, sem sudo). Padrão: system se root, senão user")
	cmd.Flags().StringVar(&hostName, "hostname", "", "hostname reportado (default: hostname do sistema)")
	cmd.Flags().BoolVar(&pausar, "pause", false, "esperar um Enter antes de sair (janela aberta pelo setup no Windows)")
	cmd.Flags().BoolVar(&checkStorage, "check-storage", true, "testar a chave do bucket antes de registrar (sem chave no arquivo, pergunta no terminal)")
	// Escondida: o install sempre espera a aprovação. Fica só para que
	// --wait=false pare com a explicação em vez de "unknown flag".
	cmd.Flags().BoolVar(&waitApproval, "wait", true, "(sem efeito) o install sempre aguarda a aprovação; --wait=false não é aceito")
	_ = cmd.Flags().MarkHidden("wait")

	// Mantém alias antigo para compatibilidade com docs.
	cmd.Flags().StringVar(&enrollmentToken, "enrollment-token", "", "(alias) enrollment_token")
	_ = cmd.Flags().MarkHidden("enrollment-token")

	return cmd
}

// identidadePropria grava no env-file caminhos de identidade derivados dele
// (agent-oci.env → agent-oci.token.jwt, agent-oci.key.pem, agent-oci.agent.id)
// quando o arquivo não é o padrão e o caminho ainda é o padrão. Sem isso, o
// segundo agente da máquina gravava a chave e o token por cima dos do
// primeiro. Caminho já definido (no arquivo ou no ambiente) não muda, nem o
// agente da configuração padrão.
func identidadePropria(configFile string, cfg *config.Config) (bool, error) {
	padrao, err := filepath.Abs(config.DefaultPath)
	if err != nil || chaveDeCaminho(configFile) == chaveDeCaminho(padrao) {
		return false, nil
	}
	base := strings.TrimSuffix(configFile, filepath.Ext(configFile))
	var linhas []string
	if cfg.TokenPath == config.DefaultTokenPath {
		linhas = append(linhas, "TOKEN_PATH="+base+".token.jwt")
	}
	if cfg.PrivateKeyPath == config.DefaultPrivateKeyPath {
		linhas = append(linhas, "PRIVATE_KEY_PATH="+base+".key.pem")
	}
	if cfg.AgentIDPath == config.DefaultAgentIDPath {
		linhas = append(linhas, "AGENT_ID_PATH="+base+".agent.id")
	}
	if len(linhas) == 0 {
		return false, nil
	}
	if err := setup.Gravar(configFile, linhas); err != nil {
		return false, fmt.Errorf("gravando os caminhos da identidade em %s: %w", configFile, err)
	}
	return true, nil
}

// passoDoAcessoTotal é o passo que falta no macOS: sem o Acesso Total ao
// Disco, o serviço (LaunchDaemon ou LaunchAgent) não lê Mesa, Documentos,
// Downloads nem o iCloud Drive — o TCC devolve "operation not permitted" e o
// backup sai parcial sem dizer o que fazer. Fora do macOS, vazio.
func passoDoAcessoTotal(goos string, inst *service.Installed) string {
	if goos != "darwin" {
		return ""
	}
	programa := inst.Programa
	if programa == "" {
		programa = "/usr/local/bin/arkame-agent"
		if inst.Scope == service.ScopeUser {
			programa = "~/.local/bin/arkame-agent"
		}
	}
	return "  ⚠ Falta um passo no macOS: dê Acesso Total ao Disco ao agente.\n" +
		"    Ajustes do Sistema → Privacidade e Segurança → Acesso Total ao Disco → +\n" +
		"    e escolha " + programa + " (no seletor, Cmd+Shift+G e cole o caminho).\n" +
		"    Depois reinicie o serviço: " + inst.StartCmd + "\n" +
		"    Sem isso, Mesa, Documentos, Downloads e iCloud Drive ficam de fora do backup."
}

// errSemEspera: --wait=false não é aceito.
var errSemEspera = errors.New("--wait=false não é aceito: a identidade nova só vai ao disco com a aprovação, " +
	"e nada a concluiria depois. Rode o install sem --wait e aprove o servidor no painel enquanto ele espera")

// instalarServico é o service.Install; os testes o trocam.
var instalarServico = service.Install

// mantém contexto disponível para testes
var _ = context.Background

// metodoDeInstalacao: "docker" dentro da imagem (a variável vem no
// Dockerfile), "binary" no resto. O painel escolhe os comandos por isto — um
// servidor em Docker recebia os comandos do Linux, que ali não fazem nada.
func metodoDeInstalacao() string {
	if m := os.Getenv("ARKAME_INSTALL_METHOD"); m != "" {
		return m
	}
	return "binary"
}
