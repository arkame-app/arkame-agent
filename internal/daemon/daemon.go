// Package daemon implementa o loop principal do agent:
//   - Heartbeat periódico ao painel
//   - Poll de plans ativos
//   - Execução de plans conforme schedule + janelas + throttle
//   - Probe periódico do bucket de storage (GetBucketVersioning, etc.)
package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/fsbrowse"
	"github.com/arkame-app/agent/internal/hooks"
	"github.com/arkame-app/agent/internal/purge"
	"github.com/arkame-app/agent/internal/restore"
	"github.com/arkame-app/agent/internal/scheduler"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/storage"
	syncengine "github.com/arkame-app/agent/internal/sync"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Run é o loop principal do daemon. Bloqueia até ctx ser cancelado.
func Run(ctx context.Context, cfg *config.Config) error {
	bearer, err := cfg.LoadToken()
	if err != nil {
		return fmt.Errorf("lendo bearer token: %w", err)
	}
	if bearer == "" {
		return fmt.Errorf("bearer token vazio — rode 'arkame-agent install' primeiro")
	}

	client, err := api.New(api.Options{
		BaseURL: cfg.PanelURL,
		Bearer:  bearer,
	})
	if err != nil {
		return fmt.Errorf("api client: %w", err)
	}

	s3Client, err := storage.NewS3Client(ctx, cfg)
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); heartbeatLoop(ctx, client, cfg) }()
	go func() { defer wg.Done(); probeLoop(ctx, client, s3Client, cfg) }()
	go func() { defer wg.Done(); planLoop(ctx, client, s3Client, cfg) }()
	go func() { defer wg.Done(); restoreLoop(ctx, client, s3Client, cfg) }()
	go func() { defer wg.Done(); fsBrowseLoop(ctx, client, cfg) }()
	go func() { defer wg.Done(); purgeLoop(ctx, client, s3Client, cfg) }()

	<-ctx.Done()
	slog.Info("ctx cancelado, aguardando loops encerrarem...")
	wg.Wait()
	return nil
}

func heartbeatLoop(ctx context.Context, c *api.Client, cfg *config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.HeartbeatIntervalSec) * time.Second)
	defer ticker.Stop()

	// Detecta o serviço uma vez (não muda em runtime) para o painel poder
	// mostrar o comando exato de reinício quando o agent cair.
	svc := service.Detect()

	send := func() {
		hb := api.HeartbeatRequest{
			AgentID:      cfg.AgentID,
			AgentVersion: version.Version,
			OS:           runtime.GOOS + "-" + runtime.GOARCH,
			ReportedAt:   time.Now().UTC(),
			ServiceName:  svc.Name,
			ServiceScope: svc.Scope,
		}
		if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/heartbeat", hb, nil); err != nil {
			slog.Warn("heartbeat falhou", "err", err)
			return
		}
		slog.Debug("heartbeat ok")
	}

	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// probeLoop chama Probe periodicamente (1×/h) e também atende solicitações
// on-demand ("Testar conexão" no painel) via poll a cada 30s do endpoint
// /probe-request — assim o usuário não espera até 1h pra ver o resultado.
func probeLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config) {
	periodic := time.NewTicker(1 * time.Hour)
	defer periodic.Stop()
	onDemand := time.NewTicker(30 * time.Second)
	defer onDemand.Stop()

	run := func() {
		if cfg.StorageBucket == "" || cfg.StorageID == "" {
			return
		}
		report := storage.Probe(ctx, s3c, cfg.StorageBucket, cfg.StorageID)
		body := struct {
			StorageID   string          `json:"storage_id"`
			Versioning  string          `json:"versioning"`
			ObjectLock  *api.ObjectLock `json:"object_lock,omitempty"`
			Lifecycle   []api.Lifecycle `json:"lifecycle,omitempty"`
			UsedBytes   int64           `json:"used_bytes"`
			ObjectCount int64           `json:"object_count"`
			Error       string          `json:"error,omitempty"`
		}{
			StorageID:   report.StorageID,
			Versioning:  report.Versioning,
			ObjectLock:  report.ObjectLock,
			Lifecycle:   report.Lifecycle,
			UsedBytes:   report.UsedBytes,
			ObjectCount: report.ObjectCount,
			Error:       report.Error,
		}
		if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/probe", body, nil); err != nil {
			slog.Warn("probe POST falhou", "err", err)
			return
		}
		slog.Info("probe reportado",
			"versioning", report.Versioning,
			"lifecycle_rules", len(report.Lifecycle),
			"used_bytes", report.UsedBytes,
			"objects", report.ObjectCount)
	}

	// Verifica se há probe on-demand pendente para o storage deste agent.
	checkOnDemand := func() bool {
		if cfg.StorageID == "" {
			return false
		}
		var resp struct {
			StorageIDs []string `json:"storage_ids"`
		}
		if err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/probe-request", &resp); err != nil {
			slog.Debug("poll probe-request falhou", "err", err)
			return false
		}
		for _, id := range resp.StorageIDs {
			if id == cfg.StorageID {
				return true
			}
		}
		return false
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-periodic.C:
			run()
		case <-onDemand.C:
			if checkOnDemand() {
				slog.Info("probe on-demand solicitado pelo painel")
				run()
			}
		}
	}
}

// planLoop puxa plans pro agent e executa os que devem rodar agora.
func planLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	run := func() {
		var plans []api.Plan
		if err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/plans", &plans); err != nil {
			slog.Warn("poll plans falhou", "err", err)
			return
		}
		now := time.Now().UTC()
		for _, plan := range plans {
			if plan.Kind != "backup" {
				continue
			}
			if !scheduler.ShouldRun(plan, now) {
				continue
			}
			slog.Info("executando plan", "plan_id", plan.ID, "paths", plan.SourcePaths)
			if err := executePlan(ctx, c, s3c, cfg, plan); err != nil {
				slog.Error("plan falhou", "plan_id", plan.ID, "err", err)
			}
		}
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// executePlan implementa o ciclo backup completo:
//  1. POST /sessions/start → recebe sessionId
//  2. syncengine.Run percorre paths, hash + upload S3 streamed
//  3. POST /sessions/[sid]/complete com version_map inline
//  4. Em erro de upload: POST /sessions/[sid]/fail
func executePlan(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, plan api.Plan) error {
	startReq := struct {
		PlanID            string   `json:"plan_id"`
		SourcePaths       []string `json:"source_paths"`
		ConsistencyMethod string   `json:"consistency_method,omitempty"`
		AgentVersion      string   `json:"agent_version,omitempty"`
	}{
		PlanID:       plan.ID,
		SourcePaths:  plan.SourcePaths,
		AgentVersion: version.Version,
	}
	var startResp struct {
		SessionID string `json:"session_id"`
		StorageID string `json:"storage_id"`
		StartedAt string `json:"started_at"`
	}
	if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/sessions/start", startReq, &startResp); err != nil {
		return fmt.Errorf("session start: %w", err)
	}
	slog.Info("session iniciada", "session_id", startResp.SessionID)

	// Comando de antes. Falha aqui aborta o backup, e é deliberado: seguir
	// adiante salvaria o dump da véspera achando que salvou o de hoje — pior
	// que não salvar, porque ninguém procura o que parece estar lá.
	prazo := time.Duration(plan.HookTimeoutSeconds) * time.Second
	var saidaDosHooks strings.Builder
	if r, err := hooks.Run(ctx, plan.PreHook, prazo); err != nil {
		slog.Error("comando de antes falhou; backup abortado",
			"plan_id", plan.ID, "exit_code", r.ExitCode, "timed_out", r.TimedOut)
		msg := fmt.Sprintf("comando de antes do backup falhou: %v", err)
		failBody := struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
			HookOutput   string `json:"hook_output,omitempty"`
		}{
			ErrorCode:    "pre_hook_failed",
			ErrorMessage: msg,
			HookOutput:   r.Output,
		}
		_ = c.POST(ctx, "/api/agents/"+cfg.AgentID+"/sessions/"+startResp.SessionID+"/fail", failBody, nil)
		return fmt.Errorf("%s", msg)
	} else if r.Ran {
		slog.Info("comando de antes concluído", "plan_id", plan.ID, "duracao", r.Duration)
		saidaDosHooks.WriteString("$ antes do backup\n")
		saidaDosHooks.WriteString(r.Output)
	}

	result, syncErr := syncengine.Run(ctx, syncengine.EngineOptions{
		S3:           s3c,
		Bucket:       plan.StorageRef.Bucket,
		PrefixRoot:   plan.StorageRef.PrefixRoot + "data/" + cfg.AgentID + "/",
		HostRoot:     cfg.HostRoot,
		SourcePaths:  plan.SourcePaths,
		ExcludeGlobs: plan.ExcludeGlobs,
		MaxMbps:      plan.Throttle.MaxMbps,
	})
	// Comando de depois: roda com sucesso OU com falha, porque é ele que limpa
	// o dump temporário — deixar o arquivo para trás encheria o disco do
	// cliente justamente nos dias em que o backup deu errado.
	if r, err := hooks.Run(ctx, plan.PostHook, prazo); err != nil {
		// Falha aqui não invalida o backup: os arquivos já subiram. Fica
		// registrado para quem for investigar o disco cheio depois.
		slog.Warn("comando de depois falhou", "plan_id", plan.ID, "err", err)
		saidaDosHooks.WriteString("\n$ depois do backup (falhou)\n")
		saidaDosHooks.WriteString(r.Output)
	} else if r.Ran {
		saidaDosHooks.WriteString("\n$ depois do backup\n")
		saidaDosHooks.WriteString(r.Output)
	}

	// Falha total: ou o walker abortou sem nada enviado, ou TODOS os arquivos
	// falharam no upload (nenhum enviado, nenhum dedup) — não pode virar "complete".
	totalFailure := (result == nil) ||
		(syncErr != nil && result.Stats.FilesUploaded == 0) ||
		(result.FilesFailed > 0 && len(result.VersionMap) == 0)
	if totalFailure {
		msg := "todos os arquivos falharam no upload"
		if syncErr != nil {
			msg = syncErr.Error()
		}
		failBody := struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}{
			ErrorCode:    "sync_failed",
			ErrorMessage: msg,
		}
		_ = c.POST(ctx, "/api/agents/"+cfg.AgentID+"/sessions/"+startResp.SessionID+"/fail", failBody, nil)
		if syncErr != nil {
			return syncErr
		}
		return fmt.Errorf("backup falhou: %s", msg)
	}

	// Mapeia FileEntry pro JSON que o painel espera
	type entry struct {
		Key        string    `json:"key"`
		VersionID  string    `json:"version_id"`
		Size       int64     `json:"size"`
		SHA256     string    `json:"sha256"`
		ModifiedAt time.Time `json:"modified_at"`
	}
	versionMap := make([]entry, 0, len(result.VersionMap))
	for _, e := range result.VersionMap {
		// FileEntry.SHA256 já vem em hex do engine; garante lowercase
		sha := e.SHA256
		if _, err := hex.DecodeString(sha); err != nil {
			slog.Warn("entry com sha256 inválido, ignorando", "key", e.Key)
			continue
		}
		versionMap = append(versionMap, entry{
			Key:        e.Key,
			VersionID:  e.VersionID,
			Size:       e.Size,
			SHA256:     sha,
			ModifiedAt: e.ModifiedAt,
		})
	}

	// Parcial se o walker reportou erro OU se algum arquivo falhou no upload
	// (mas pelo menos um foi enviado/dedup, senão teria caído em totalFailure).
	completeStatus := "complete"
	if syncErr != nil || result.FilesFailed > 0 {
		completeStatus = "partial"
	}
	completeBody := struct {
		Status     string           `json:"status"`
		Stats      api.SessionStats `json:"stats"`
		VersionMap []entry          `json:"version_map"`
	}{
		Status:     completeStatus,
		Stats:      result.Stats,
		VersionMap: versionMap,
	}
	var completeResp struct {
		FilesIndexed int `json:"files_indexed"`
	}
	if err := c.POST(
		ctx,
		"/api/agents/"+cfg.AgentID+"/sessions/"+startResp.SessionID+"/complete",
		completeBody,
		&completeResp,
	); err != nil {
		return fmt.Errorf("session complete: %w", err)
	}
	slog.Info("session concluída",
		"session_id", startResp.SessionID,
		"status", completeStatus,
		"files_uploaded", result.Stats.FilesUploaded,
		"files_indexed", completeResp.FilesIndexed)
	return nil
}

// restoreLoop puxa restore_items pendentes e executa cada um:
//  1. GET /restore-items → lista pendentes (queued/running)
//  2. Para cada item: PATCH status=running
//  3. restore.Run → GetObject + write + sha256 verify
//  4. PATCH status=complete (ou failed com error_message)
//
// Items running de execuções anteriores são reprocessados (recovery após crash).
// A idempotência da escrita vem do conflict_strategy=suffix-version.
// Recuo entre conferências de um item que está aquecendo.
//
// O laço acorda a cada PollIntervalSec (60 s por padrão), o que é certo para
// item que está transferindo e absurdo para item que está em Deep Archive: doze
// horas de espera davam 720 conferências, cada uma com um GetObject, um
// HeadObject e dois PATCH no painel. Com cem arquivos frios são ~144 mil
// chamadas à S3 e outras tantas escritas no nosso banco, para descobrir 719
// vezes a mesma coisa.
//
// Dobra a cada conferência até o teto. No pior caso o cliente espera quinze
// minutos a mais numa restauração de doze horas — e nós fazemos 48 conferências
// em vez de 720.
const (
	recuoInicial = 1 * time.Minute
	recuoMaximo  = 15 * time.Minute
)

// esperaDeAquecimento guarda quando voltar a olhar um item frio e de quanto é o
// recuo atual dele.
type esperaDeAquecimento struct {
	proxima time.Time
	recuo   time.Duration
}

// agendarRecuo marca a próxima conferência do item, dobrando o intervalo até o
// teto. Ver o comentário das constantes acima.
func agendarRecuo(m map[string]*esperaDeAquecimento, id string) {
	e, ok := m[id]
	if !ok {
		e = &esperaDeAquecimento{recuo: recuoInicial}
		m[id] = e
	} else if e.recuo < recuoMaximo {
		e.recuo *= 2
		if e.recuo > recuoMaximo {
			e.recuo = recuoMaximo
		}
	}
	e.proxima = time.Now().Add(e.recuo)
}

func restoreLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	// Quando voltar a olhar cada item que está aquecendo. Em memória de
	// propósito: reiniciar o agente confere uma vez a mais, que é barato e é o
	// comportamento certo depois de uma queda.
	aquecendo := map[string]*esperaDeAquecimento{}

	run := func() {
		var resp api.ListRestoreItemsResponse
		if err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/restore-items", &resp); err != nil {
			slog.Warn("poll restore-items falhou", "err", err)
			return
		}
		if len(resp.Items) == 0 {
			return
		}
		slog.Info("restore-items pendentes", "count", len(resp.Items))

		exec := restore.Options{S3: s3c, HostRoot: cfg.HostRoot}
		for _, item := range resp.Items {
			if ctx.Err() != nil {
				return
			}
			// Ainda no recuo: nem chega a falar com a S3 nem com o painel.
			if e, ok := aquecendo[item.ItemID]; ok && time.Now().Before(e.proxima) {
				continue
			}
			// Este processo só tem credenciais para o bucket configurado. Item de
			// outro bucket falha rápido com erro claro (em vez de um 403 confuso) —
			// se houver outro processo do agent com as credenciais certas, ele já
			// terá filtrado o item dele por aqui também.
			if cfg.StorageBucket != "" && item.Bucket != cfg.StorageBucket {
				if slices.Contains(cfg.SiblingBuckets, item.Bucket) {
					continue // outro processo deste agent atende esse bucket
				}
				fail := api.RestoreItemUpdate{
					Status:       "failed",
					ErrorCode:    "wrong_bucket",
					ErrorMessage: fmt.Sprintf("agent sem credenciais para o bucket %q (este processo atende %q)", item.Bucket, cfg.StorageBucket),
				}
				if err := c.PATCH(ctx, "/api/agents/"+cfg.AgentID+"/restore-items/"+item.ItemID, fail, nil); err != nil {
					slog.Warn("PATCH wrong_bucket falhou", "item_id", item.ItemID, "err", err)
				}
				slog.Warn("restore item de bucket não atendido", "item_id", item.ItemID, "bucket", item.Bucket)
				continue
			}
			// Marca running antes de tentar
			markRunning := api.RestoreItemUpdate{Status: "running"}
			if err := c.PATCH(ctx, "/api/agents/"+cfg.AgentID+"/restore-items/"+item.ItemID, markRunning, nil); err != nil {
				slog.Warn("PATCH running falhou, pulando item", "item_id", item.ItemID, "err", err)
				continue
			}

			err := restore.Run(ctx, exec, item)
			update := api.RestoreItemUpdate{}

			var warmReq *restore.ErrWarmingRequested
			var warmInProg *restore.ErrWarmingInProgress
			switch {
			case err == nil:
				delete(aquecendo, item.ItemID)
				update.Status = "complete"
				slog.Info("restore item OK",
					"item_id", item.ItemID, "key", item.SourceKey,
					"dest", item.DestPath+"/"+item.DestFilename)
			case errors.As(err, &warmReq):
				// Objeto em cold storage. Mantém status=running, próximo poll re-tenta.
				slog.Info("warming requested",
					"item_id", item.ItemID, "key", item.SourceKey,
					"class", warmReq.StorageClass, "tier", warmReq.Tier)
				update.Status = "running"
				update.ErrorMessage = "warming-requested"
				update.WarmingState = "requested"
				update.WarmingTier = warmReq.Tier
				update.WarmingETA = warmReq.PrevistoEm.Format(time.RFC3339)
				agendarRecuo(aquecendo, item.ItemID)
			case errors.As(err, &warmInProg):
				slog.Info("warming in progress, aguardando",
					"item_id", item.ItemID, "key", item.SourceKey,
					"class", warmInProg.StorageClass)
				update.Status = "running"
				update.ErrorMessage = "warming-in-progress"
				update.WarmingState = "in_progress"
				agendarRecuo(aquecendo, item.ItemID)
			default:
				delete(aquecendo, item.ItemID)
				update.Status = "failed"
				update.ErrorMessage = err.Error()
				// Objeto sumiu do bucket → sinaliza ao painel pra reconciliar o índice.
				if isObjectGone(err) {
					update.ErrorCode = "not_found"
				}
				slog.Error("restore item falhou",
					"item_id", item.ItemID, "key", item.SourceKey, "err", err)
			}

			var resp api.RestoreItemUpdateResponse
			if perr := c.PATCH(ctx, "/api/agents/"+cfg.AgentID+"/restore-items/"+item.ItemID, update, &resp); perr != nil {
				slog.Warn("PATCH final falhou", "item_id", item.ItemID, "err", perr)
				continue
			}
			if resp.JobStatus == "complete" || resp.JobStatus == "partial" || resp.JobStatus == "failed" {
				slog.Info("restore job concluído",
					"job_id", resp.JobID,
					"status", resp.JobStatus,
					"done", resp.ItemsDone,
					"failed", resp.ItemsFailed,
					"total", resp.ItemsTotal)
			}
		}
	}

	// Primeiro tick imediato pra recuperar items "running" deixados num crash
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// isObjectGone identifica erros S3 de objeto/versão inexistente — sinal pro
// painel reconciliar o índice (marcar a versão como indisponível).
func isObjectGone(err error) bool {
	if err == nil {
		return false
	}
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NoSuchVersion", "NotFound":
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "NoSuchKey") ||
		strings.Contains(msg, "NoSuchVersion") ||
		strings.Contains(msg, "status code: 404")
}

// fsBrowseLoop atende o explorador de pastas do wizard de plano no painel:
// long-poll em GET /fs-requests (o servidor segura a conexão até ~25s; 204 =
// nada pendente) e responde cada requisição com a listagem do diretório.
// Latência percebida pelo usuário: ~1-3s por nível expandido.
func fsBrowseLoop(ctx context.Context, c *api.Client, cfg *config.Config) {
	for {
		if ctx.Err() != nil {
			return
		}
		var resp api.FsRequestsResponse
		err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/fs-requests", &resp)
		if errors.Is(err, api.ErrNotReady) {
			continue // 204: reabre o long-poll
		}
		if err != nil {
			slog.Debug("poll fs-requests falhou", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second): // backoff em erro persistente
			}
			continue
		}

		for _, req := range resp.Requests {
			report := api.FsListingReport{}
			entries, lerr := fsbrowse.ListDir(req.Path)
			if lerr != nil {
				report.Error = lerr.Error()
			} else {
				report.Entries = make([]api.FsEntry, len(entries))
				for i, e := range entries {
					report.Entries[i] = api.FsEntry{Name: e.Name, Dir: e.Dir, Size: e.Size}
				}
			}
			if perr := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/fs-requests/"+req.ID, report, nil); perr != nil {
				slog.Warn("report de fs listing falhou", "req", req.ID, "err", perr)
			} else {
				slog.Debug("fs listing reportado", "path", req.Path, "entries", len(report.Entries))
			}
		}
	}
}

// purgeLoop aplica a retenção: pergunta ao painel se há versões a apagar no
// bucket e apaga.
//
// A pergunta é barata e quase sempre volta vazia — o painel emite no máximo
// uma rodada por dia por storage. Por isso o intervalo é de uma hora e não de
// um minuto: retenção é trabalho de fundo, e um servidor que ficou desligado
// alguns dias recupera o atraso na primeira pergunta que fizer.
//
// Este é o único loop do agent que destrói dado. Ele não decide nada: só
// executa o que o painel autorizou, e recusa o que estiver fora do prefixo do
// storage ou sem VersionId (ver internal/purge).
func purgeLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	run := func() {
		if cfg.StorageID == "" || cfg.StorageBucket == "" {
			return
		}

		var plano api.PurgePlanResponse
		path := "/api/agents/" + cfg.AgentID + "/purge-plan?storage_id=" + url.QueryEscape(cfg.StorageID)
		if err := c.GET(ctx, path, &plano); err != nil {
			slog.Debug("poll purge-plan falhou", "err", err)
			return
		}
		if plano.RunID == "" || len(plano.Versions) == 0 {
			return
		}
		if plano.Bucket != "" && plano.Bucket != cfg.StorageBucket {
			// O plano é de um bucket que este processo não atende. Silêncio: se
			// houver outro processo do agent com as credenciais certas, ele pega.
			slog.Debug("plano de expurgo de outro bucket", "plano", plano.Bucket, "meu", cfg.StorageBucket)
			return
		}

		slog.Info("expurgo de retenção autorizado pelo painel",
			"run_id", plano.RunID, "versoes", len(plano.Versions), "truncado", plano.Truncated)

		versoes := make([]purge.Version, len(plano.Versions))
		for i, v := range plano.Versions {
			versoes[i] = purge.Version{Key: v.Key, VersionID: v.VersionID, Size: v.Size, Reason: v.Reason}
		}

		apagadas, falhas := purge.Run(ctx, purge.Options{
			S3:         s3c,
			Bucket:     cfg.StorageBucket,
			PrefixRoot: plano.PrefixRoot,
		}, versoes)

		relato := api.PurgeResult{RunID: plano.RunID}
		for _, v := range apagadas {
			relato.Deleted = append(relato.Deleted, api.PurgeDeleted{Key: v.Key, VersionID: v.VersionID})
		}
		for _, f := range falhas {
			relato.Failed = append(relato.Failed, api.PurgeFailure{Key: f.Key, VersionID: f.VersionID, Error: f.Error})
		}

		var resposta api.PurgeResultResponse
		if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/purge-result", relato, &resposta); err != nil {
			// O relato se perdeu. As versões já saíram do bucket, mas o catálogo
			// segue dizendo que existem. A rodada expira em algumas horas e a
			// próxima recalcula — as versões já apagadas simplesmente não
			// aparecerão mais no bucket, e o relato seguinte corrige o catálogo.
			slog.Warn("relato de expurgo falhou; catálogo será corrigido na próxima rodada", "err", err)
			return
		}

		slog.Info("expurgo concluído",
			"versoes_apagadas", resposta.VersionsDeleted,
			"bytes_liberados", resposta.BytesFreed,
			"falhas", resposta.Failed,
			"recusadas_pelo_painel", resposta.Rejected)
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
