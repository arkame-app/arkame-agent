package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// envioQueParaOServico troca o rodarSync por um envio que "para o serviço"
// (cancela o ctx do daemon) no meio e volta como o engine volta: com o que
// já guardou e context.Canceled.
func envioQueParaOServico(t *testing.T, cancel context.CancelFunc, guardados int) {
	t.Helper()
	antes := rodarSync
	t.Cleanup(func() { rodarSync = antes })
	rodarSync = func(ctx context.Context, _ syncengine.EngineOptions) (*syncengine.Result, error) {
		r := &syncengine.Result{}
		for i := 0; i < guardados; i++ {
			r.VersionMap = append(r.VersionMap, api.FileEntry{
				Key: "k" + string(rune('a'+i)), VersionID: "v1", Size: 1, SHA256: strings.Repeat("a", 64)})
			r.Stats.FilesTotal++
			r.Stats.FilesUploaded++
		}
		cancel()
		<-ctx.Done()
		return r, ctx.Err()
	}
}

// Serviço parando com o envio em curso e nada guardado ainda: a sessão fecha
// como agent_stopped, e não sync_failed com "context canceled", que mandava
// conferir as pastas e o bucket.
func TestServicoParandoNoEnvioSemNadaGuardado(t *testing.T) {
	painel, c := novoPainel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envioQueParaOServico(t, cancel, 0)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	err := executePlan(ctx, c, s3SemUso(t), cfg, api.Plan{ID: "p1", Kind: "backup", StorageRef: api.StorageRef{Bucket: "b"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; queria context.Canceled", err)
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 1 {
		t.Fatalf("/fail chamado %d vezes; chamadas: %v", n, painel.chamadas)
	}
	if n := painel.recebeu("/sessions/s1/complete"); n != 0 {
		t.Fatalf("sessão sem nada guardado foi concluída; chamadas: %v", painel.chamadas)
	}
	f := lerFail(t, painel)
	if f.ErrorCode != "agent_stopped" || f.ErrorMessage != "interrompido: o serviço do agente parou" {
		t.Fatalf("/fail = %+v; queria agent_stopped / interrompido: o serviço do agente parou", f)
	}
}

// Serviço parando com parte do envio guardada: a sessão segue parcial (o que
// subiu é restaurável), mas com sync_interrupted e a contagem, não
// sync_partial culpando permissões. O /complete sai mesmo com o ctx do
// daemon já cancelado (contexto de finalização).
func TestServicoParandoNoEnvioComParteGuardada(t *testing.T) {
	painel, c := novoPainel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envioQueParaOServico(t, cancel, 2)
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	if err := executePlan(ctx, c, s3SemUso(t), cfg, api.Plan{ID: "p1", Kind: "backup", StorageRef: api.StorageRef{Bucket: "b"}}); err != nil {
		t.Fatalf("sessão parcial concluída não pode voltar com erro: %v (chamadas: %v)", err, painel.chamadas)
	}
	if n := painel.recebeu("/sessions/s1/fail"); n != 0 {
		t.Fatalf("/fail numa sessão com parte guardada; chamadas: %v", painel.chamadas)
	}
	got := lerComplete(t, painel)
	want := "envio interrompido: o serviço do agente parou; 2 arquivo(s) guardado(s) antes da parada"
	if got.Status != "partial" || got.ErrorCode != "sync_interrupted" || got.ErrorMessage != want {
		t.Fatalf("/complete = %+v; queria partial/sync_interrupted/%q", got, want)
	}
}

// context.Canceled vindo do envio sem o serviço ter parado (o ctx do daemon
// vivo) não é parada do serviço: segue a regra de sempre (sync_failed).
func TestEnvioCanceladoSemParadaDoServicoSegueSyncFailed(t *testing.T) {
	painel, c := novoPainel(t)
	antes := rodarSync
	t.Cleanup(func() { rodarSync = antes })
	rodarSync = func(context.Context, syncengine.EngineOptions) (*syncengine.Result, error) {
		return &syncengine.Result{}, context.Canceled
	}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/"}

	_ = executePlan(context.Background(), c, s3SemUso(t), cfg, api.Plan{ID: "p1", Kind: "backup", StorageRef: api.StorageRef{Bucket: "b"}})
	if f := lerFail(t, painel); f.ErrorCode != "sync_failed" {
		t.Fatalf("error_code = %q; queria sync_failed", f.ErrorCode)
	}
}
