package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/crypto"
	"github.com/arkame-app/agent/internal/daemon"
	"github.com/arkame-app/agent/internal/service"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var configFile, serviceName, serviceScope string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Mostra o estado local do agente (identidade, fingerprint da chave e situação do registro)",
		Long: `Mostra o que está nesta máquina: agent_id, painel, fingerprint da chave
privada, caminhos e se o registro foi aprovado (pelo token gravado e o
vencimento dele). Não consulta o painel: o último sinal do servidor aparece
lá, na página do servidor.

Sem --config, lê o arquivo que o serviço usa (unit, plist, SCM): o do agente
sem root fica em ~/.config/arkame, e o de um segundo agente, onde a
instalação dele pôs.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// O arquivo padrão (/etc/arkame/agent.env) só vale para o agente
			// principal instalado como root: o sem root e um segundo agente
			// saíam "NÃO INICIADO — rode install", rodando.
			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
			if err != nil {
				return err
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return err
			}
			escreverStatus(cmd.OutOrStdout(), cfg, time.Now())
			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "env-file")
	flagsDoServico(cmd, &serviceName, &serviceScope)
	return cmd
}

// escreverStatus escreve o estado local. O registro vale pelo token no disco,
// não pelo ENROLLMENT_TOKEN: o código de instalação ficava no env-file depois
// da aprovação, e um agente aprovado aparecia "PENDENTE" para sempre.
func escreverStatus(w io.Writer, cfg *config.Config, agora time.Time) {
	fmt.Fprintln(w, "Config:       ", orNA(cfg.ConfigPath))
	fmt.Fprintln(w, "Agent ID:     ", orNA(cfg.AgentID))
	fmt.Fprintln(w, "Painel:       ", cfg.PanelURL)
	fmt.Fprintln(w, "Fingerprint:  ", fingerprintDaChave(cfg.PrivateKeyPath))
	fmt.Fprintln(w, "Token path:   ", orNA(cfg.TokenPath))
	fmt.Fprintln(w, "Host root:    ", cfg.HostRoot)
	fmt.Fprintln(w, "Storage:      ", orNA(cfg.StorageID))
	fmt.Fprintln(w, "Enrollment:   ", situacaoDoRegistro(cfg, agora))
}

// fingerprintDaChave é a fingerprint que o painel mostrou na aprovação,
// calculada da chave privada — nada a gravava no AGENT_FINGERPRINT, e o
// status dizia sempre "(não configurado)".
func fingerprintDaChave(caminho string) string {
	if caminho == "" {
		return orNA("")
	}
	kp, err := crypto.LoadPrivate(caminho)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "(sem chave em " + caminho + ")"
	case errors.Is(err, fs.ErrPermission):
		return "(sem permissão para ler " + caminho + ": rode com sudo)"
	case err != nil:
		return fmt.Sprintf("(chave ilegível em %s: %v)", caminho, err)
	}
	return crypto.Fingerprint(kp.Public)
}

func situacaoDoRegistro(cfg *config.Config, agora time.Time) string {
	tok, err := cfg.LoadToken()
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "DESCONHECIDO — sem permissão para ler " + cfg.TokenPath + " (rode com sudo)"
	case err != nil:
		return fmt.Sprintf("DESCONHECIDO — não consegui ler %s: %v", cfg.TokenPath, err)
	case tok != "":
		venc, ok := daemon.VencimentoDoToken(tok)
		switch {
		case !ok:
			return "APROVADO (token presente)"
		case !venc.After(agora):
			return "TOKEN VENCIDO em " + venc.Format("2006-01-02") + " — gere um código em Reinstalar no painel e rode o install com ele"
		default:
			return "APROVADO (token válido até " + venc.Format("2006-01-02") + ")"
		}
	case cfg.EnrollmentToken != "":
		return "PENDENTE — install não concluído (sem token; aprove no painel enquanto o install espera)"
	default:
		return "NÃO INICIADO — rode 'arkame-agent install'"
	}
}

// flagsDoServico são --service-name e --service-scope dos comandos que leem a
// configuração de um agente já instalado (configDoAgente).
func flagsDoServico(cmd *cobra.Command, nome, escopo *string) {
	cmd.Flags().StringVar(nome, "service-name", service.DefaultName, "nome do serviço do agente (para achar o arquivo de configuração dele)")
	cmd.Flags().StringVar(escopo, "service-scope", "", "system ou user, como na instalação. Padrão: system se root, senão user")
}

func orNA(s string) string {
	if s == "" {
		return "(não configurado)"
	}
	return s
}
