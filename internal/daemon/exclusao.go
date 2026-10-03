package daemon

import (
	"context"
	"sync"

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
//   - A limpeza não espera (TryLock). Com backup em curso, ela nem pergunta ao
//     painel — perguntar é o que emite a rodada, e o plano tem de ser montado
//     com o bucket parado — e tenta de novo em reprovaDoExpurgo, não na hora
//     seguinte. Entre um plano e outro o planLoop solta a trava, então a
//     limpeza só fica sem vez se houver backup ocupando o bucket o tempo todo;
//     nesse caso ela é adiada, não perdida, e o log diz por quê.
//
// É uma trava só e nenhum dos lados pega outra enquanto a segura: não há
// deadlock possível.
type exclusaoDoBucket struct {
	mu sync.Mutex
}

// backup roda fn com o bucket só para ele, esperando a limpeza em curso.
func (e *exclusaoDoBucket) backup(fn func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fn()
}

// limpeza roda fn se não houver backup em curso. Devolve false, sem rodar fn,
// quando o bucket está ocupado.
func (e *exclusaoDoBucket) limpeza(fn func()) bool {
	if !e.mu.TryLock() {
		return false
	}
	defer e.mu.Unlock()
	fn()
	return true
}

// executarPlanoExclusivo é o executePlan com o bucket fechado para a limpeza
// de retenção do começo ao fim da sessão (do /start ao /complete).
func executarPlanoExclusivo(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, plan api.Plan, trava *exclusaoDoBucket) error {
	return trava.backup(func() error {
		return executePlan(ctx, c, s3c, cfg, plan)
	})
}
