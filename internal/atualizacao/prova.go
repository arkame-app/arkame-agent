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

// Motivos de reversão, como vão ao painel.
const (
	MotivoSemHeartbeat   = "no_heartbeat"
	MotivoArranques      = "start_loop"
	MotivoTrocaIncomplet = "swap_incomplete"
	MotivoInvalido       = "invalid_binary"
	MotivoNaoExecutou    = "exec_failed"
)

// RegistrarArranque é a primeira coisa que o `run` faz. Com uma troca em
// prova, conta esta partida; na terceira sem sucesso, devolve o programa
// anterior ao lugar e avisa (reverteu=true) — o chamador sai, e o gerenciador
// de serviço sobe o anterior.
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

// voltar devolve o anterior ao lugar e registra. false quando não deu (sem
// anterior ao lado): o programa novo segue, que é melhor que programa nenhum.
func (l Lugar) voltar(e Estado, motivo string) bool {
	if err := l.reverter(); err != nil {
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
	l      Lugar
	mu     sync.Mutex
	aberta bool
}

// AbrirProva devolve a prova em curso deste programa, ou nil quando não há:
// sem registro, registro de outra versão, ou troca já confirmada.
func AbrirProva(programa, versaoAtual string) *Prova {
	l := LugarDe(programa)
	e, ok := l.lerEstado()
	if !ok || e.Situacao != SituacaoEmProva || !mesmaVersao(e.Versao, versaoAtual) {
		return nil
	}
	return &Prova{l: l, aberta: true}
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
	if !ok || e.Situacao != SituacaoEmProva {
		return
	}
	e.Situacao = SituacaoConfirmada
	e.Arranques = nil
	if err := p.l.gravarEstado(e); err != nil {
		slog.Warn("atualização confirmada, mas não consegui registrar", "path", p.l.Estado, "err", err)
		return
	}
	slog.Info("atualização confirmada: o painel aceitou o heartbeat da versão nova", "versao", e.Versao)
}

// Vencer é o fim do prazo sem heartbeat aceito. Devolve true quando o
// programa no lugar mudou (o anterior voltou; no Docker, o do volume foi
// abandonado) e o processo tem de sair.
func (p *Prova) Vencer() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aberta {
		return false
	}
	p.aberta = false
	e, ok := p.l.lerEstado()
	if !ok || e.Situacao != SituacaoEmProva {
		return false
	}
	slog.Error("o programa novo não teve um heartbeat aceito no prazo da prova; voltando atrás",
		"versao", e.Versao, "prazo", PrazoDaProva)
	if e.PeloLancador {
		e.Situacao = SituacaoAbandonada
		e.Motivo = MotivoSemHeartbeat
		if err := p.l.gravarEstado(e); err != nil {
			slog.Error("não consegui registrar o abandono do programa do volume", "err", err)
			return false
		}
		return true
	}
	return p.l.voltar(e, MotivoSemHeartbeat)
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
	e, ok := LugarDe(programa).lerEstado()
	if !ok {
		return "", "", ""
	}
	switch e.Situacao {
	case SituacaoEmProva:
		if mesmaVersao(e.Versao, versaoAtual) {
			return "proving", e.Versao, ""
		}
	case SituacaoRevertida, SituacaoAbandonada:
		return "rolled_back", e.Versao, e.Motivo
	}
	return "", "", ""
}
