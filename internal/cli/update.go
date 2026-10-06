package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/daemon"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

// respostaDoUpdate é a resposta de GET /api/agents/{id}/update: a versão
// publicada para este agente, ou nada.
type respostaDoUpdate struct {
	Update *api.UpdateOffer `json:"update"`
}

// newUpdateCmd é a atualização pedida à mão, com as mesmas conferências da
// automática. Serve à organização que escolheu a política manual, ao servidor
// "segurado" numa versão e a quem não quer esperar a janela.
func newUpdateCmd() *cobra.Command {
	var (
		configFile, serviceName, serviceScope string
		versao, sha256, baseURL               string
		permitirReversao, semReiniciar        bool
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Atualiza o programa do agente agora, com as conferências da atualização automática",
		Long: `Baixa a versão publicada, confere e põe no lugar do programa atual:

  1. pergunta ao painel qual é a versão publicada e o SHA-256 dela;
  2. baixa o checksums.txt do release e confere a assinatura Ed25519 com as
     chaves embutidas neste programa;
  3. o SHA-256 do checksums.txt assinado tem de ser o que o painel informou,
     e o do arquivo baixado tem de ser esse;
  4. roda o self-test do programa novo;
  5. troca (o atual fica ao lado, como .anterior) e reinicia os serviços que
     usam o programa.

O programa novo tem cinco minutos para um heartbeat aceito pelo painel; sem
isso, o anterior volta sozinho.

Ignora a janela e a política de atualização da organização: quem roda o
comando decidiu atualizar agora. Reiniciar o serviço interrompe um backup em
curso (ele recomeça no próximo horário).

Sem acesso ao painel, informe a versão e o SHA-256 que o painel mostra em
Servidores: --version e --sha256.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			configFile, err := configDoAgente(cmd, configFile, serviceName, serviceScope)
			if err != nil {
				return err
			}
			cfg, err := config.Load(configFile, config.Overrides{})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			return atualizarAgora(cmd.Context(), cmd.ErrOrStderr(), cfg, pedidoManual{
				versao: versao, sha256: sha256, baseURL: baseURL,
				permitirReversao: permitirReversao, semReiniciar: semReiniciar,
			})
		},
	}
	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "env-file")
	cmd.Flags().StringVar(&versao, "version", "", "versão a instalar (padrão: a publicada pelo painel para este servidor)")
	cmd.Flags().StringVar(&sha256, "sha256", "", "SHA-256 do programa desta versão, como o painel mostra (dispensa a consulta ao painel; exige --version)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "de onde baixar (padrão: o endereço que o painel informar, depois o GitHub)")
	cmd.Flags().BoolVar(&permitirReversao, "allow-downgrade", false, "aceitar versão menor ou igual à atual (com --version)")
	cmd.Flags().BoolVar(&semReiniciar, "no-restart", false, "trocar o programa sem reiniciar os serviços")
	flagsDoServico(cmd, &serviceName, &serviceScope)
	return cmd
}

type pedidoManual struct {
	versao, sha256, baseURL        string
	permitirReversao, semReiniciar bool
}

// aplicarAtualizacao e reiniciarServico são trocáveis nos testes.
var (
	aplicarAtualizacao = atualizacao.Aplicar
	reiniciarServico   = service.Reiniciar
	servicosDoPrograma = service.RodandoOPrograma
)

func atualizarAgora(ctx context.Context, w io.Writer, cfg *config.Config, m pedidoManual) error {
	if m.sha256 != "" && m.versao == "" {
		return errors.New("--sha256 exige --version: o resumo é de uma versão")
	}
	pedido := atualizacao.Pedido{
		Versao:           m.versao,
		Sha256:           m.sha256,
		BaseURL:          m.baseURL,
		Forcar:           true,
		PermitirReversao: m.permitirReversao,
	}
	if pedido.Sha256 == "" {
		oferta, err := consultarPainel(ctx, cfg, m.versao)
		if err != nil {
			return err
		}
		if oferta == nil {
			fmt.Fprintln(w, "  Nada a atualizar: o painel não tem versão publicada mais nova que a", version.Version, "para este servidor.")
			return nil
		}
		pedido.Versao, pedido.Sha256, pedido.Arquivo = oferta.Version, oferta.SHA256, oferta.File
		if pedido.BaseURL == "" {
			pedido.BaseURL = oferta.BaseURL
		}
	}

	programa := service.ProgramaAtual()
	emDocker := atualizacao.EmDocker()
	if emDocker {
		programa = atualizacao.ProgramaDoVolume(cfg.ConfigPath)
		if err := os.MkdirAll(filepath.Dir(programa), 0o755); err != nil {
			return fmt.Errorf("criando a pasta do programa no volume: %w", err)
		}
	}
	fmt.Fprintf(w, "  Atualizando %s: %s → %s\n", programa, version.Version, pedido.Versao)

	// Anotados antes da troca: depois dela, quem roda o programa segue com o
	// arquivo antigo até reiniciar.
	rodando := servicosDoPrograma(programa)

	var args []string
	if s3c, err := storage.NewS3Client(ctx, cfg); err == nil {
		args = daemon.ArgsDoAutoTeste(ctx, cfg, s3c)
	} else {
		args = daemon.ArgsDoAutoTeste(ctx, cfg, nil)
	}
	tctx, cancel := context.WithTimeout(ctx, atualizacao.PrazoDaTroca)
	defer cancel()
	res, err := aplicarAtualizacao(tctx, pedido, atualizacao.Opcoes{
		Programa:        programa,
		VersaoAtual:     version.Version,
		Chaves:          atualizacao.ChavesConfiaveis(),
		Reserva:         atualizacao.ReleasesNoGitHub,
		ArgsDoAutoTeste: args,
		PrepararNovo:    service.PrepararProgramaNovo,
		PeloLancador:    emDocker,
	})
	var adiada *atualizacao.Adiada
	switch {
	case errors.As(err, &adiada):
		return fmt.Errorf("há outra atualização em curso nesta máquina (%s): tente de novo em alguns minutos", adiada.Motivo)
	case err != nil:
		return fmt.Errorf("a atualização não foi feita, e o programa atual continua no lugar: %w", err)
	}
	if res.JaNoLugar {
		fmt.Fprintln(w, "  ✓ O programa da versão", res.Versao, "já estava no lugar")
	} else {
		fmt.Fprintln(w, "  ✓ Programa novo conferido e no lugar:", res.Versao)
	}

	switch {
	case emDocker:
		fmt.Fprintln(w, "  Reinicie o container para o programa novo valer (docker restart <container>).")
	case m.semReiniciar:
		fmt.Fprintln(w, "  --no-restart: os serviços seguem no programa antigo até reiniciarem.")
	case len(rodando) == 0:
		fmt.Fprintln(w, "  Nenhum serviço em execução usa este programa: ele vale na próxima partida.")
	default:
		reiniciarOutrosDoPrograma(w, rodando, "", reiniciarServico)
		fmt.Fprintln(w, "  O programa novo tem", atualizacao.PrazoDaProva, "para um heartbeat aceito pelo painel; sem isso, o anterior volta sozinho.")
	}
	return nil
}

// consultarPainel pergunta ao painel a versão publicada para este agente (ou
// a versão pedida) e o SHA-256 dela. nil, sem erro: nada a atualizar.
func consultarPainel(ctx context.Context, cfg *config.Config, versao string) (*api.UpdateOffer, error) {
	if cfg.AgentID == "" {
		return nil, fmt.Errorf("agente sem identidade em %s: rode 'arkame-agent install' primeiro", cfg.ConfigPath)
	}
	bearer, err := cfg.LoadToken()
	if err != nil {
		return nil, fmt.Errorf("lendo o token: %w", err)
	}
	if bearer == "" {
		return nil, fmt.Errorf("sem token em %s: rode 'arkame-agent install' primeiro (ou com sudo, se o agente é do sistema)", cfg.TokenPath)
	}
	client, err := api.New(api.Options{BaseURL: cfg.PanelURL, Bearer: bearer, Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	caminho := "/api/agents/" + cfg.AgentID + "/update"
	if versao != "" {
		caminho += "?version=" + url.QueryEscape(versao)
	}
	var resp respostaDoUpdate
	if err := client.GET(ctx, caminho, &resp); err != nil {
		var he *api.HTTPError
		if errors.As(err, &he) && he.Status == 404 {
			return nil, fmt.Errorf("o painel não conhece essa versão para este servidor (ou ainda não publica versões): %w", err)
		}
		return nil, fmt.Errorf("consultando o painel: %w\n     Sem acesso ao painel, informe --version e --sha256 (o painel os mostra em Servidores)", err)
	}
	return resp.Update, nil
}
