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
	"strconv"
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

	token := &estadoDoToken{}
	send := func() { enviarHeartbeat(ctx, c, cfg, svc, token) }

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

// enviarHeartbeat manda um heartbeat e trata a resposta (renovação do token).
func enviarHeartbeat(ctx context.Context, c *api.Client, cfg *config.Config, svc service.Detected, token *estadoDoToken) {
	hb := api.HeartbeatRequest{
		AgentID:      cfg.AgentID,
		AgentVersion: version.Version,
		OS:           runtime.GOOS + "-" + runtime.GOARCH,
		ReportedAt:   time.Now().UTC(),
		ServiceName:  svc.Name,
		ServiceScope: svc.Scope,
	}
	var resp api.HeartbeatResponse
	if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/heartbeat", hb, &resp); err != nil {
		slog.Warn("heartbeat falhou", "err", err)
		return
	}
	slog.Debug("heartbeat ok")
	token.tratarRespostaDoHeartbeat(c, cfg, resp)
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
			if planoDeOutroProcesso(cfg, plan) {
				slog.Debug("plano de bucket atendido por outro processo deste agente",
					"plan_id", plan.ID, "bucket", plan.StorageRef.Bucket)
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

// planoDeOutroProcesso: o plano é de um bucket que outro processo deste
// agente atende (SIBLING_BUCKETS). Cada processo tem as credenciais de um
// bucket só; sem este filtro, todos os processos rodavam todos os planos — o
// irmão sem credencial abria uma sessão que falhava com 403, em paralelo com a
// sessão certa. A restauração já filtrava assim.
func planoDeOutroProcesso(cfg *config.Config, plan api.Plan) bool {
	b := plan.StorageRef.Bucket
	return cfg.StorageBucket != "" && b != "" && b != cfg.StorageBucket &&
		slices.Contains(cfg.SiblingBuckets, b)
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
		marcarFalha(ctx, c, cfg, startResp.SessionID, failBody)
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
	//
	// Com o serviço parando (ctx cancelado), o comando ainda roda: com o ctx
	// original ele era morto antes de começar e o dump ficava no disco. Ganha
	// até finalizacaoGraca depois do cancelamento.
	hctx, hcancel := contextoDeFinalizacao(ctx)
	r, err := hooks.Run(hctx, plan.PostHook, prazo)
	hcancel()
	if err != nil {
		// Falha aqui não invalida o backup: os arquivos já subiram. Fica
		// registrado para quem for investigar o disco cheio depois.
		slog.Warn("comando de depois falhou", "plan_id", plan.ID, "err", err)
		saidaDosHooks.WriteString("\n$ depois do backup (falhou)\n")
		saidaDosHooks.WriteString(r.Output)
	} else if r.Ran {
		saidaDosHooks.WriteString("\n$ depois do backup\n")
		saidaDosHooks.WriteString(r.Output)
	}

	if errors.Is(syncErr, syncengine.ErrBucketSemVersionamento) {
		// Nem "partial": o que subiu não tem VersionId, o painel não indexa
		// nada, e "complete" com zero arquivos é um backup que não restaura.
		marcarFalha(ctx, c, cfg, startResp.SessionID, struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}{
			ErrorCode:    "bucket_unversioned",
			ErrorMessage: syncErr.Error(),
		})
		return syncErr
	}

	totalFailure, completeStatus := avaliarSessao(result, syncErr)
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
		marcarFalha(ctx, c, cfg, startResp.SessionID, failBody)
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

	completeBody := struct {
		Status       string           `json:"status"`
		Stats        api.SessionStats `json:"stats"`
		VersionMap   []entry          `json:"version_map"`
		ErrorCode    string           `json:"error_code,omitempty"`
		ErrorMessage string           `json:"error_message,omitempty"`
	}{
		Status:     completeStatus,
		Stats:      result.Stats,
		VersionMap: versionMap,
	}
	if completeStatus == "partial" {
		// A causa da sessão parcial ia embora: o painel via "parcial" sem
		// saber por quê, e o log do agente não dizia nada além dos avisos
		// por arquivo.
		completeBody.ErrorCode = "sync_partial"
		completeBody.ErrorMessage = causaDoParcial(result, syncErr)
		slog.Warn("backup parcial", "plan_id", plan.ID, "session_id", startResp.SessionID,
			"arquivos_com_falha", result.FilesFailed, "err", syncErr, "causa", completeBody.ErrorMessage)
	}
	var completeResp struct {
		// Ponteiro: ausente (resposta de "not_running", painel antigo) não é
		// zero indexado.
		FilesIndexed *int `json:"files_indexed"`
	}
	// O /complete carrega o version_map inteiro — é o que torna o backup
	// restaurável. Perdê-lo num erro passageiro jogava fora o trabalho todo
	// (e a sessão ficava "running" para sempre): tenta de novo com recuo,
	// com prazo maior, e mesmo com o serviço parando.
	cctx, ccancel := contextoDeFinalizacao(ctx)
	defer ccancel()
	if err := concluirSessao(cctx, c,
		"/api/agents/"+cfg.AgentID+"/sessions/"+startResp.SessionID+"/complete",
		completeBody, &completeResp,
	); err != nil {
		// Só uma recusa definitiva do painel (4xx) diz que a sessão não foi
		// concluída. Prazo, rede ou cancelamento não dizem nada: o /complete
		// pode ter chegado e só a resposta se perdido — um /fail aqui
		// sobrescreveria um "complete" já gravado. Nesses casos a sessão fica
		// como está. Com 410 o agente foi arquivado: o /fail também levaria 410.
		if recusaDefinitiva(err) && !errors.Is(err, api.ErrGone) {
			marcarFalha(ctx, c, cfg, startResp.SessionID, struct {
				ErrorCode    string `json:"error_code"`
				ErrorMessage string `json:"error_message"`
			}{
				ErrorCode:    "complete_failed",
				ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o painel recusou a conclusão: %v", err),
			})
		} else {
			slog.Warn("/complete sem resposta definitiva; a sessão fica como está no painel",
				"session_id", startResp.SessionID, "err", err)
		}
		return fmt.Errorf("session complete: %w", err)
	}
	indexados := -1
	if completeResp.FilesIndexed != nil {
		indexados = *completeResp.FilesIndexed
		if indexados < len(versionMap) {
			// O painel descartou entradas (sem version_id, sha256 ou data
			// inválidos): esses arquivos subiram e não são restauráveis pelo
			// painel. A sessão fica como o painel a gravou.
			slog.Error("o painel indexou menos arquivos do que o agente enviou",
				"session_id", startResp.SessionID,
				"enviados", len(versionMap),
				"files_indexed", indexados)
		}
	}
	slog.Info("session concluída",
		"session_id", startResp.SessionID,
		"status", completeStatus,
		"files_uploaded", result.Stats.FilesUploaded,
		"files_indexed", indexados)
	return nil
}

// finalizacaoGraca é quanto a finalização (comando de depois, /fail,
// /complete, PATCH final da restauração) ainda tem depois que o serviço manda
// parar. Com o ctx cancelado elas não saíam: a sessão ficava "running" no
// painel para sempre e o comando de depois era morto.
var finalizacaoGraca = 30 * time.Second

// contextoDeFinalizacao é um contexto que não morre com o ctx: enquanto o
// serviço roda, não tem prazo próprio; quando o ctx é cancelado, ganha
// finalizacaoGraca para terminar.
func contextoDeFinalizacao(ctx context.Context) (context.Context, context.CancelFunc) {
	graca := finalizacaoGraca
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	parar := context.AfterFunc(ctx, func() {
		time.AfterFunc(graca, cancel)
	})
	return fctx, func() {
		parar()
		cancel()
	}
}

// marcarFalha avisa o painel que a sessão falhou, mesmo com o serviço parando.
func marcarFalha(ctx context.Context, c *api.Client, cfg *config.Config, sessionID string, body any) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizacaoGraca)
	defer cancel()
	if err := c.POST(fctx, "/api/agents/"+cfg.AgentID+"/sessions/"+sessionID+"/fail", body, nil); err != nil {
		slog.Warn("não consegui marcar a sessão como falha no painel", "session_id", sessionID, "err", err)
	}
}

// Tentativas do /complete: a primeira e mais quatro, com 2, 4, 8 e 16 s de
// recuo; cada uma com até prazoDoComplete (o version_map de um servidor
// grande leva mais que os 30 s padrão para o painel gravar).
var (
	recuosDoComplete = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	prazoDoComplete  = 5 * time.Minute
)

// recusaDefinitiva diz se o painel respondeu e recusou de vez: 4xx que não
// melhora tentando de novo (408 e 429 melhoram), ou 410 (agente arquivado).
// Erro de rede, prazo ou cancelamento não é recusa: o pedido pode ter chegado.
func recusaDefinitiva(err error) bool {
	if errors.Is(err, api.ErrGone) {
		return true
	}
	var he *api.HTTPError
	return errors.As(err, &he) && he.Status >= 400 && he.Status < 500 &&
		he.Status != 408 && he.Status != 429
}

// concluirSessao envia o /complete com novas tentativas em erro passageiro
// (rede, prazo, 5xx, 408, 429). Erro 4xx do painel (410 incluído) não melhora
// tentando de novo; e "not_running" quer dizer que uma tentativa anterior chegou e só a
// resposta se perdeu — a sessão já está concluída.
func concluirSessao(ctx context.Context, c *api.Client, path string, body, out any) error {
	longo := c.ComPrazo(prazoDoComplete)
	var err error
	for tentativa := 0; ; tentativa++ {
		err = longo.POST(ctx, path, body, out)
		if err == nil {
			return nil
		}
		if recusaDefinitiva(err) {
			var he *api.HTTPError
			if errors.As(err, &he) && strings.Contains(he.Body, "not_running") {
				slog.Info("o painel já tinha a sessão concluída (resposta anterior perdida)", "path", path)
				return nil
			}
			return err
		}
		if tentativa >= len(recuosDoComplete) || ctx.Err() != nil {
			return err
		}
		slog.Warn("/complete falhou; tentando de novo", "tentativa", tentativa+1, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(recuosDoComplete[tentativa]):
		}
	}
}

// causaDoParcial resume por que a sessão ficou parcial: quantos arquivos
// falharam (com o primeiro de exemplo) e o erro do walker, se houve.
func causaDoParcial(result *syncengine.Result, syncErr error) string {
	var partes []string
	if result.FilesFailed > 0 {
		p := fmt.Sprintf("%d arquivo(s) não subiram", result.FilesFailed)
		if result.PrimeiraFalha != "" {
			p += " (ex.: " + result.PrimeiraFalha + ")"
		}
		partes = append(partes, p)
	}
	if syncErr != nil {
		partes = append(partes, syncErr.Error())
	}
	msg := strings.Join(partes, "; ")
	if len(msg) > 4000 {
		msg = strings.ToValidUTF8(msg[:4000], "")
	}
	return msg
}

// avaliarSessao decide como a sessão termina.
//
// Falha total quando nada entrou no version_map e houve erro (do walker ou de
// upload): não pode virar "complete". Com algo no version_map — enviado agora
// OU já presente no bucket (dedup) —, o erro deixa a sessão "partial".
//
// O critério era FilesUploaded == 0: num dia sem mudança (tudo dedup) com uma
// pasta ilegível, o backup inteiro virava "failed", embora todo o resto
// estivesse salvo e indexável.
func avaliarSessao(result *syncengine.Result, syncErr error) (falhou bool, status string) {
	if result == nil {
		return true, ""
	}
	houveErro := syncErr != nil || result.FilesFailed > 0
	if houveErro && len(result.VersionMap) == 0 {
		return true, ""
	}
	if houveErro {
		return false, "partial"
	}
	return false, "complete"
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

	run := func() { processarFilaDeRestore(ctx, c, s3c, cfg, aquecendo) }

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

// limiteDeRestoreItems é quantos itens o agente pede por vez — o teto do
// painel. Sem ?limit, vinha o padrão do painel (100): com cem itens frios
// aquecendo no topo da fila, todos pulados pelo recuo, o item 101 em diante
// nunca chegava ao agente.
const limiteDeRestoreItems = 500

// processarFilaDeRestore busca um lote de restore_items e executa cada um.
// Item ainda no recuo do aquecimento é pulado, e o laço segue para os outros
// do lote.
func processarFilaDeRestore(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config, aquecendo map[string]*esperaDeAquecimento) {
	var resp api.ListRestoreItemsResponse
	if err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/restore-items?limit="+strconv.Itoa(limiteDeRestoreItems), &resp); err != nil {
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

		// Serviço parando no meio do item: não é falha do arquivo. Fica
		// "running", e o próximo arranque o refaz (recuperação de crash).
		if err != nil && ctx.Err() != nil {
			return
		}
		// O resultado de um item que terminou tem de chegar ao painel
		// mesmo que o serviço esteja parando.
		var resp api.RestoreItemUpdateResponse
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), finalizacaoGraca)
		perr := c.PATCH(pctx, "/api/agents/"+cfg.AgentID+"/restore-items/"+item.ItemID, update, &resp)
		pcancel()
		if perr != nil {
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
// intervaloMinimoFs é o menor intervalo entre duas aberturas do long-poll do
// explorador de pastas quando o painel responde 204 sem segurar a conexão.
var intervaloMinimoFs = 2 * time.Second

func fsBrowseLoop(ctx context.Context, c *api.Client, cfg *config.Config) {
	for {
		if ctx.Err() != nil {
			return
		}
		var resp api.FsRequestsResponse
		inicio := time.Now()
		err := c.GET(ctx, "/api/agents/"+cfg.AgentID+"/fs-requests", &resp)
		if errors.Is(err, api.ErrNotReady) {
			// 204: reabre o long-poll. Se o painel respondeu na hora em vez
			// de segurar (proxy na frente, painel sem long-poll), reabrir
			// direto vira um laço apertado de requisições: espera o resto
			// do intervalo mínimo antes.
			if falta := intervaloMinimoFs - time.Since(inicio); falta > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(falta):
				}
			}
			continue
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
			entries, lerr := fsbrowse.ListDir(cfg.HostRoot, req.Path)
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
// storage, sem VersionId ou, no desbaste, a versão atual da chave (ver
// internal/purge).
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
