package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/daemon"
	"github.com/arkame-app/agent/internal/logarquivo"
	"github.com/arkame-app/agent/internal/segredo"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		configFile  string
		hostRoot    string
		serviceName string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inicia o daemon do agent (enrollment já concluído)",
		Long: `Loop principal do agent:
  - Busca os planos no painel a cada POLL_INTERVAL_SEC (padrão 60s)
  - Executa backups conforme schedule + janelas + throttling
  - Reporta heartbeat a cada HEARTBEAT_INTERVAL_SEC (padrão 60s) e grava o
    token novo que o painel mandar na resposta
  - Busca restaurações pendentes a cada POLL_INTERVAL_SEC
  - Testa o bucket de hora em hora e quando o painel pede (conferido a cada 30s)
  - Atende o explorador de pastas do painel (long-poll)
  - Aplica o expurgo de retenção autorizado pelo painel (de hora em hora)
  - Atualiza o próprio programa quando o painel manda (na resposta do
    heartbeat): baixa, confere a assinatura e o SHA-256, roda o self-test do
    programa novo, troca e sai para o gerenciador de serviço religar. Só com o
    bucket livre e sem restauração em curso. O programa novo tem 5 minutos
    para um heartbeat aceito; sem isso, o anterior volta (com o painel fora do
    ar, a prova é estendida até 30 minutos da troca). ARKAME_AUTO_UPDATE=off
    desliga (o comando 'update' continua valendo).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Docker: o programa da imagem passa a vez ao do volume, se houver
			// um válido e mais novo. Só volta daqui se não há, ou se falhou.
			lancarDoVolume(configFile)
			// Este processo é o `.anterior` de uma troca que parou no meio
			// (o caminho do programa ficou vazio): põe o programa de volta
			// antes de seguir. Ver atualizacao.Recuperar.
			recuperarSeSouOAnterior()
			cfg, err := config.Load(configFile, config.Overrides{
				HostRoot: hostRoot,
			})
			if err != nil {
				return fmt.Errorf("carregando config: %w", err)
			}
			if !cfg.TokenExists() {
				return fmt.Errorf("agent ainda não foi aprovado — rode 'arkame-agent install' primeiro")
			}
			if cfg.AgentID == "" {
				return fmt.Errorf("agent sem identidade: nem AGENT_ID em %s nem o arquivo %s — rode 'arkame-agent install' de novo", configFile, cfg.AgentIDPath)
			}
			// Programa novo em prova: conta esta partida. Na terceira sem um
			// heartbeat aceito, o anterior volta ao lugar e este processo sai
			// para o gerenciador de serviço subi-lo. Só depois de a
			// configuração carregar e de haver identidade e token: a partida
			// que para antes disso é de um agente que não subiria com programa
			// nenhum (o vizinho mal configurado, um `run` com o arquivo
			// errado), e contá-la desfazia a troca de quem está bem.
			if registrarArranque(service.ProgramaAtual(), version.Version, configFile) {
				// Sob o SCM do Windows, sai já com o serviço no ar: o processo
				// que morre antes de se apresentar ao SCM é uma partida que
				// falhou, e não um serviço que caiu — e só o segundo é religado.
				if handled, _ := service.RunAsService(serviceName, func(context.Context) error {
					sairParaReiniciar()
					return nil
				}); !handled {
					sairParaReiniciar()
				}
				return nil
			}
			// Instalações anteriores gravaram o token e a chave privada com
			// os.WriteFile(0600), que no Windows não restringe nada. Protege
			// o que já está no disco a cada partida (idempotente).
			for _, p := range []string{cfg.TokenPath, cfg.PrivateKeyPath} {
				if _, serr := os.Stat(p); serr == nil {
					if perr := segredo.Proteger(p); perr != nil {
						slog.Warn("não consegui restringir o acesso a um arquivo sensível", "path", p, "err", perr)
					}
				}
			}

			// Sob o SCM a saída de erro não vai a lugar nenhum: o log vai
			// para um arquivo ao lado da configuração, com rodízio.
			if service.EmServicoWindows() {
				caminho := service.ArquivoDeLog(cfg.ConfigPath)
				if w, lerr := logarquivo.Abrir(caminho, logarquivo.TamanhoPadrao); lerr == nil {
					defer w.Close()
					logPara(w)
				}
			}

			start := func(ctx context.Context) error {
				slog.Info("iniciando daemon",
					"agent_id", cfg.AgentID,
					"panel", cfg.PanelURL,
					"host_root", cfg.HostRoot)
				err := daemon.Run(ctx, cfg)
				if errors.Is(err, atualizacao.ErrReiniciar) {
					// O programa no disco mudou. Sai daqui mesmo, sem voltar
					// ao SCM do Windows: quem para sem avisar é religado pelas
					// ações de recuperação do serviço.
					slog.Info("saindo para o serviço reiniciar com o programa que está no lugar")
					sairParaReiniciar()
				}
				return err
			}

			// No Windows, quando o processo é iniciado pelo Service Control
			// Manager, o daemon precisa rodar sob o protocolo de serviço —
			// senão o SCM encerra tudo com o erro 1053. Fora desse caso (e em
			// qualquer outro sistema) seguimos o caminho normal de terminal.
			if handled, err := service.RunAsService(serviceName, start); handled {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return start(ctx)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultPath, "env-file com credenciais de storage")
	// Padrão vazio: com "/" aqui, a flag sempre vencia o HOST_ROOT do
	// ambiente/env-file (a precedência é CLI > env). O "/" vem do config.
	cmd.Flags().StringVar(&hostRoot, "host-root", "", "raiz do filesystem a proteger (em container Docker: /host). Padrão: HOST_ROOT, senão /")
	cmd.Flags().StringVar(&serviceName, "service-name", service.DefaultName, "nome do serviço (usado só quando iniciado pelo gerenciador de serviços do Windows)")

	return cmd
}

// sair é o os.Exit; os testes o trocam.
var sair = os.Exit

// registrarArranque é o atualizacao.RegistrarArranque; os testes o trocam.
var registrarArranque = atualizacao.RegistrarArranque

// recuperarSeSouOAnterior: quando quem está rodando é o `arkame-agent.anterior`
// e o caminho do programa está vazio, a troca parou entre os dois renames.
// Alguém executou o anterior à mão (ou por uma ação de recuperação do
// serviço): ele põe o programa de volta no lugar.
func recuperarSeSouOAnterior() {
	programa, ok := atualizacao.ProgramaDoAnterior(service.ProgramaAtual())
	if !ok {
		return
	}
	if feito := atualizacao.Recuperar(programa); feito != "" {
		slog.Warn("a troca do programa tinha parado no meio; pus o programa de volta no lugar",
			"programa", programa, "qual", feito)
	}
}

// sairParaReiniciar encerra o processo para o programa que está no disco
// subir. No Docker não há gerenciador de serviço: este processo é trocado
// pelo programa da imagem (o lançador), que decide de novo o que executar — o
// container não para, e nada depende da política de reinício do docker run.
// Fora dele, sai com o código que o gerenciador de serviço religa.
func sairParaReiniciar() {
	if atualizacao.EmDocker() {
		lancador := os.Getenv(atualizacao.EnvLancador)
		if lancador == "" {
			lancador, _ = os.Executable()
		}
		if lancador != "" {
			err := atualizacao.Executar(lancador, os.Args[1:], atualizacao.AmbienteSemLancador(os.Environ()))
			slog.Error("não consegui voltar ao programa da imagem; saindo para o container reiniciar", "err", err)
		}
	}
	sair(atualizacao.CodigoDeReinicio)
}

// lancarDoVolume é o lançador do Docker: se o volume da configuração tem um
// programa baixado pela atualização automática, válido e mais novo que o da
// imagem, este processo vira ele. Fora do Docker, com a atualização desligada
// ou já lançado, não faz nada.
func lancarDoVolume(configFile string) {
	if !atualizacao.EmDocker() || os.Getenv(atualizacao.EnvLancador) != "" {
		return
	}
	// ARKAME_AUTO_UPDATE vale no ambiente do container e no env-file.
	desligada := atualizacao.Desligada(os.Getenv(atualizacao.EnvAutoUpdate))
	if cfg, err := config.Load(configFile, config.Overrides{}); err == nil {
		desligada = desligada || atualizacao.Desligada(cfg.AutoUpdate)
	}
	if desligada {
		return
	}
	programa := atualizacao.ProgramaDoVolume(configFile)
	// O programa do volume vem aberto e conferido pelo descritor, e é pelo
	// descritor que é executado: entre conferir e executar não há caminho em
	// texto para alguém trocar.
	aberto, porque := atualizacao.AbrirDoVolume(programa, version.Version,
		atualizacao.ChavesConfiaveis(), atualizacao.NomeNoRelease())
	if aberto == nil {
		slog.Debug("lançador: sigo com o programa da imagem", "motivo", porque)
		return
	}
	defer aberto.Close()
	eu, err := os.Executable()
	if err != nil {
		slog.Warn("lançador: não sei o caminho do programa da imagem; sigo com ele", "err", err)
		return
	}
	slog.Info("lançador: executando o programa baixado para o volume", "programa", programa, "imagem", version.Version)
	err = atualizacao.ExecutarAberto(aberto, programa, os.Args[1:], atualizacao.AmbienteDoLancado(os.Environ(), eu, version.Version))
	// Só chega aqui se o programa do volume não executou (volume montado com
	// noexec, arquivo de outra arquitetura): fica abandonado, e vale o da imagem.
	slog.Error("lançador: o programa do volume não executou; sigo com o da imagem", "err", err)
	atualizacao.AbandonarDoVolume(programa, atualizacao.MotivoNaoExecutou)
}

var _ = context.Background
