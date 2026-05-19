// Package daemon implementa o loop principal do agent:
//   - Heartbeat periódico ao painel
//   - Poll de plans ativos
//   - Execução de plans conforme schedule + janelas + throttle
//   - Probe periódico do bucket de storage (GetBucketVersioning, etc.)
package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/scheduler"
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
	wg.Add(3)
	go func() { defer wg.Done(); heartbeatLoop(ctx, client, cfg) }()
	go func() { defer wg.Done(); probeLoop(ctx, client, s3Client, cfg) }()
	go func() { defer wg.Done(); planLoop(ctx, client, s3Client, cfg) }()

	<-ctx.Done()
	slog.Info("ctx cancelado, aguardando loops encerrarem...")
	wg.Wait()
	return nil
}

func heartbeatLoop(ctx context.Context, c *api.Client, cfg *config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.HeartbeatIntervalSec) * time.Second)
	defer ticker.Stop()

	send := func() {
		hb := api.HeartbeatRequest{
			AgentID:      cfg.AgentID,
			AgentVersion: version.Version,
			OS:           runtime.GOOS + "-" + runtime.GOARCH,
			ReportedAt:   time.Now().UTC(),
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

// probeLoop chama Probe periodicamente e reporta ao painel.
// Probe 1× por hora é suficiente — bucket config raramente muda.
func probeLoop(ctx context.Context, c *api.Client, s3c *s3.Client, cfg *config.Config) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	run := func() {
		if cfg.StorageBucket == "" || cfg.StorageID == "" {
			return
		}
		report := storage.Probe(ctx, s3c, cfg.StorageBucket, cfg.StorageID)
		body := struct {
			StorageID  string         `json:"storage_id"`
			Versioning string         `json:"versioning"`
			ObjectLock *api.ObjectLock `json:"object_lock,omitempty"`
			Lifecycle  []api.Lifecycle `json:"lifecycle,omitempty"`
			Error      string         `json:"error,omitempty"`
		}{
			StorageID:  report.StorageID,
			Versioning: report.Versioning,
			ObjectLock: report.ObjectLock,
			Lifecycle:  report.Lifecycle,
			Error:      report.Error,
		}
		if err := c.POST(ctx, "/api/agents/"+cfg.AgentID+"/probe", body, nil); err != nil {
			slog.Warn("probe POST falhou", "err", err)
			return
		}
		slog.Info("probe reportado", "versioning", report.Versioning, "lifecycle_rules", len(report.Lifecycle))
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
//   1. POST /sessions/start → recebe sessionId
//   2. syncengine.Run percorre paths, hash + upload S3 streamed
//   3. POST /sessions/[sid]/complete com version_map inline
//   4. Em erro de upload: POST /sessions/[sid]/fail
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

	result, syncErr := syncengine.Run(ctx, syncengine.EngineOptions{
		S3:           s3c,
		Bucket:       plan.StorageRef.Bucket,
		PrefixRoot:   plan.StorageRef.PrefixRoot + "data/" + cfg.AgentID + "/",
		HostRoot:     cfg.HostRoot,
		SourcePaths:  plan.SourcePaths,
		ExcludeGlobs: plan.ExcludeGlobs,
		MaxMbps:      plan.Throttle.MaxMbps,
	})
	if syncErr != nil && (result == nil || result.Stats.FilesUploaded == 0) {
		// Falha total — marca session como failed
		failBody := struct {
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}{
			ErrorCode:    "sync_failed",
			ErrorMessage: syncErr.Error(),
		}
		_ = c.POST(ctx, "/api/agents/"+cfg.AgentID+"/sessions/"+startResp.SessionID+"/fail", failBody, nil)
		return syncErr
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

	completeStatus := "complete"
	if syncErr != nil {
		completeStatus = "partial"
	}
	completeBody := struct {
		Status     string             `json:"status"`
		Stats      api.SessionStats   `json:"stats"`
		VersionMap []entry            `json:"version_map"`
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
