package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/enrollment"
	"github.com/arkame-app/agent/internal/service"
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
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Registra o agente no painel (first-time ou re-enrollment)",
		Long: `Fluxo de enrollment:
  1. Gera um keypair Ed25519 local
  2. Envia public_key + fingerprint ao painel com o --token (enrollment_token)
  3. Imprime fingerprint e link do painel para o usuário aprovar
  4. (default) Faz long-poll aguardando aprovação e recebe um JWT bearer
  5. Salva o JWT em /etc/arkame/token.jwt (0600)
  6. Opcionalmente instala como serviço do SO (--install-service, default: true)

Um host pode rodar mais de um agent — um por conjunto de credenciais de
storage. Nesse caso dê um nome a cada um com --service-name (arkame-agent-aws,
arkame-agent-oci, ...) e aponte --config para o env-file correspondente.

Em re-enrollment (trocar servidor físico / reinstalar), o token novo é
gerado no painel clicando "Reinstalar" em /agents/:id — ele é amarrado ao
agent_id existente, preservando histórico e path no bucket.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(configFile, config.Overrides{
				EnrollmentToken: enrollmentToken,
				PanelURL:        panelURL,
			})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}

			if cfg.EnrollmentToken == "" {
				return fmt.Errorf("enrollment token é obrigatório (--token ou ENROLLMENT_TOKEN no env-file)")
			}
			if cfg.PanelURL == "" {
				return fmt.Errorf("panel URL é obrigatório (--panel-url ou PANEL_URL)")
			}

			result, err := enrollment.Run(ctx, cfg, enrollment.Options{
				Hostname:      hostName,
				OS:            runtime.GOOS + "-" + runtime.GOARCH,
				InstallMethod: "binary",
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

			if waitApproval {
				fmt.Fprintln(os.Stderr, "  Aguardando aprovação (Ctrl-C cancela)...")
				tok, err := enrollment.WaitForApproval(ctx, cfg, result.WaitURL)
				if err != nil {
					return fmt.Errorf("aguardando aprovação: %w", err)
				}
				if err := enrollment.PersistToken(cfg, tok.AgentToken); err != nil {
					return err
				}
				slog.Info("aprovado — token persistido", "expires_at", tok.ExpiresAt)
				fmt.Fprintln(os.Stderr, "  ✓ Aprovado. Token válido até", tok.ExpiresAt.Format("2006-01-02"))
			}

			if installService {
				inst, err := service.Install(ctx, cfg, service.Options{
					Name:  serviceName,
					Scope: service.Scope(serviceScope),
					Start: true,
				})
				if err != nil {
					return fmt.Errorf("instalando serviço do SO: %w", err)
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
				fmt.Fprintln(os.Stderr, "")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", "/etc/arkame/agent.env", "caminho do env-file com credenciais de storage")
	cmd.Flags().StringVar(&enrollmentToken, "token", "", "enrollment_token gerado no painel (ex: atk_...)")
	cmd.Flags().StringVar(&panelURL, "panel-url", "https://save.arkame.app", "URL base do painel Arkame")
	cmd.Flags().BoolVar(&installService, "install-service", true, "instalar como serviço systemd/launchd/Windows Service (set false para só enrollar)")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço — use um por credencial de storage no mesmo host (ex.: arkame-agent-aws)")
	cmd.Flags().StringVar(&serviceScope, "service-scope", "", "system (todo o host, exige root) ou user (só o seu usuário, sem sudo). Padrão: system se root, senão user")
	cmd.Flags().StringVar(&hostName, "hostname", "", "hostname reportado (default: hostname do sistema)")
	cmd.Flags().BoolVar(&waitApproval, "wait", true, "aguardar aprovação humana (long-poll). --wait=false retorna logo após enrollment")

	// Mantém alias antigo para compatibilidade com docs.
	cmd.Flags().StringVar(&enrollmentToken, "enrollment-token", "", "(alias) enrollment_token")
	_ = cmd.Flags().MarkHidden("enrollment-token")

	return cmd
}

// mantém contexto disponível para testes
var _ = context.Background
