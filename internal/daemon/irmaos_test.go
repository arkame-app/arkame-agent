package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Dois processos do mesmo agente (um por credencial): cada um roda só os
// planos do seu bucket. Antes, os dois rodavam todos.
func TestPlanoDeOutroProcesso(t *testing.T) {
	cfg := &config.Config{StorageBucket: "aws-b", SiblingBuckets: []string{"oci-b"}}
	plano := func(b string) api.Plan { return api.Plan{StorageRef: api.StorageRef{Bucket: b}} }

	if !planoDeOutroProcesso(cfg, plano("oci-b")) {
		t.Fatal("plano do bucket do irmão deveria ser pulado")
	}
	if planoDeOutroProcesso(cfg, plano("aws-b")) {
		t.Fatal("plano do próprio bucket roda")
	}
	if planoDeOutroProcesso(cfg, plano("desconhecido")) {
		t.Fatal("bucket fora dos irmãos segue como antes (roda e falha com erro visível)")
	}
	if planoDeOutroProcesso(&config.Config{}, plano("oci-b")) {
		t.Fatal("sem bucket configurado, nada é filtrado")
	}
}

// Dois armazenamentos do agente com bucket de mesmo nome (AWS e OCI, ou o
// mesmo bucket com prefixos diferentes), um por processo. O plano do irmão é
// pulado sem abrir sessão: antes, este processo fazia o /start (que avança o
// next_run_at) e falhava com wrong_storage, roubando a execução do irmão. O
// plano de outro armazenamento com outro bucket continua no wrong_storage.
func TestPlanoDeIrmaoComMesmoBucketNaoAbreSessao(t *testing.T) {
	vencido := time.Now().Add(-time.Minute).UTC()
	planos := []api.Plan{
		{ID: "p-irmao", Kind: "backup", SourcePaths: []string{t.TempDir()},
			StorageRef: api.StorageRef{ID: "s-irmao", Bucket: "backups"},
			Schedule:   api.Schedule{Type: "manual"}, NextRunAt: &vencido},
		// Controle: outro armazenamento com outro bucket ainda abre a sessão
		// e falha com wrong_storage — prova que a rodada chegou aos planos.
		{ID: "p-outro", Kind: "backup", SourcePaths: []string{t.TempDir()},
			StorageRef: api.StorageRef{ID: "s-outro", Bucket: "arquivo"},
			Schedule:   api.Schedule{Type: "manual"}, NextRunAt: &vencido},
	}
	corpoPlanos, _ := json.Marshal(planos)
	painel, c := novoPainel(t)
	painel.resposta = func(path string, _ int) (int, string) {
		if path == "/api/agents/a1/plans" {
			return http.StatusOK, string(corpoPlanos)
		}
		return 0, ""
	}
	cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageID: "s-meu", StorageBucket: "backups"}

	rodadaDePlanos(context.Background(), c, s3SemUso(t), cfg, &exclusaoDoBucket{})

	if n := painel.recebeu("/sessions/start"); n != 1 {
		t.Fatalf("esperava 1 sessão (só a do controle), veio %d; chamadas: %v", n, painel.chamadas)
	}
	if corpo := painel.corpo("/api/agents/a1/sessions/start"); !strings.Contains(corpo, `"plan_id":"p-outro"`) {
		t.Fatalf("abriu sessão para o plano do irmão: %s", corpo)
	}
	if corpo := painel.corpo("/api/agents/a1/sessions/s1/fail"); !strings.Contains(corpo, `"error_code":"wrong_storage"`) {
		t.Fatalf("o controle de outro bucket não falhou com wrong_storage: %s", corpo)
	}
}

func TestPlanoDeIrmaoComMesmoBucket(t *testing.T) {
	cfg := &config.Config{StorageID: "s-meu", StorageBucket: "backups"}
	plano := func(id, b string) api.Plan { return api.Plan{StorageRef: api.StorageRef{ID: id, Bucket: b}} }

	if !planoDeIrmaoComMesmoBucket(cfg, plano("s-irmao", "backups")) {
		t.Fatal("outro armazenamento, mesmo bucket: pular sem sessão")
	}
	if planoDeIrmaoComMesmoBucket(cfg, plano("s-meu", "backups")) {
		t.Fatal("plano do armazenamento instalado roda")
	}
	if planoDeIrmaoComMesmoBucket(cfg, plano("s-outro", "arquivo")) {
		t.Fatal("outro armazenamento com outro bucket segue para o wrong_storage")
	}
	if planoDeIrmaoComMesmoBucket(cfg, plano("", "backups")) {
		t.Fatal("plano sem id de armazenamento roda como antes")
	}
	if planoDeIrmaoComMesmoBucket(&config.Config{StorageBucket: "backups"}, plano("s-irmao", "backups")) {
		t.Fatal("sem STORAGE_ID (instalação antiga), nada muda")
	}
}

// Restauração com o mesmo cenário: o agente pede só os itens do seu
// armazenamento (storage_id na query) e, se o painel (antigo) mandar mesmo
// assim um item de outro armazenamento, não o executa nem o toca. Item sem
// storage_id segue pelo filtro de bucket, como antes.
func TestRestoreDeIrmaoComMesmoBucket(t *testing.T) {
	dest := t.TempDir()
	itens := []api.RestoreItem{
		{ItemID: "do-irmao", StorageID: "s-irmao", Bucket: "backups", Status: "queued",
			DestPath: dest, DestFilename: "a.txt", SourceKey: "k", SourceVersionID: "v"},
		{ItemID: "sem-storage", Bucket: "backups", Status: "queued",
			DestPath: dest, DestFilename: "b.txt", SourceKey: "k", SourceVersionID: "v"},
	}
	for _, caso := range []struct {
		nome      string
		storageID string
		querEsp   string
		irmaoToca bool
	}{
		{"com STORAGE_ID", "s-meu", "storage_id=s-meu", false},
		{"instalação antiga", "", "", true},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			var (
				mu      gosync.Mutex
				patches = map[string]int{}
				query   url.Values
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/restore-items") {
					mu.Lock()
					query = r.URL.Query()
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(api.ListRestoreItemsResponse{Items: itens})
					return
				}
				if r.Method == http.MethodPatch {
					mu.Lock()
					patches[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]++
					mu.Unlock()
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
			cfg := &config.Config{AgentID: "a1", HostRoot: "/", StorageID: caso.storageID, StorageBucket: "backups"}

			processarFilaDeRestore(context.Background(), c, s3SemUso(t), cfg, map[string]*esperaDeAquecimento{})

			mu.Lock()
			defer mu.Unlock()
			if caso.querEsp != "" {
				if query.Get("storage_id") != caso.storageID {
					t.Errorf("o agente não mandou storage_id na query: %v", query)
				}
			} else if query.Has("storage_id") {
				t.Errorf("sem STORAGE_ID, não há storage_id para mandar: %v", query)
			}
			if query.Get("limit") == "" {
				t.Errorf("o limit sumiu da query: %v", query)
			}
			if tocou := patches["do-irmao"] > 0; tocou != caso.irmaoToca {
				t.Errorf("item do irmão: %d PATCH (esperava tocar=%v)", patches["do-irmao"], caso.irmaoToca)
			}
			if patches["sem-storage"] == 0 {
				t.Error("item sem storage_id deveria seguir pelo filtro de bucket e ser executado")
			}
		})
	}
}
