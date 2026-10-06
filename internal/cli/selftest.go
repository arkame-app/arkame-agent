package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

// newSelfTestCmd é o auto-teste do programa: a atualização automática o roda
// no programa NOVO, com a configuração deste agente, antes de pô-lo no lugar
// do atual. Sai com 0 só se o programa carrega a configuração, fala com o
// painel e monta o cliente do bucket; qualquer outra saída mantém o programa
// atual.
func newSelfTestCmd() *cobra.Command {
	var (
		configFile, serviceName, serviceScope string
		exigirBucket                          bool
	)
	cmd := &cobra.Command{
		Use:   "self-test",
		Short: "Confere que este programa funciona com a configuração do agente (usado antes de uma atualização)",
		Long: `Roda o que o daemon precisa para trabalhar, uma vez, sem mudar nada:
  - carrega o arquivo de configuração e o token;
  - manda um heartbeat de teste ao painel (o painel não o registra);
  - monta o cliente do bucket e consulta o versionamento.

A atualização automática roda este comando no programa novo antes da troca:
com saída diferente de zero, o programa atual continua. A consulta ao bucket
só reprova com --require-bucket (a atualização o passa quando o bucket
responde ao programa atual).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
			if err != nil {
				return err
			}
			return autoTeste(cmd.Context(), cmd.OutOrStdout(), configFile, exigirBucket)
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "env-file")
	cmd.Flags().BoolVar(&exigirBucket, "require-bucket", false, "reprovar se o bucket não responder")
	flagsDoServico(cmd, &serviceName, &serviceScope)
	return cmd
}

func autoTeste(ctx context.Context, w io.Writer, configFile string, exigirBucket bool) error {
	// A primeira linha é a versão: a troca confere que o programa novo é da
	// versão pedida (atualizacao.LinhaDaVersao).
	fmt.Fprintln(w, atualizacao.LinhaDaVersao+version.Version)

	cfg, err := config.Load(configFile, config.Overrides{})
	if err != nil {
		return fmt.Errorf("carregando config: %w", err)
	}
	if cfg.AgentID == "" {
		return fmt.Errorf("agente sem identidade em %s", configFile)
	}
	bearer, err := cfg.LoadToken()
	if err != nil {
		return fmt.Errorf("lendo o token: %w", err)
	}
	if bearer == "" {
		return fmt.Errorf("sem token em %s", cfg.TokenPath)
	}
	fmt.Fprintln(w, "configuração ok:", configFile)

	client, err := api.New(api.Options{BaseURL: cfg.PanelURL, Bearer: bearer, Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	hb := api.HeartbeatRequest{
		AgentID:      cfg.AgentID,
		AgentVersion: version.Version,
		OS:           runtime.GOOS + "-" + runtime.GOARCH,
		ReportedAt:   time.Now().UTC(),
		Capabilities: []string{},
		SelfTest:     true,
	}
	var resp api.HeartbeatResponse
	if err := client.POST(ctx, "/api/agents/"+cfg.AgentID+"/heartbeat", hb, &resp); err != nil {
		return fmt.Errorf("heartbeat de teste: %w", err)
	}
	fmt.Fprintln(w, "painel ok:", cfg.PanelURL)

	s3c, err := storage.NewS3Client(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cliente do bucket: %w", err)
	}
	if cfg.StorageBucket == "" {
		fmt.Fprintln(w, "bucket: sem STORAGE_BUCKET na configuração, consulta pulada")
		return nil
	}
	bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	estado, err := storage.VersioningStatus(bctx, s3c, cfg.StorageBucket)
	switch {
	case err == nil:
		fmt.Fprintln(w, "bucket ok:", cfg.StorageBucket, "versionamento", estado)
	case exigirBucket:
		return fmt.Errorf("consultando o bucket %s: %w", cfg.StorageBucket, err)
	default:
		fmt.Fprintf(w, "bucket: a consulta falhou e não foi exigida: %v\n", err)
	}
	return nil
}
