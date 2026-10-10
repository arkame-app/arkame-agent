package atualizacao

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrReiniciar: o programa no disco mudou (troca feita, ou revertida) e este
// processo tem de sair para o gerenciador de serviço subir o que está lá.
var ErrReiniciar = errors.New("o programa foi trocado: o serviço reinicia com o que está no lugar")

// CodigoDeReinicio é o código de saída do processo que sai para ser religado.
// Diferente de zero de propósito: o SCM do Windows só aplica as ações de
// recuperação a quem sai com erro, e uma unit feita à mão com
// Restart=on-failure também só religa assim. 75 é o EX_TEMPFAIL do sysexits.
const CodigoDeReinicio = 75

// PrazoDaProva é quanto o programa novo tem, depois de subir, para um
// heartbeat aceito pelo painel. MaximoDeArranques é em qual partida seguida
// sem sucesso o anterior volta sem esperar o prazo: um programa que cai antes
// de completar cinco minutos nunca chegaria ao fim dele.
var PrazoDaProva = 5 * time.Minute

const MaximoDeArranques = 3

// TetoDaProva é até quando a prova pode ser estendida, contado da troca: com
// o painel fora do ar (erro de rede, 5xx) o programa novo não tem como provar
// nada, e voltar atrás puniria a versão por uma falha que não é dela. Passado
// o teto, volta mesmo assim — um programa novo que não fala com o painel por
// meia hora não fica. PassoDaExtensao é de quanto em quanto a prova estendida
// é reavaliada.
var (
	TetoDaProva     = 30 * time.Minute
	PassoDaExtensao = 1 * time.Minute
)

// RespostaDoPainel é o que o último heartbeat deste processo ouviu.
type RespostaDoPainel int

const (
	// PainelSemTentativa: este processo ainda não mandou heartbeat.
	PainelSemTentativa RespostaDoPainel = iota
	// PainelIndisponivel: erro de rede, 5xx, 408 ou 429 — o painel (ou o
	// caminho até ele) não está atendendo.
	PainelIndisponivel
	// PainelRecusou: 401, 403 e os demais 4xx — o painel atendeu e disse não.
	PainelRecusou
)

// Motivos de reversão, como vão ao painel.
const (
	MotivoSemHeartbeat   = "no_heartbeat"
	MotivoArranques      = "start_loop"
	MotivoTrocaIncomplet = "swap_incomplete"
	MotivoInvalido       = "invalid_binary"
	MotivoNaoExecutou    = "exec_failed"
	// MotivoSemVolta: a prova venceu e não havia anterior utilizável; o
	// programa novo seguiu no lugar.
	MotivoSemVolta = "rollback_unavailable"
)

// RegistrarArranque é chamado pelo `run` depois de a configuração carregar e
// de o agente ter identidade e token. Com uma troca em prova, conta esta
// partida; na terceira sem sucesso, devolve o programa anterior ao lugar e
// avisa (reverteu=true) — o chamador sai, e o gerenciador de serviço sobe o
// anterior.
//
// Só conta a partida que passou da configuração: contada antes, um `run` com
// o arquivo errado, ou o agente vizinho que já não subia por um problema dele
// (sem token, sem identidade), gastava as três partidas e desfazia a troca de
// quem estava bem. A configuração que o programa novo não consegue ler é
// barrada antes da troca, pelo auto-teste, que a carrega.
//
// chave identifica o agente (o arquivo de configuração): dois agentes da
// máquina dividem o programa e o registro, e a partida de um não conta para o
// outro.
func RegistrarArranque(programa, versaoAtual, chave string) (reverteu bool) {
	l := LugarDe(programa)
	e, ok := l.lerEstado()
	if !ok || e.Situacao != SituacaoEmProva || e.PeloLancador {
		return false
	}
	if !mesmaVersao(e.Versao, versaoAtual) {
		// Quem está subindo não é o programa em prova: a troca não chegou ao
		// fim (o processo morreu entre o registro e o rename) ou alguém pôs
		// outro programa por cima. A prova não tem mais o que provar.
		e.Situacao = SituacaoRevertida
		e.Motivo = MotivoTrocaIncomplet
		if err := l.gravarEstado(e); err != nil {
			slog.Warn("não consegui atualizar o registro da troca", "path", l.Estado, "err", err)
		}
		return false
	}
	if e.Arranques == nil {
		e.Arranques = map[string]int{}
	}
	e.Arranques[chave]++
	if e.Arranques[chave] < MaximoDeArranques {
		if err := l.gravarEstado(e); err != nil {
			slog.Warn("não consegui contar esta partida no registro da troca", "path", l.Estado, "err", err)
		}
		return false
	}
	slog.Error("o programa novo subiu três vezes sem um heartbeat aceito; voltando ao anterior",
		"versao", e.Versao, "anterior", e.VersaoAnterior)
	return l.voltar(e, MotivoArranques)
}

// voltar devolve o anterior ao lugar e registra. false quando não deu: o
// programa novo segue, que é melhor que programa nenhum.
//
// Sem anterior utilizável (ErrSemAnterior) a prova não fica aberta para
// sempre nem fecha como "revertida", que seria mentira: o registro passa a
// "sem_volta", o painel recebe rollback_unavailable, e o primeiro heartbeat
// aceito ainda confirma a troca. Outro erro (o rename falhou) deixa o registro
// como está, e a volta é tentada de novo na próxima partida.
func (l Lugar) voltar(e Estado, motivo string) bool {
	err := l.reverter(e.Sha256Anterior)
	if errors.Is(err, ErrSemAnterior) {
		slog.Error("não há programa anterior utilizável para voltar; sigo com o novo", "err", err, "motivo", motivo)
		e.Situacao = SituacaoSemVolta
		e.Motivo = MotivoSemVolta
		e.Arranques = nil
		if gerr := l.gravarEstado(e); gerr != nil {
			slog.Warn("não consegui registrar que a troca ficou sem volta", "path", l.Estado, "err", gerr)
		}
		return false
	}
	if err != nil {
		slog.Error("não consegui voltar ao programa anterior; sigo com o novo", "err", err)
		return false
	}
	e.Situacao = SituacaoRevertida
	e.Motivo = motivo
	if err := l.gravarEstado(e); err != nil {
		slog.Warn("programa anterior de volta, mas não consegui registrar", "path", l.Estado, "err", err)
	}
	return true
}

// Prova acompanha, no daemon, a troca em prova deste programa.
type Prova struct {
	l          Lugar
	aplicadaEm time.Time
	mu         sync.Mutex
	aberta     bool
}

// AbrirProva devolve a prova em curso deste programa, ou nil quando não há:
// sem registro, registro de outra versão, ou troca já confirmada. A troca que
// ficou sem volta (SituacaoSemVolta) também abre prova: não há mais o que
// reverter, mas o heartbeat aceito ainda a confirma.
func AbrirProva(programa, versaoAtual string) *Prova {
	l := LugarDe(programa)
	e, ok := l.lerEstado()
	if !ok || !emProva(e.Situacao) || !mesmaVersao(e.Versao, versaoAtual) {
		return nil
	}
	return &Prova{l: l, aplicadaEm: e.AplicadaEm, aberta: true}
}

func emProva(situacao string) bool {
	return situacao == SituacaoEmProva || situacao == SituacaoSemVolta
}

// Estende diz se a prova que venceu ganha mais um passo em vez de voltar
// atrás: só quando o último heartbeat não foi atendido (rede, 5xx) e a troca
// ainda está dentro do teto — contado da troca e, por garantia contra relógio
// acertado no meio, também do arranque deste processo.
//
// Não estende: quem nunca chegou a tentar um heartbeat (o processo está preso
// antes disso), nem quem ouviu um "não" do painel (401/403 que duram o prazo
// inteiro da prova são do programa novo ou do token que ele gravou).
func (p *Prova) Estende(ultima RespostaDoPainel, desdeOArranque time.Duration) bool {
	if p == nil || ultima != PainelIndisponivel {
		return false
	}
	return desdeOArranque < TetoDaProva && agora().Sub(p.aplicadaEm) < TetoDaProva
}

// Confirmar registra que o painel aceitou um heartbeat deste programa: a
// troca fica. Chamar de novo não faz nada.
func (p *Prova) Confirmar() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aberta {
		return
	}
	p.aberta = false
	e, ok := p.l.lerEstado()
	if !ok || !emProva(e.Situacao) {
		return
	}
	e.Situacao = SituacaoConfirmada
	e.Motivo = ""
	e.Arranques = nil
	if err := p.l.gravarEstado(e); err != nil {
		slog.Warn("atualização confirmada, mas não consegui registrar", "path", p.l.Estado, "err", err)
		return
	}
	if v, err := LerVersao(e.Versao); err == nil && e.PeloLancador {
		p.l.limparOutrasVersoes(v)
	}
	slog.Info("atualização confirmada: o painel aceitou o heartbeat da versão nova", "versao", e.Versao)
}

// Vencer é o fim do prazo sem heartbeat aceito. Devolve true quando o
// programa no lugar mudou (o anterior voltou; no Docker, o do volume foi
// abandonado) e o processo tem de sair.
//
// Sem anterior utilizável o programa novo segue (false) e a prova continua
// aberta para o Confirmar: o registro vira "sem_volta" (ver voltar).
func (p *Prova) Vencer() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aberta {
		return false
	}
	e, ok := p.l.lerEstado()
	if !ok || e.Situacao != SituacaoEmProva {
		// Sem volta (já vencida antes), ou o registro mudou por fora: nada a
		// reverter. Só a troca sem volta segue esperando o heartbeat.
		p.aberta = ok && e.Situacao == SituacaoSemVolta
		return false
	}
	slog.Error("o programa novo não teve um heartbeat aceito no prazo da prova; voltando atrás",
		"versao", e.Versao, "prazo", PrazoDaProva)
	if e.PeloLancador {
		p.aberta = false
		e.Situacao = SituacaoAbandonada
		e.Motivo = MotivoSemHeartbeat
		if err := p.l.gravarEstado(e); err != nil {
			slog.Error("não consegui registrar o abandono do programa do volume", "err", err)
			return false
		}
		return true
	}
	if p.l.voltar(e, MotivoSemHeartbeat) {
		p.aberta = false
		return true
	}
	// Não voltou. Sem volta possível, a prova segue aberta para o heartbeat
	// que ainda chegar; se foi o rename que falhou, fecha (a próxima partida
	// tenta de novo, pela contagem).
	depois, ok := p.l.lerEstado()
	p.aberta = ok && depois.Situacao == SituacaoSemVolta
	return false
}

// Aberta diz se a prova ainda espera o heartbeat.
func (p *Prova) Aberta() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aberta
}

// Relato é o que o heartbeat conta ao painel sobre a última troca desta
// máquina, lido do registro: estado ("proving", "rolled_back" ou vazio), a
// versão a que ele se refere e o motivo.
func Relato(programa, versaoAtual string) (estado, versao, motivo string) {
	estado, versao, motivo, _ = RelatoComInstante(programa, versaoAtual)
	return estado, versao, motivo
}

// RelatoComInstante é o Relato com o instante de quando é o estado relatado.
// O registro fica no disco, e o agente segue relatando a reversão de uma
// tentativa até a próxima: é pelo instante que o painel sabe que o relato é
// anterior à oferta nova.
func RelatoComInstante(programa, versaoAtual string) (estado, versao, motivo string, em time.Time) {
	e, ok := LugarDe(programa).lerEstado()
	if !ok {
		return "", "", "", time.Time{}
	}
	em = e.MudouEm
	if em.IsZero() {
		em = e.AplicadaEm
	}
	switch e.Situacao {
	case SituacaoEmProva:
		if mesmaVersao(e.Versao, versaoAtual) {
			return "proving", e.Versao, "", em
		}
	case SituacaoSemVolta:
		// O programa novo segue no lugar sem ter provado nada e sem volta
		// possível: continua "em prova" para o painel, com o porquê.
		if mesmaVersao(e.Versao, versaoAtual) {
			return "proving", e.Versao, MotivoSemVolta, em
		}
	case SituacaoRevertida, SituacaoAbandonada:
		return "rolled_back", e.Versao, e.Motivo, em
	}
	return "", "", "", time.Time{}
}
