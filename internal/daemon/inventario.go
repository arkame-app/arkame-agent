package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	gosync "sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/arkame-app/agent/internal/inventario"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// O inventário do bucket (contrato, itens 48 a 52): o painel entrega, em
// páginas, as versões que o catálogo deste servidor diz existir; o agente as
// confere contra a listagem do prefixo dele no bucket e relata o que falta.
//
// Não baixa arquivo nenhum: lista (uma requisição a cada mil versões) e faz
// até 200 HeadObject de amostra, mais um por versão que a listagem não trouxe.
//
// A trava do bucket é a mesma de backup, limpeza e teste de restauração, e o
// inventário é o mais educado dos quatro: pega-a por janela de poucas
// requisições, sem esperar na fila (TryLock), solta entre uma janela e outra,
// dá a vez ao backup que estiver esperando — inclusive no meio da janela,
// entre uma requisição e outra — e nunca a segura enquanto fala com o painel.
// Cada requisição ao bucket tem prazo de 60 s (inventario.PrazoDaRequisicao):
// um provedor que não responde não segura a trava até o prazo da rodada. Uma limpeza que rode entre duas janelas pode apagar versão que o
// plano ainda citava: o painel sabe o que mandou apagar e não a conta como
// faltante.

// Os tempos do laço; variáveis para os testes.
var (
	// intervaloDoInventario é o intervalo entre perguntas ao painel, que quase
	// sempre responde "nada": ele emite uma rodada por semana.
	intervaloDoInventario = 1 * time.Hour
	// primeiroInventario: depois da primeira pergunta do teste de restauração,
	// para os dois não disputarem o bucket no arranque.
	primeiroInventario = 12 * time.Minute
	// esperaSemRotaDeInventario: o painel não tem a rota (404).
	esperaSemRotaDeInventario = 24 * time.Hour
	// prazoDaPaginaDoInventario: cada página do plano é uma consulta ao
	// catálogo com prazo de 20 s no painel.
	prazoDaPaginaDoInventario = 2 * time.Minute
	// esperaPelaVezDoInventario: com o bucket ocupado, de quanto em quanto
	// tempo a janela tenta de novo.
	esperaPelaVezDoInventario = 2 * time.Second
)

// atividadeInventario: o inventário do bucket (na trava do bucket).
const atividadeInventario = "inventory"

// podeInventariar diz se este processo tem como fazer o inventário: um
// armazenamento configurado. Não precisa de pasta de estado — nada é gravado.
func podeInventariar(cfg *config.Config) bool {
	return cfg != nil && cfg.AgentID != "" && cfg.StorageID != "" && cfg.StorageBucket != ""
}

// inventario é a janela do inventário do bucket: roda fn com a trava, se
// conseguir a vez. Não entra na fila (TryLock) e cede ao backup que estiver
// esperando; tenta de novo a cada esperaPelaVezDoInventario, até o contexto
// vencer — aí devolve false, sem rodar fn.
func (e *exclusaoDoBucket) inventario(ctx context.Context, fn func()) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if !e.backupEsperando() && e.mu.TryLock() {
			e.quem.Store(atividadeInventario)
			defer e.mu.Unlock()
			fn()
			return true
		}
		timer := time.NewTimer(esperaPelaVezDoInventario)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// inventoryLoop é o laço do inventário do bucket.
func inventoryLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, trava *exclusaoDoBucket) {
	if !podeInventariar(cfg) {
		return
	}
	espera := esperaDoInventario(cfg, primeiroInventario)
	for {
		timer := time.NewTimer(espera)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		espera = esperaDoInventario(cfg, rodadaDeInventario(ctx, c, s3c, cfg, trava))
	}
}

// esperaDoInventario aplica o intervalo configurado
// (ARKAME_RESTORE_TEST_INTERVAL_SEC, o mesmo do teste de restauração) à espera
// padrão: só encurta, e não encurta a espera de um dia do painel sem a rota.
func esperaDoInventario(cfg *config.Config, padrao time.Duration) time.Duration {
	if cfg == nil || cfg.RestoreTestIntervalSec <= 0 || padrao == esperaSemRotaDeInventario {
		return padrao
	}
	if d := time.Duration(cfg.RestoreTestIntervalSec) * time.Second; d < padrao {
		return d
	}
	return padrao
}

// semRotaDeInventario garante uma linha de log por processo quando o painel
// não tem a rota.
var semRotaDeInventario gosync.Once

// rotaDoPlanoDeInventario é a pergunta inicial (sem cursor).
func rotaDoPlanoDeInventario(cfg *config.Config) string {
	return "/api/agents/" + cfg.AgentID + "/inventory-plan?storage_id=" + url.QueryEscape(cfg.StorageID)
}

// rodadaDeInventario faz uma passada: pergunta ao painel se há inventário a
// fazer e, havendo, faz e relata. Devolve quanto esperar até a pergunta
// seguinte.
//
// Com o bucket ocupado nem pergunta: perguntar é o que emite a rodada, e uma
// rodada emitida que não roda custa ao cliente a espera de um dia pela
// reentrega.
func rodadaDeInventario(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, trava *exclusaoDoBucket) time.Duration {
	if !podeInventariar(cfg) {
		return intervaloDoInventario
	}
	if !trava.livre() {
		slog.Debug("inventário do bucket adiado: o bucket está ocupado (backup, limpeza ou teste de restauração)")
		return reprovaDaVerificacao
	}
	var primeira api.InventoryPlanResponse
	if err := c.ComPrazo(prazoDaPaginaDoInventario).GET(ctx, rotaDoPlanoDeInventario(cfg), &primeira); err != nil {
		var he *api.HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			semRotaDeInventario.Do(func() {
				slog.Info("o painel ainda não tem o inventário do bucket; pergunto de novo em um dia")
			})
			return esperaSemRotaDeInventario
		}
		slog.Debug("poll inventory-plan falhou", "err", err)
		return intervaloDoInventario
	}
	if primeira.RunID == "" {
		return intervaloDoInventario
	}
	if (primeira.Bucket != "" && primeira.Bucket != cfg.StorageBucket) || (primeira.StorageID != "" && primeira.StorageID != cfg.StorageID) {
		// De um bucket que este processo não atende: o processo irmão pega.
		slog.Debug("plano de inventário de outro armazenamento", "plano", primeira.Bucket, "meu", cfg.StorageBucket)
		return intervaloDoInventario
	}
	if !formaDeUUID.MatchString(primeira.RunID) {
		slog.Warn("plano de inventário recusado: run_id fora do formato")
		return intervaloDoInventario
	}
	// O prefixo é a pasta DESTE servidor: o inventário não lista a de outro,
	// diga o painel o que disser.
	if minha := primeira.PrefixRoot + "data/" + cfg.AgentID + "/"; primeira.Prefix != minha {
		slog.Warn("plano de inventário recusado: o prefixo não é a pasta deste servidor", "prefixo", primeira.Prefix)
		return intervaloDoInventario
	}
	executarInventario(ctx, c, s3c, cfg, &primeira, trava)
	return intervaloDoInventario
}

// executarInventario faz a rodada e relata.
func executarInventario(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, primeira *api.InventoryPlanResponse, trava *exclusaoDoBucket) {
	prazo, amostra := inventario.Limites(primeira.DeadlineSeconds, primeira.SampleSize)
	slog.Info("inventário do bucket pedido pelo painel", "run_id", primeira.RunID, "prefixo", primeira.Prefix)
	inicio := time.Now().UTC()

	relato, interrompida := inventario.Rodar(ctx, inventario.Opcoes{
		Bucket:  inventario.BucketS3{Cliente: s3c, Nome: cfg.StorageBucket},
		Prefixo: primeira.Prefix,
		Prazo:   prazo,
		Amostra: amostra,
		Janela:  trava.inventario,
		// Entre uma requisição e outra da mesma janela: backup esperando, a
		// janela acaba ali.
		Ceder: trava.backupEsperando,
		Proxima: func(ctx context.Context, cursor string) (*api.InventoryPlanResponse, error) {
			var p api.InventoryPlanResponse
			rota := "/api/agents/" + cfg.AgentID + "/inventory-plan?run_id=" + url.QueryEscape(primeira.RunID) + "&cursor=" + url.QueryEscape(cursor)
			if err := c.ComPrazo(prazoDaPaginaDoInventario).GET(ctx, rota, &p); err != nil {
				return nil, err
			}
			return &p, nil
		},
	}, primeira)
	if interrompida {
		// O serviço está parando: sem relato a rodada segue emitida e o painel
		// a reentrega (desde a primeira página).
		slog.Info("inventário do bucket interrompido pela parada do serviço; a rodada será refeita", "run_id", primeira.RunID)
		return
	}
	relato.RunID = primeira.RunID
	relato.StartedAt, relato.FinishedAt = inicio, time.Now().UTC()

	// O estado local do backup não pode seguir afirmando a versão que não está
	// no bucket, ou que está lá com outro tamanho: o próximo backup lê o
	// arquivo e o envia de novo.
	if relato.Complete {
		var baixas []estado.VersaoApagada
		for _, v := range relato.Missing {
			baixas = append(baixas, estado.VersaoApagada{Chave: v.Key, Versao: v.VersionID})
		}
		for _, v := range relato.SizeMismatch {
			baixas = append(baixas, estado.VersaoApagada{Chave: v.Key, Versao: v.VersionID})
		}
		darBaixaNoEstado(cfg, baixas)
	}

	inventario.Caber(&relato, inventario.TetoDoCorpo)
	// O relato sai mesmo com o serviço parando: a rodada terminou, e refazê-la
	// listaria o bucket inteiro de novo.
	fctx, fcancel := contextoDeFinalizacao(ctx)
	defer fcancel()
	var resposta api.InventoryResultResponse
	if err := c.POST(fctx, "/api/agents/"+cfg.AgentID+"/inventory-result", relato, &resposta); err != nil {
		var he *api.HTTPError
		switch {
		case errors.Is(err, api.ErrGone), errors.As(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusConflict):
			slog.Info("o painel não aceita mais o relato desta rodada do inventário (expirada ou já encerrada)", "run_id", relato.RunID)
		default:
			slog.Warn("relato do inventário do bucket falhou; o painel reentrega a rodada", "run_id", relato.RunID, "err", err)
		}
		return
	}
	nivel := slog.LevelInfo
	if !relato.Complete || relato.MissingTotal > 0 || relato.SizeMismatchTotal > 0 {
		nivel = slog.LevelWarn
	}
	slog.Log(ctx, nivel, "inventário do bucket concluído",
		"run_id", relato.RunID, "completo", relato.Complete, "interrompido", relato.Interrupted,
		"versoes", relato.PlanVersions, "encontradas", relato.Found, "faltando", relato.MissingTotal,
		"tamanho_diferente", relato.SizeMismatchTotal, "marcadores", relato.DeleteMarkersTotal,
		"a_mais", relato.ExtraVersions, "listagens", relato.ListRequests, "heads", relato.HeadRequests,
		"status_no_painel", resposta.Status, "marcadas_no_painel", resposta.MarkedUnavailable)
}
