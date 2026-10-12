package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/estado"
)

// restaurarUm faz o painel falso entregar um item de restauração e roda a
// fila; devolve o que o agente mandou no PATCH final.
func (m *mesaDoDaemon) restaurarUm(item api.RestoreItem) api.RestoreItemUpdate {
	m.t.Helper()
	var final api.RestoreItemUpdate
	antes := m.painel.Responder
	m.painel.Responder = func(rota string, r *http.Request, corpo []byte) (int, string) {
		switch {
		case r.Method == http.MethodGet && rota == "restore-items":
			b, _ := json.Marshal(api.ListRestoreItemsResponse{Items: []api.RestoreItem{item}})
			return 200, string(b)
		case r.Method == http.MethodPatch && rota == item.ItemID:
			var u api.RestoreItemUpdate
			_ = json.Unmarshal(corpo, &u)
			if u.Status != "running" {
				final = u
			}
			return 200, `{}`
		}
		return 0, ""
	}
	processarFilaDeRestore(context.Background(), m.c, m.s3c, m.cfg, map[string]*esperaDeAquecimento{})
	m.painel.Responder = antes
	return final
}

// A restauração não achou a versão no bucket (`not_found`): a entrada dela sai
// do estado local, e o backup seguinte lê o arquivo e o envia de novo — sem
// isso o atalho o daria por guardado até a reconferência rotativa.
func TestRestauracaoNaoAchouAVersaoDaBaixaNoEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(20)
	m.rodarOK()
	k := m.chave("p04/arquivo-00004.txt")
	v, _ := m.s3.Atual(k)
	m.s3.ApagarVersao(k, v.ID)

	u := m.restaurarUm(api.RestoreItem{ItemID: "it1", Bucket: "b", StorageID: "st1", Status: "queued",
		DestPath: t.TempDir(), DestFilename: "x.txt", SourceKey: k, SourceVersionID: v.ID, SourceSha256: v.SHA256})
	if u.Status != "failed" || u.ErrorCode != "not_found" {
		t.Fatalf("PATCH final: %+v", u)
	}
	m.s3.Zerar()
	s := m.rodarOK()
	if m.s3.N("HeadObject") != 1 || m.s3.N("PutObject") != 1 || s.Modo != "delta" || s.Entradas != 1 {
		t.Fatalf("o backup seguinte reenvia só esse arquivo: s3=%v sessão=%+v", m.s3.Requisicoes(), s)
	}
	m.conferir()
}

// O objeto baixado não confere com o índice (sha256): a entrada também sai do
// estado, e o backup seguinte relê o arquivo e o confere no bucket.
func TestRestauracaoComHashDivergenteDaBaixaNoEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.s3.GuardarDados = true
	m.semear(20)
	m.rodarOK()
	k := m.chave("p05/arquivo-00005.txt")
	v, _ := m.s3.Atual(k)
	u := m.restaurarUm(api.RestoreItem{ItemID: "it2", Bucket: "b", StorageID: "st1", Status: "queued",
		DestPath: t.TempDir(), DestFilename: "x.txt", SourceKey: k, SourceVersionID: v.ID,
		SourceSha256: strings.Repeat("0", 64)})
	if u.Status != "failed" || !strings.Contains(u.ErrorMessage, "sha256 mismatch") {
		t.Fatalf("PATCH final: %+v", u)
	}
	m.s3.Zerar()
	s := m.rodarOK()
	if m.s3.N("HeadObject") != 1 || s.Entradas != 1 {
		t.Fatalf("o backup seguinte reconfere esse arquivo: s3=%v sessão=%+v", m.s3.Requisicoes(), s)
	}
	// Uma restauração que deu certo não mexe no estado.
	if u := m.restaurarUm(api.RestoreItem{ItemID: "it3", Bucket: "b", StorageID: "st1", Status: "queued",
		DestPath: t.TempDir(), DestFilename: "y.txt", SourceKey: k, SourceVersionID: v.ID, SourceSha256: v.SHA256}); u.Status != "complete" {
		t.Fatalf("PATCH final: %+v", u)
	}
	m.s3.Zerar()
	m.rodarOK()
	semS3AlemDoVersionamento(t, m.s3)
}

// A baixa que chega com um backup em curso (o arquivo de estado está aberto)
// espera e entra quando ele fecha: vale para o backup seguinte.
func TestBaixaDuranteOBackupEsperaOEstadoFechar(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(20)
	m.rodarOK()
	k := m.chave("p06/arquivo-00006.txt")
	v, _ := m.s3.Atual(k)
	m.painel.Responder = func(rota string, _ *http.Request, _ []byte) (int, string) {
		if rota == "complete" {
			darBaixaNoEstado(m.cfg, []estado.VersaoApagada{{Chave: k, Versao: v.ID}})
		}
		return 0, ""
	}
	m.s3.Zerar()
	m.rodarOK()
	semS3AlemDoVersionamento(t, m.s3)
	m.painel.Responder = nil
	m.s3.Zerar()
	if s := m.rodarOK(); m.s3.N("HeadObject") != 1 || s.Entradas != 1 {
		t.Fatalf("a baixa não valeu no backup seguinte: s3=%v sessão=%+v", m.s3.Requisicoes(), s)
	}
	m.conferir()
}
