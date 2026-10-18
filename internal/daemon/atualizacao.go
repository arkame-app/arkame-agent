package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/instantaneo"
	"github.com/arkame-app/agent/internal/localstore"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/internal/zimbra"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ocupacao diz à atualização do programa se o daemon está no meio de algo que
// ela não pode cortar, e segura esse algo enquanto a troca acontece.
//
// A troca termina com o processo saindo. Com backup em curso, a sessão
// fecharia como interrompida; com expurgo, a rodada ficaria pela metade; com
// restauração, o item voltaria a "running" para o próximo arranque. Nada disso
// perde dado, mas a atualização pode esperar e o backup da noite, não: ela só
// acontece com o bucket livre e sem restauração em curso — e, enquanto
// acontece, nenhum dos três começa.
type ocupacao struct {
	// trava é a mesma do backup e do expurgo (exclusaoDoBucket).
	trava *exclusaoDoBucket

	mu          sync.Mutex
	restaurando int
	atualizando bool
}

// comecarRestauracao marca uma rodada de restauração com itens a executar.
// false: há uma troca de programa em curso, e a rodada fica para o programa
// que subir.
func (o *ocupacao) comecarRestauracao() bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.atualizando {
		return false
	}
	o.restaurando++
	return true
}

func (o *ocupacao) terminarRestauracao() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.restaurando--
	o.mu.Unlock()
}

// reservar fecha o daemon para a troca do programa: pega a trava do bucket
// (sem esperar) e barra restauração nova. Com algo em curso, devolve o motivo
// e não reserva nada.
func (o *ocupacao) reservar() (liberar func(), motivo string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.restaurando > 0 {
		return nil, atualizacao.AdiadaRestauracao
	}
	if !o.trava.mu.TryLock() {
		if q, _ := o.trava.quem.Load().(string); q == atividadeExpurgo {
			return nil, atualizacao.AdiadaExpurgo
		}
		return nil, atualizacao.AdiadaBackup
	}
	o.atualizando = true
	return func() {
		o.mu.Lock()
		o.atualizando = false
		o.mu.Unlock()
		o.trava.mu.Unlock()
	}, ""
}

// esperaDepoisDeFalha é por quanto tempo o agente não tenta de novo a versão
// cuja troca falhou (download, conferência, auto-teste), salvo `force`. O
// painel também para de oferecer a versão a quem relatou a falha; isto cobre o
// intervalo até o relato chegar e o painel antigo.
const esperaDepoisDeFalha = 6 * time.Hour

// atualizador recebe do heartbeat o pedido de atualização do painel e faz a
// troca num laço próprio — o download não pode segurar o heartbeat.
type atualizador struct {
	cfg *config.Config
	oc  *ocupacao
	// reiniciar encerra o daemon para o gerenciador de serviço subir o
	// programa que está no lugar.
	reiniciar func()
	// programa é o arquivo que a troca substitui; impedimento, por que este
	// processo não se atualiza sozinho ("" quando pode).
	programa    string
	impedimento string
	svc         service.Detected
	s3c         *s3.Client
	prova       *atualizacao.Prova

	pedidos chan api.UpdateOffer

	mu sync.Mutex
	// relato é o estado da tentativa em curso ou da última, na memória.
	relato api.UpdateReport
	// naoAntesDe: versão cuja troca falhou → quando tentar de novo.
	naoAntesDe map[string]time.Time
	// saindo: a troca foi feita e o processo está de saída.
	saindo atomic.Bool
	// painel é o que o último heartbeat ouviu (atualizacao.RespostaDoPainel):
	// decide se a prova que vence é estendida (painel fora do ar) ou não.
	painel atomic.Int32

	// aplicar é o atualizacao.Aplicar; os testes o trocam.
	aplicar func(context.Context, atualizacao.Pedido, atualizacao.Opcoes) (*atualizacao.Resultado, error)
}

// programaDaTroca é o arquivo que a atualização substitui: o que o serviço
// executa; no Docker, o do volume da configuração (o da imagem é só leitura).
func programaDaTroca(cfg *config.Config) string {
	if atualizacao.EmDocker() {
		return atualizacao.ProgramaDoVolume(cfg.ConfigPath)
	}
	return service.ProgramaAtual()
}

// arquivoNaPastaDoEstado é um arquivo qualquer da pasta do estado local, para
// ela entrar nas pastasDoAgente ("" sem estado).
func arquivoNaPastaDoEstado(cfg *config.Config) string {
	if cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(cfg.StateDir, "estado.db")
}

// pastasDoAgente são as pastas onde a restauração nunca grava
// (restore.ErrDestinoProtegido): a do programa que este processo executa, a do
// programa que a atualização troca (no Docker, bin/ no volume) e as da
// configuração, do token, da chave e do id do agente (/etc/arkame).
func pastasDoAgente(cfg *config.Config) []string {
	var pastas []string
	visto := map[string]bool{}
	for _, arquivo := range []string{
		service.ProgramaAtual(), programaDaTroca(cfg),
		cfg.ConfigPath, cfg.TokenPath, cfg.PrivateKeyPath, cfg.AgentIDPath,
		arquivoNaPastaDoEstado(cfg),
	} {
		if arquivo == "" || !filepath.IsAbs(arquivo) {
			continue
		}
		d := filepath.Dir(arquivo)
		// A raiz como "pasta do agente" recusaria toda restauração.
		if d == filepath.VolumeName(d)+string(filepath.Separator) || visto[d] {
			continue
		}
		visto[d] = true
		pastas = append(pastas, d)
	}
	return pastas
}

func novoAtualizador(cfg *config.Config, svc service.Detected, s3c *s3.Client, oc *ocupacao, reiniciar func()) *atualizador {
	programa := programaDaTroca(cfg)
	inst := atualizacao.Instalacao{
		Programa:    programa,
		VersaoAtual: version.Version,
		Desligada:   atualizacao.Desligada(cfg.AutoUpdate),
		SobServico:  svc.Name != "" || atualizacao.EmDocker(),
		TemChaves:   len(atualizacao.ChavesConfiaveis()) > 0,
	}
	a := &atualizador{
		cfg:         cfg,
		oc:          oc,
		reiniciar:   reiniciar,
		programa:    programa,
		impedimento: inst.Impedimento(),
		svc:         svc,
		s3c:         s3c,
		prova:       provaDoProcesso(cfg, programa),
		pedidos:     make(chan api.UpdateOffer, 1),
		naoAntesDe:  map[string]time.Time{},
		aplicar:     atualizacao.Aplicar,
	}
	if a.impedimento == "" {
		slog.Info("atualização automática do programa ligada", "programa", programa)
	} else {
		slog.Info("este agente não se atualiza sozinho", "motivo", a.impedimento, "programa", programa)
	}
	return a
}

// capacidades é o que o agente declara ao painel. Sempre lista, nunca nil:
// no JSON, ausente é agente antigo.
func (a *atualizador) capacidades() []string {
	caps := capacidadesDoIndice()
	if a == nil {
		return caps
	}
	// O teste de restauração por amostra: só com armazenamento configurado e
	// pasta de estado onde restaurar (podeVerificar).
	if podeVerificar(a.cfg) {
		caps = append(caps, api.CapacidadeVerificarRestauracao)
	}
	// O inventário do bucket: basta o armazenamento configurado (nada é gravado).
	if podeInventariar(a.cfg) {
		caps = append(caps, api.CapacidadeInventariarBucket)
	}
	if instantaneo.Suportado {
		caps = append(caps, api.CapacidadeInstantaneoDoVolume)
	}
	if a.impedimento != "" {
		return caps
	}
	return append(caps, atualizacao.Capacidade)
}

// capacidadesDoIndice são as que todo agente desta versão declara: o índice
// em lotes e em delta, e o `removed` com prova positiva em sessão parcial.
// Não dependem da instalação.
func capacidadesDoIndice() []string {
	caps := []string{api.CapacidadeIndiceEmLotes, api.CapacidadeIndiceDelta, api.CapacidadeRemovidosEmParcial,
		// prepare-dump-folder e ARKAME_AGENT nos comandos: também não
		// dependem da instalação.
		api.CapacidadePastaDeDump}
	// A cópia local: só onde há RustFS para a plataforma. No contêiner (0.5.3)
	// também: o `local-storage install` roda de dentro dele e a cópia local é
	// um segundo contêiner. Junto, o share e o join (receber backups de outros
	// servidores da rede, e gravar na cópia local de outro).
	if _, err := localstore.ArtefatoDaPlataforma(); err == nil {
		caps = append(caps, api.CapacidadeCopiaLocal, api.CapacidadeCopiaLocalNaRede)
	}
	// O Zimbra: só no servidor Linux que o tem (no Docker, /opt/zimbra seria
	// a do contêiner do agente, e a porta de administração não é a dele).
	if runtime.GOOS == "linux" && temZimbra() {
		caps = append(caps, api.CapacidadeZimbra, api.CapacidadeZimbraPastaDaRestauracao, api.CapacidadeZimbraLimpeza)
	}
	return caps
}

// emConteiner: o agente roda com HOST_ROOT (o Docker), o mesmo critério com
// que o `local-storage install` recusa. Não olha /.dockerenv nem a variável
// `container`: o agente nativo num LXC (container=lxc) cria a cópia local, e o
// teste num toolbox não muda de resultado. O Run grava; variável nos testes.
var emConteiner = func() bool { return comHostRoot.Load() }

var comHostRoot atomic.Bool

// temZimbra: o Zimbra no lugar padrão. Variável para os testes.
var temZimbra = func() bool { return zimbra.Instalado("") == nil }

// relatoDoHeartbeat monta o `update` do heartbeat: a tentativa em curso (ou a
// última, se falhou), senão o que o registro da troca diz (em prova,
// revertida), mais o impedimento. nil quando não há nada a dizer.
func (a *atualizador) relatoDoHeartbeat() *api.UpdateReport {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	r := a.relato
	a.mu.Unlock()
	if r.State == "" {
		var em time.Time
		r.State, r.Version, r.Error, em = atualizacao.RelatoComInstante(a.programa, version.Version)
		if r.State != "" && !em.IsZero() {
			r.At = &em
		}
	}
	if r.At != nil {
		idade := int64(time.Since(*r.At) / time.Second)
		if idade < 0 {
			idade = 0 // o relógio andou para trás desde o estado
		}
		r.AgeSeconds = &idade
	}
	r.Blocked = a.impedimento
	if r == (api.UpdateReport{}) {
		return nil
	}
	return &r
}

func (a *atualizador) relatar(estado, versao, erro string) {
	em := time.Now().UTC()
	a.mu.Lock()
	a.relato = api.UpdateReport{State: estado, Version: versao, Error: erro, At: &em}
	a.mu.Unlock()
}

// ouviuDoPainel anota o resultado de um heartbeat: aceito (nil), não atendido
// (rede, 5xx, 408, 429) ou recusado (os demais 4xx).
func (a *atualizador) ouviuDoPainel(err error) {
	if a == nil || err == nil {
		return
	}
	resposta := atualizacao.PainelIndisponivel
	var he *api.HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status < 500 &&
		he.Status != http.StatusRequestTimeout && he.Status != http.StatusTooManyRequests {
		resposta = atualizacao.PainelRecusou
	}
	a.painel.Store(int32(resposta))
}

// receber é chamado com o `update` da resposta do heartbeat. Não bloqueia:
// com uma troca em curso, o pedido repetido é descartado (o painel repete a
// cada heartbeat enquanto o agente dever atualizar).
func (a *atualizador) receber(p *api.UpdateOffer) {
	if a == nil || p == nil {
		return
	}
	if a.impedimento != "" {
		slog.Warn("o painel mandou atualizar, mas este agente não se atualiza sozinho",
			"versao", p.Version, "motivo", a.impedimento)
		return
	}
	select {
	case a.pedidos <- *p:
	default:
	}
}

// laco aplica os pedidos, um de cada vez.
func (a *atualizador) laco(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-a.pedidos:
			a.tentar(ctx, p)
		}
	}
}

// tentar faz uma tentativa de troca. Com o daemon ocupado, adia (o painel
// manda de novo no próximo heartbeat); com falha, relata e não insiste na
// mesma versão por um tempo; com a troca feita, encerra o daemon.
func (a *atualizador) tentar(ctx context.Context, p api.UpdateOffer) {
	if a.saindo.Load() || ctx.Err() != nil {
		return
	}
	if a.prova.Aberta() {
		// Este programa ainda está provando que funciona: não empilha outra
		// troca por cima de uma que pode ser desfeita.
		return
	}
	a.mu.Lock()
	ate, falhou := a.naoAntesDe[p.Version]
	a.mu.Unlock()
	if falhou && !p.Force && time.Now().Before(ate) {
		return
	}

	liberar, motivo := a.oc.reservar()
	if motivo != "" {
		slog.Info("atualização do programa adiada: o agente está ocupado", "versao", p.Version, "motivo", motivo)
		a.relatar("deferred", p.Version, motivo)
		return
	}
	a.relatar("downloading", p.Version, "")
	slog.Info("atualizando o programa do agente", "de", version.Version, "para", p.Version)

	tctx, cancel := context.WithTimeout(ctx, atualizacao.PrazoDaTroca)
	res, err := a.aplicar(tctx, atualizacao.Pedido{
		Versao:  p.Version,
		Sha256:  p.SHA256,
		BaseURL: p.BaseURL,
		Arquivo: p.File,
		Forcar:  p.Force,
	}, a.opcoes(tctx))
	cancel()

	var adiada *atualizacao.Adiada
	switch {
	case errors.As(err, &adiada):
		liberar()
		slog.Info("atualização do programa adiada", "versao", p.Version, "motivo", adiada.Motivo)
		a.relatar("deferred", p.Version, adiada.Motivo)
	case err != nil && ctx.Err() != nil:
		// O serviço parou no meio: não é falha da versão.
		liberar()
		a.relatar("deferred", p.Version, atualizacao.AdiadaServicoSaindo)
	case err != nil:
		liberar()
		slog.Error("a atualização do programa falhou; o programa atual continua", "versao", p.Version, "err", err)
		a.mu.Lock()
		a.naoAntesDe[p.Version] = time.Now().Add(esperaDepoisDeFalha)
		a.mu.Unlock()
		a.relatar("failed", p.Version, atualizacao.CodigoDaFalha(err))
	default:
		if res.JaNoLugar {
			slog.Info("o programa novo já está no lugar (outro agente desta máquina fez a troca); reiniciando para usá-lo", "versao", res.Versao)
		} else {
			slog.Info("programa novo no lugar; o serviço reinicia para a prova", "versao", res.Versao)
		}
		// O SCM do Windows só religa quem sai com erro se estiver avisado
		// (serviço instalado antes da atualização automática).
		if err := service.GarantirReligamento(a.svc.Name); err != nil {
			slog.Warn("não consegui confirmar o religamento automático do serviço", "err", err)
		}
		a.saindo.Store(true)
		// Encerra antes de soltar a trava: um backup que esperava por ela não
		// começa no programa que está de saída.
		a.reiniciar()
		liberar()
	}
}

// opcoes monta o que a troca precisa desta instalação.
func (a *atualizador) opcoes(ctx context.Context) atualizacao.Opcoes {
	return atualizacao.Opcoes{
		Programa:        a.programa,
		VersaoAtual:     version.Version,
		Chaves:          atualizacao.ChavesConfiaveis(),
		Reserva:         atualizacao.ReleasesNoGitHub,
		ArgsDoAutoTeste: ArgsDoAutoTeste(ctx, a.cfg, a.s3c),
		PrepararNovo:    service.PrepararProgramaNovo,
		PeloLancador:    atualizacao.EmDocker(),
	}
}

// ArgsDoAutoTeste são os argumentos do `self-test` do programa novo: a
// configuração deste agente e se o bucket tem de responder.
//
// O bucket só é exigido do programa novo quando responde ao atual: com a
// chave do bucket errada (ou o provedor fora do ar) agora, o auto-teste
// falharia por um motivo que não é do programa novo, e o agente nunca mais se
// atualizaria.
func ArgsDoAutoTeste(ctx context.Context, cfg *config.Config, s3c *s3.Client) []string {
	args := []string{"self-test", "--config", cfg.ConfigPath}
	if s3c != nil && cfg.StorageBucket != "" {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := storage.VersioningStatus(cctx, s3c, cfg.StorageBucket); err == nil {
			args = append(args, "--require-bucket")
		}
	}
	return args
}

// vigiarProva dá ao programa novo o prazo da prova. Sem heartbeat aceito até
// lá, o anterior volta ao lugar e o daemon encerra para o gerenciador subi-lo.
//
// Com o painel fora do ar — o último heartbeat deu erro de rede ou 5xx — a
// prova é estendida, de passo em passo, até o teto (atualizacao.TetoDaProva):
// um programa novo não pode ser desfeito porque o painel caiu logo depois da
// troca. Voltam atrás no prazo normal: o processo que nem chegou a tentar um
// heartbeat, e o que ouviu 401/403 até o fim do prazo. O processo que cai é
// com a contagem de partidas (RegistrarArranque).
func (a *atualizador) vigiarProva(ctx context.Context) {
	if !a.prova.Aberta() {
		return
	}
	slog.Info("programa novo em prova: precisa de um heartbeat aceito pelo painel",
		"versao", version.Version, "prazo", atualizacao.PrazoDaProva)
	inicio := time.Now()
	timer := time.NewTimer(atualizacao.PrazoDaProva)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !a.prova.Aberta() {
			return
		}
		if a.prova.Estende(atualizacao.RespostaDoPainel(a.painel.Load()), time.Since(inicio)) {
			slog.Warn("o prazo da prova venceu com o painel fora do ar; a prova é estendida",
				"versao", version.Version, "teto", atualizacao.TetoDaProva)
			timer.Reset(atualizacao.PassoDaExtensao)
			continue
		}
		if a.prova.Vencer() {
			a.saindo.Store(true)
			a.reiniciar()
		}
		return
	}
}

// versaoDoLancador é a versão do programa da imagem quando este processo é o
// programa do volume (Docker). Vazio fora disso.
func versaoDoLancador() string {
	if os.Getenv(atualizacao.EnvLancador) == "" {
		return ""
	}
	return os.Getenv(atualizacao.EnvVersaoDoLancador)
}

// ehIrmao: a cópia local ou a cópia da rede, que seguem o programa do
// principal em vez de trocá-lo.
func ehIrmao(cfg *config.Config) bool {
	return cfg.LocalStoreProgram != "" || cfg.Role == api.PapelCopiaDaRede || atualizacao.Segue(cfg.AutoUpdate)
}

// provaDoProcesso: só o principal abre (e confirma) a prova do programa novo.
// O irmão confirmava pelo principal, e um programa que só derrubava o
// principal não voltava atrás (revisão, 18/10).
func provaDoProcesso(cfg *config.Config, programa string) *atualizacao.Prova {
	if ehIrmao(cfg) {
		return nil
	}
	return atualizacao.AbrirProva(programa, version.Version)
}
