package daemon

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// exclusaoDoBucket impede que backup e limpeza de retenção rodem juntos no
// bucket deste processo.
//
// A corrida que ela fecha: o backup reaproveita a versão atual da chave quando
// o sha256 bate (checkDedup, sem reenviar), e a limpeza apaga a versão atual
// de propósito em hard_delete/deleted_file, com um plano que o painel montou
// sobre a sessão completa anterior. Se os dois se cruzam, a sessão grava no
// catálogo uma versão que já saiu do bucket — o painel a mostra como
// restaurável e a restauração falha com NoSuchVersion.
//
// Uma trava só, por processo: cada processo do agente atende um bucket (os
// outros buckets do mesmo agente são de processos irmãos, SIBLING_BUCKETS), e
// o planLoop já roda os planos um de cada vez. Trava por storage seria a mesma
// trava com um mapa em volta.
//
// Quem espera por quem, sem deadlock nem fome:
//   - O backup espera (Lock). A limpeza em curso é curta e limitada — o painel
//     emite no máximo uma rodada por dia, com teto de versões — e o backup
//     agendado não pode ser pulado.
//   - O teste de restauração por amostra não espera (TryLock) e dá a vez ao
//     backup que chegar no meio dele (verificacao, backupEsperando).
//   - A limpeza não espera (TryLock). Com backup em curso, ela nem pergunta ao
//     painel — perguntar é o que emite a rodada, e o plano tem de ser montado
//     com o bucket parado — e tenta de novo em reprovaDoExpurgo, não na hora
//     seguinte. Entre um plano e outro o planLoop solta a trava, então a
//     limpeza só fica sem vez se houver backup ocupando o bucket o tempo todo;
//     nesse caso ela é adiada, não perdida, e o log diz por quê.
//
// É uma trava só e nenhum dos lados pega outra enquanto a segura: não há
// deadlock possível.
//
// A atualização do programa pega a mesma trava (ocupacao.reservar, sem
// esperar): só troca o programa com o bucket livre, e enquanto troca nem
// backup nem limpeza começam.
type exclusaoDoBucket struct {
	mu sync.Mutex
	// quem segura a trava agora (atividadeBackup ou atividadeExpurgo), para a
	// atualização adiada dizer ao painel por quê.
	quem atomic.Value
	// esperando conta os backups parados no Lock: o teste de restauração olha
	// entre um item e outro e dá a vez (backupEsperando).
	esperando atomic.Int32
	// pendente é o plano de teste de restauração que o painel já entregou e
	// ainda não pôde rodar porque o bucket ficou ocupado (planoPendente).
	pendente planoPendente
}

const (
	atividadeBackup  = "backup"
	atividadeExpurgo = "purge"
	// atividadeVerificacao: o teste de restauração por amostra.
	atividadeVerificacao = "verify"
)

// backup roda fn com o bucket só para ele, esperando a limpeza em curso.
func (e *exclusaoDoBucket) backup(fn func() error) error {
	e.esperando.Add(1)
	e.mu.Lock()
	e.esperando.Add(-1)
	defer e.mu.Unlock()
	e.quem.Store(atividadeBackup)
	return fn()
}

// limpeza roda fn se não houver backup em curso. Devolve false, sem rodar fn,
// quando o bucket está ocupado.
func (e *exclusaoDoBucket) limpeza(fn func()) bool {
	if !e.mu.TryLock() {
		return false
	}
	defer e.mu.Unlock()
	e.quem.Store(atividadeExpurgo)
	fn()
	return true
}

// verificacao roda fn (o teste de restauração por amostra) se o bucket estiver
// livre. Como a limpeza, não espera: devolve false, sem rodar fn, com backup
// ou limpeza em curso. O backup que chegar durante o teste espera — pouco: o
// teste vê que há backup esperando (backupEsperando) e encerra no item em que
// está.
func (e *exclusaoDoBucket) verificacao(fn func()) bool {
	if !e.mu.TryLock() {
		return false
	}
	defer e.mu.Unlock()
	e.quem.Store(atividadeVerificacao)
	fn()
	return true
}

// livre diz se o bucket está desocupado agora — sem segurar a trava.
func (e *exclusaoDoBucket) livre() bool {
	if !e.mu.TryLock() {
		return false
	}
	e.mu.Unlock()
	return true
}

// backupEsperando diz se há backup parado à espera do bucket.
func (e *exclusaoDoBucket) backupEsperando() bool { return e.esperando.Load() > 0 }

// executarPlanoExclusivo é o executePlan com o bucket fechado para a limpeza
// de retenção do começo ao fim da sessão (do /start ao /complete).
func executarPlanoExclusivo(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, plan api.Plan, trava *exclusaoDoBucket) error {
	return trava.backup(func() error {
		// A trava pode ter sido solta por uma troca de programa que acabou de
		// encerrar o daemon: a sessão não abre no programa que está de saída.
		if err := ctx.Err(); err != nil {
			return err
		}
		return executePlan(ctx, c, s3c, cfg, plan)
	})
}
