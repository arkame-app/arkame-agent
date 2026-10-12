package daemon

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// 400 `invalid_batch`: o painel não aceita uma entrada do jeito que o agente
// a escreve. É recusa definitiva (contrato, item 19): o agente não repete o
// lote, não manda /complete e NÃO refaz o índice em outra sessão — a mesma
// entrada iria de novo. O backup falha com o motivo que o painel deu.
func TestLoteInvalidoERecusaDefinitiva(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.MaxEntradas = 20
	m.semear(50)
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota == "files" && strings.HasPrefix(string(corpo), `{"seq":2,`) {
			return 400, `{"error":"invalid_batch","reason":"modified_at","index":7}`
		}
		return 0, ""
	}
	err := m.rodar()
	if err == nil || !strings.Contains(err.Error(), "invalid_batch") || !strings.Contains(err.Error(), "modified_at") {
		t.Fatalf("o erro tem de levar o motivo do painel: %v", err)
	}
	sessoes := m.painel.Sessoes()
	if len(sessoes) != 1 {
		t.Fatalf("recusa definitiva não abre outra sessão: %d sessões", len(sessoes))
	}
	if s := sessoes[0]; s.Status != "failed" || s.ErrorCode != "complete_failed" || !strings.Contains(s.ErrorMessage, "invalid_batch") {
		t.Fatalf("%+v", s)
	}
	rotas := m.painel.Rotas()
	if rotas["complete"].Requisicoes != 0 || rotas["files"].Status[400] != 1 || rotas["start"].Requisicoes != 1 || rotas["fail"].Requisicoes != 1 {
		t.Fatalf("um 400 só, sem /complete e sem outra sessão, e um /fail: %+v", rotas)
	}
}

// O painel falso acompanha o de verdade nos motivos de `invalid_batch` do item
// 19 do contrato, e nos do mapa inline.
func TestPainelFalsoRecusaOQueOPainelRecusa(t *testing.T) {
	m := novaMesaDoDaemon(t)
	var ini struct {
		SessionID string `json:"session_id"`
	}
	inicio := map[string]any{"plan_id": "p1", "capabilities": []string{api.CapacidadeIndiceEmLotes}}
	if err := m.c.POST(t.Context(), "/api/agents/ag1/sessions/start", inicio, &ini); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("ab", 32)
	boa := map[string]any{"op": "upsert", "key": "data/ag1/ok.txt", "version_id": "v1", "size": 1, "sha256": sha, "modified_at": "2026-01-01T00:00:00Z"}
	com := func(campo string, valor any) map[string]any {
		e := map[string]any{}
		for k, v := range boa {
			e[k] = v
		}
		e[campo] = valor
		return e
	}
	for _, c := range []struct {
		nome    string
		entrada map[string]any
		motivo  string // "" = aceita
	}{
		{"chave de 1.024 bytes", com("key", strings.Repeat("k", 1024)), ""},
		{"chave de 1.025 bytes", com("key", strings.Repeat("k", 1025)), "key"},
		{"`removed` com chave de 1.025 bytes", map[string]any{"op": "removed", "key": strings.Repeat("k", 1025)}, "key"},
		{"versão de 1.025 bytes", com("version_id", strings.Repeat("v", 1025)), "version_id"},
		{"ano 0000", com("modified_at", "0000-06-01T00:00:00Z"), "modified_at"},
		{"ano 0001", com("modified_at", "0001-01-01T00:00:00Z"), ""},
		{"ano 9999", com("modified_at", "9999-12-31T23:59:59.999999999Z"), ""},
		{"ano 9999 no fuso, 10000 em UTC", com("modified_at", "9999-12-31T23:00:00-03:00"), "modified_at"},
	} {
		// Um seq fora de ordem: o painel confere as entradas antes do seq, e o
		// lote que passa da conferência não chega a ser gravado.
		err := m.c.POST(t.Context(), "/api/agents/ag1/sessions/"+ini.SessionID+"/files", map[string]any{"seq": 99, "entries": []any{boa, c.entrada}}, nil)
		var he *api.HTTPError
		switch {
		case c.motivo == "":
			// Passou da conferência das entradas: a recusa que sobra é a do seq.
			if !errors.As(err, &he) || he.Status != 409 || !strings.Contains(he.Body, "seq_gap") {
				t.Errorf("%s: devia passar da conferência (e parar no seq): %v", c.nome, err)
			}
		case !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Body, `"invalid_batch"`) ||
			!strings.Contains(he.Body, `"reason":"`+c.motivo+`"`) || !strings.Contains(he.Body, `"index":1`):
			t.Errorf("%s: queria 400 invalid_batch (%s, index 1): %v", c.nome, c.motivo, err)
		}
	}

	// O mapa inline: 400 `invalid_version_map`, e a sessão segue aberta.
	corpo := map[string]any{"status": "complete", "version_map": []any{boa, com("key", strings.Repeat("k", 1025))}}
	err := m.c.POST(t.Context(), "/api/agents/ag1/sessions/"+ini.SessionID+"/complete", corpo, nil)
	var he *api.HTTPError
	if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Body, `"invalid_version_map"`) || !strings.Contains(he.Body, `"reason":"key"`) {
		t.Fatalf("mapa inline com chave de 1.025 bytes: %v", err)
	}
	if s := m.painel.Ultima(); s.Status != "running" {
		t.Fatalf("a sessão do mapa recusado segue em curso: %+v", s)
	}
}
