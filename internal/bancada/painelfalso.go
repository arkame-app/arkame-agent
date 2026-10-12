package bancada

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// PainelFalso implementa, em memória, as rotas do painel que o agente usa —
// e, das sessões, o contrato do índice em lotes e delta: oferece `index` no
// /sessions/start a quem declarou a capacidade, confere `seq` e idempotência
// em /files, confere a contagem e a base no /complete. Com SemLotes vira o
// painel de antes do contrato (v2.76.0): /complete com o version_map inteiro
// e o teto de 150 mil arquivos / 48 MiB.
//
// Ele mantém o catálogo de "arquivo presente na última sessão concluída" de
// cada agente + plano, para os testes conferirem que inline, full e delta
// chegam ao mesmo resultado.
type PainelFalso struct {
	// SemLotes: painel antigo, sem `index` na resposta do start.
	SemLotes bool
	// SemDelta: oferece lotes, mas sempre inventário completo.
	SemDelta bool
	// SemRemovidosEmParcial: o painel de antes do item 27 do contrato — não
	// oferece `removed_partial`, e o `removed` de sessão parcial é ignorado.
	SemRemovidosEmParcial bool
	// MaxEntradas e MaxBytes por lote (padrão do contrato: 5.000 e 2 MiB).
	MaxEntradas int
	MaxBytes    int
	// Teto do /complete inline (padrão: 150 mil arquivos, 48 MiB).
	MaxInlineArquivos int
	MaxInlineBytes    int
	// MaxArquivosDaSessao é o teto de entradas de uma sessão em lotes (zero:
	// sem teto). No painel de verdade é 150 mil enquanto a sessão ainda grava
	// o catálogo antigo e 20 milhões depois do corte. Passou: 413
	// `index_too_large` e a sessão fecha como falha na mesma resposta.
	MaxArquivosDaSessao int
	// SemCatalogo não guarda as entradas, só conta: para medir escala sem a
	// memória do painel falso.
	SemCatalogo bool
	// Responder, se devolver status != 0, responde no lugar do painel. rota é
	// o último segmento do caminho ("start", "files", "complete", "fail"…).
	Responder func(rota string, r *http.Request, corpo []byte) (int, string)
	// Engolir, se devolver true, faz o painel processar a requisição e
	// responder 502: o pedido chegou e a resposta se perdeu.
	Engolir func(rota string, corpo []byte) bool
	// Planos é o que GET /plans devolve. Iniciar a sessão de um plano tira o
	// next_run_at dele (disparo único), como o painel faz com o plano manual.
	Planos []api.Plan

	mu       sync.Mutex
	sessoes  map[string]*SessaoFalsa
	ordem    []string
	catalogo map[string]map[string]api.FileEntry // agente/plano → chave → versão presente
	ultima   map[string]string                   // agente/plano → última sessão concluída
	rotas    map[string]*EstatDaRota
	lotes    []LoteRecebido
	// baseErrada: quantos /complete em delta ainda respondem base_mismatch.
	baseErrada int
	hbCaps     []string
	probes     []json.RawMessage
}

// SessaoFalsa é uma sessão de backup no painel falso.
type SessaoFalsa struct {
	ID           string
	Agente       string
	Plano        string
	Status       string // running | complete | partial | failed
	ErrorCode    string
	ErrorMessage string
	// Oferta feita no start: "inline" ou "batches", com ou sem delta.
	Oferta      string
	OfertaDelta bool
	// OfertaRemovidosEmParcial: a sessão recebeu `removed_partial: true` — o
	// `removed` dela vale mesmo que feche parcial.
	OfertaRemovidosEmParcial bool
	Base                     string
	// Modo com que fechou: "inline", "full" ou "delta".
	Modo         string
	Capacidades  []string
	Lotes        int
	Entradas     int64
	hashes       map[int][32]byte
	porLote      []int
	ops          []api.IndexEntry
	Complete     map[string]any // corpo do /complete, sem o version_map
	Indexados    int
	Presentes    int // arquivos presentes depois de a sessão fechar
	RemocoesSemX int // removed/unselected de chave que não estava na base
	// SemVersao: upserts sem version_id — aceitos e contados, não catalogados;
	// a sessão fecha `partial` com `missing_version_id`.
	SemVersao int
	// RemovidosIgnorados: `removed` de sessão parcial sem a oferta
	// `removed_partial`, que o painel não aplica.
	RemovidosIgnorados int
}

// EstatDaRota é o tráfego de uma rota.
type EstatDaRota struct {
	Requisicoes int64
	Bytes       int64
	MaiorCorpo  int64
	Status      map[int]int64
}

// LoteRecebido é um POST /files aceito.
type LoteRecebido struct {
	Sessao   string
	Seq      int
	Entradas int
	Bytes    int
}

// NovoPainelFalso cria o painel com os limites do contrato.
func NovoPainelFalso() *PainelFalso {
	return &PainelFalso{
		MaxEntradas:       5000,
		MaxBytes:          2 << 20,
		MaxInlineArquivos: 150000,
		MaxInlineBytes:    48 << 20,
		sessoes:           map[string]*SessaoFalsa{},
		catalogo:          map[string]map[string]api.FileEntry{},
		ultima:            map[string]string{},
		rotas:             map[string]*EstatDaRota{},
	}
}

// Sessoes devolve as sessões na ordem em que começaram.
func (p *PainelFalso) Sessoes() []*SessaoFalsa {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*SessaoFalsa, 0, len(p.ordem))
	for _, id := range p.ordem {
		c := *p.sessoes[id]
		out = append(out, &c)
	}
	return out
}

// Ultima devolve a sessão mais recente (nil se não houve).
func (p *PainelFalso) Ultima() *SessaoFalsa {
	s := p.Sessoes()
	if len(s) == 0 {
		return nil
	}
	return s[len(s)-1]
}

// Catalogo devolve os arquivos presentes na última sessão concluída do
// agente + plano: chave → versão.
func (p *PainelFalso) Catalogo(agente, plano string) map[string]api.FileEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]api.FileEntry, len(p.catalogo[agente+"/"+plano]))
	for k, v := range p.catalogo[agente+"/"+plano] {
		out[k] = v
	}
	return out
}

// Rotas devolve o tráfego por rota.
func (p *PainelFalso) Rotas() map[string]EstatDaRota {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]EstatDaRota{}
	for k, v := range p.rotas {
		c := *v
		c.Status = map[int]int64{}
		for s, n := range v.Status {
			c.Status[s] = n
		}
		out[k] = c
	}
	return out
}

// LotesRecebidos devolve os lotes aceitos, na ordem.
func (p *PainelFalso) LotesRecebidos() []LoteRecebido {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]LoteRecebido(nil), p.lotes...)
}

// Zerar zera as estatísticas de tráfego (não as sessões nem o catálogo).
func (p *PainelFalso) Zerar() {
	p.mu.Lock()
	p.rotas = map[string]*EstatDaRota{}
	p.lotes = nil
	p.mu.Unlock()
}

// PerderLotes simula o painel perdendo os lotes de seq > ate de uma sessão
// em curso (a tabela de preparo é UNLOGGED): o próximo lote leva 409
// `seq_gap`, e o /complete, 409 `index_mismatch`.
func (p *PainelFalso) PerderLotes(sessao string, ate int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessoes[sessao]
	if s == nil || ate >= s.Lotes {
		return
	}
	fora := 0
	for _, n := range s.porLote[ate:] {
		fora += n
	}
	s.porLote = s.porLote[:ate]
	if len(s.ops) >= fora {
		s.ops = s.ops[:len(s.ops)-fora]
	}
	s.Entradas -= int64(fora)
	for seq := ate + 1; seq <= s.Lotes; seq++ {
		delete(s.hashes, seq)
	}
	s.Lotes = ate
}

// ForcarBaseErrada faz os próximos n /complete em delta responderem 409
// `base_mismatch`.
func (p *PainelFalso) ForcarBaseErrada(n int) {
	p.mu.Lock()
	p.baseErrada = n
	p.mu.Unlock()
}

// CapacidadesDoHeartbeat é a lista do último heartbeat.
func (p *PainelFalso) CapacidadesDoHeartbeat() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hbCaps)
}

// Probes devolve os corpos de POST /probe recebidos.
func (p *PainelFalso) Probes() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.probes)
}

// Disparar marca os planos para rodar agora.
func (p *PainelFalso) Disparar() {
	p.mu.Lock()
	defer p.mu.Unlock()
	agora := time.Now().UTC().Add(-time.Second)
	for i := range p.Planos {
		p.Planos[i].NextRunAt = &agora
	}
}

func responder(w http.ResponseWriter, status int, corpo any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(corpo)
}

func (p *PainelFalso) anotar(rota string, bytes, status int) {
	e := p.rotas[rota]
	if e == nil {
		e = &EstatDaRota{Status: map[int]int64{}}
		p.rotas[rota] = e
	}
	e.Requisicoes++
	e.Bytes += int64(bytes)
	e.MaiorCorpo = max(e.MaiorCorpo, int64(bytes))
	e.Status[status]++
}

// descarte joga fora a resposta do painel (ver Engolir).
type descarte struct{}

func (descarte) Header() http.Header         { return http.Header{} }
func (descarte) Write(b []byte) (int, error) { return len(b), nil }
func (descarte) WriteHeader(int)             {}

// gravador anota o status de cada resposta.
type gravador struct {
	http.ResponseWriter
	status int
}

func (g *gravador) WriteHeader(s int) {
	g.status = s
	g.ResponseWriter.WriteHeader(s)
}

func (p *PainelFalso) ServeHTTP(w0 http.ResponseWriter, r *http.Request) {
	corpo, _ := io.ReadAll(r.Body)
	seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// api/agents/{id}/rota… ; sessions/{sid}/rota
	rota, agente, sid := seg[len(seg)-1], "", ""
	if len(seg) >= 3 {
		agente = seg[2]
	}
	if len(seg) >= 6 && seg[3] == "sessions" {
		sid = seg[4]
	}
	if len(seg) == 5 && seg[3] == "fs-requests" {
		rota = "fs-requests"
	}
	engolida := false
	if p.Engolir != nil && p.Engolir(rota, corpo) {
		engolida = true
		real := w0
		w0 = descarte{}
		defer func() {
			real.WriteHeader(http.StatusBadGateway)
		}()
	}
	w := &gravador{ResponseWriter: w0, status: 200}
	defer func() {
		if engolida {
			w.status = http.StatusBadGateway
		}
		p.mu.Lock()
		p.anotar(rota, len(corpo), w.status)
		p.mu.Unlock()
	}()
	if p.Responder != nil {
		if st, txt := p.Responder(rota, r, corpo); st != 0 {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, txt)
			return
		}
	}
	switch rota {
	case "heartbeat":
		var hb struct {
			Capabilities []string `json:"capabilities"`
		}
		_ = json.Unmarshal(corpo, &hb)
		p.mu.Lock()
		p.hbCaps = hb.Capabilities
		p.mu.Unlock()
		responder(w, 200, map[string]any{"ok": true})
	case "plans":
		p.mu.Lock()
		planos := slices.Clone(p.Planos)
		p.mu.Unlock()
		if planos == nil {
			planos = []api.Plan{}
		}
		responder(w, 200, planos)
	case "start":
		p.start(w, agente, corpo)
	case "files":
		p.files(w, sid, corpo)
	case "complete":
		p.complete(w, sid, corpo)
	case "fail":
		p.fail(w, sid, corpo)
	case "probe":
		p.mu.Lock()
		p.probes = append(p.probes, json.RawMessage(slices.Clone(corpo)))
		p.mu.Unlock()
		responder(w, 200, map[string]any{"ok": true})
	case "probe-request":
		responder(w, 200, map[string]any{"storage_ids": []string{}})
	case "restore-items":
		responder(w, 200, map[string]any{"items": []any{}})
	case "fs-requests":
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	case "purge-plan":
		responder(w, 200, map[string]any{})
	default:
		responder(w, 200, map[string]any{})
	}
}

func (p *PainelFalso) start(w http.ResponseWriter, agente string, corpo []byte) {
	var req struct {
		PlanID       string   `json:"plan_id"`
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(corpo, &req) != nil {
		responder(w, 400, map[string]any{"error": "invalid_json"})
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	par := agente + "/" + req.PlanID
	// O agente que abre outra sessão do mesmo plano provou que largou a
	// anterior: ela fecha como abandonada (regra da fase 1).
	for _, s := range p.sessoes {
		if s.Agente == agente && s.Plano == req.PlanID && s.Status == "running" {
			s.Status, s.ErrorCode = "failed", "session_abandoned"
			s.ops = nil
		}
	}
	s := &SessaoFalsa{
		ID:          fmt.Sprintf("00000000-0000-4000-8000-%012d", len(p.ordem)+1),
		Agente:      agente,
		Plano:       req.PlanID,
		Status:      "running",
		Oferta:      "inline",
		Capacidades: req.Capabilities,
		hashes:      map[int][32]byte{},
	}
	p.sessoes[s.ID] = s
	p.ordem = append(p.ordem, s.ID)
	for i := range p.Planos {
		if p.Planos[i].ID == req.PlanID {
			p.Planos[i].NextRunAt = nil
		}
	}
	resp := map[string]any{
		"session_id": s.ID,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}
	if !p.SemLotes && slices.Contains(req.Capabilities, api.CapacidadeIndiceEmLotes) {
		s.Oferta = "batches"
		s.OfertaDelta = !p.SemDelta && slices.Contains(req.Capabilities, api.CapacidadeIndiceDelta)
		s.Base = p.ultima[par]
		var base any
		if s.Base != "" {
			base = s.Base
		}
		indice := map[string]any{
			"mode": "batches", "delta": s.OfertaDelta,
			"max_entries": p.MaxEntradas, "max_bytes": p.MaxBytes,
			"base_session_id": base,
		}
		// Como o painel de verdade: `removed_partial` acompanha o delta, só
		// para quem declarou a capacidade, e o campo não vem quando é falso.
		// O de verdade só oferece delta com base; aqui `delta: true` vem
		// também sem base (o agente manda `full` do mesmo jeito), e por isso
		// a base é conferida à parte.
		if s.OfertaDelta && s.Base != "" && !p.SemRemovidosEmParcial && slices.Contains(req.Capabilities, api.CapacidadeRemovidosEmParcial) {
			s.OfertaRemovidosEmParcial = true
			indice["removed_partial"] = true
		}
		resp["index"] = indice
	}
	responder(w, 200, resp)
}

// maxBytesDoTexto é o teto, em bytes UTF-8, da chave e da versão de uma
// entrada do índice no painel (o do S3).
const maxBytesDoTexto = 1024

// textoGuardavel diz se a chave ou a versão cabe no catálogo do painel: até
// 1.024 bytes e sem caractere nulo (contrato, item 19). O painel de verdade
// também recusa o substituto UTF-16 solto; aqui ele não chega a ser visto — o
// encoding/json já o troca por U+FFFD ao ler o corpo.
func textoGuardavel(t string) bool {
	return len(t) <= maxBytesDoTexto && !strings.ContainsRune(t, 0)
}

// dataGuardavel diz se a data de modificação está nos anos 0001–9999, em UTC
// — a faixa que o painel aceita (contrato, item 19).
func dataGuardavel(t time.Time) bool {
	a := t.UTC().Year()
	return a >= 1 && a <= 9999
}

func (p *PainelFalso) files(w http.ResponseWriter, sid string, corpo []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	limite := map[string]any{"entries": p.MaxEntradas, "bytes": p.MaxBytes}
	// O painel recusa pelo tamanho antes de ler o corpo e de olhar a sessão.
	if len(corpo) > p.MaxBytes {
		responder(w, 413, map[string]any{"error": "batch_too_large", "limit": limite})
		return
	}
	var bruto struct {
		Seq     json.RawMessage `json:"seq"`
		Entries json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(corpo, &bruto) != nil {
		responder(w, 400, map[string]any{"error": "invalid_json"})
		return
	}
	var lote api.IndexBatch
	if json.Unmarshal(bruto.Seq, &lote.Seq) != nil || lote.Seq < 1 {
		responder(w, 400, map[string]any{"error": "invalid_batch", "reason": "seq"})
		return
	}
	if json.Unmarshal(bruto.Entries, &lote.Entries) != nil || lote.Entries == nil {
		responder(w, 400, map[string]any{"error": "invalid_batch", "reason": "entries"})
		return
	}
	if len(lote.Entries) > p.MaxEntradas {
		responder(w, 413, map[string]any{"error": "batch_too_large", "limit": limite})
		return
	}
	semVersao := 0
	for i, e := range lote.Entries {
		motivo := ""
		switch {
		case e.Op != api.OpUpsert && e.Op != api.OpRemoved && e.Op != api.OpUnselected:
			motivo = "op"
		case e.Key == "" || !textoGuardavel(e.Key):
			motivo = "key"
		case e.Op != api.OpUpsert:
		case !textoGuardavel(e.VersionID):
			motivo = "version_id"
		case e.Size == nil || *e.Size < 0:
			motivo = "size"
		case len(e.SHA256) != 64 || strings.Trim(strings.ToLower(e.SHA256), "0123456789abcdef") != "":
			motivo = "sha256"
		case e.ModifiedAt == nil || !dataGuardavel(*e.ModifiedAt):
			motivo = "modified_at"
		case e.VersionID == "":
			semVersao++
		}
		if motivo != "" {
			// Lote torto é recusado inteiro, com a posição da entrada.
			responder(w, 400, map[string]any{"error": "invalid_batch", "reason": motivo, "index": i})
			return
		}
	}
	s := p.sessoes[sid]
	if s == nil {
		responder(w, 404, map[string]any{"error": "session_not_found"})
		return
	}
	if s.Status != "running" {
		responder(w, 409, map[string]any{"error": "session_closed"})
		return
	}
	if s.Oferta != "batches" {
		responder(w, 400, map[string]any{"error": "batches_not_offered"})
		return
	}
	h := sha256.Sum256(corpo)
	if lote.Seq <= s.Lotes {
		if s.hashes[lote.Seq] != h {
			responder(w, 409, map[string]any{"error": "seq_conflict"})
			return
		}
		responder(w, 200, api.IndexBatchResponse{OK: true, Seq: lote.Seq, Received: s.Entradas})
		return
	}
	if lote.Seq != s.Lotes+1 {
		responder(w, 409, map[string]any{"error": "seq_gap", "expected": s.Lotes + 1})
		return
	}
	if total := s.Entradas + int64(len(lote.Entries)); p.MaxArquivosDaSessao > 0 && total > int64(p.MaxArquivosDaSessao) {
		// Como no /complete inline: a sessão fecha como falha, com o motivo,
		// na mesma resposta que recusa.
		s.Status, s.ErrorCode = "failed", "index_too_large"
		s.ErrorMessage = fmt.Sprintf("index_too_large: %d arquivos; limite %d", total, p.MaxArquivosDaSessao)
		s.ops = nil
		responder(w, 413, map[string]any{
			"error": "index_too_large", "message": s.ErrorMessage, "files": total,
			"limit": map[string]any{"files": p.MaxArquivosDaSessao},
		})
		return
	}
	s.Lotes = lote.Seq
	s.hashes[lote.Seq] = h
	s.porLote = append(s.porLote, len(lote.Entries))
	s.Entradas += int64(len(lote.Entries))
	s.SemVersao += semVersao
	if !p.SemCatalogo {
		s.ops = append(s.ops, lote.Entries...)
	}
	p.lotes = append(p.lotes, LoteRecebido{Sessao: sid, Seq: lote.Seq, Entradas: len(lote.Entries), Bytes: len(corpo)})
	responder(w, 200, api.IndexBatchResponse{OK: true, Seq: lote.Seq, Received: s.Entradas})
}

func (p *PainelFalso) complete(w http.ResponseWriter, sid string, corpo []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessoes[sid]
	if s == nil {
		responder(w, 404, map[string]any{"error": "session_not_found"})
		return
	}
	par := s.Agente + "/" + s.Plano
	var req struct {
		Status           string            `json:"status"`
		VersionMap       []api.FileEntry   `json:"version_map"`
		Index            *api.IndexSummary `json:"index"`
		CloudOnlySkipped int               `json:"cloud_only_skipped"`
		ReparseSkipped   int               `json:"reparse_skipped"`
		ErrorCode        string            `json:"error_code"`
		ErrorMessage     string            `json:"error_message"`
	}
	// O painel decide pelo tamanho antes de ler: o teto do corpo vale para
	// qualquer /complete (o de lotes é pequeno e nunca chega perto).
	if len(corpo) > p.MaxInlineBytes {
		p.recusarInline(w, s, 0, len(corpo))
		return
	}
	if json.Unmarshal(corpo, &req) != nil {
		responder(w, 400, map[string]any{"error": "invalid_json"})
		return
	}
	if len(req.VersionMap) > p.MaxInlineArquivos {
		p.recusarInline(w, s, len(req.VersionMap), len(corpo))
		return
	}
	if s.Status != "running" {
		responder(w, 400, map[string]any{"error": "not_running", "message": "session já está em status " + s.Status})
		return
	}
	if req.Status != "complete" && req.Status != "partial" {
		responder(w, 400, map[string]any{"error": "invalid_status"})
		return
	}
	var resto map[string]any
	_ = json.Unmarshal(corpo, &resto)
	delete(resto, "version_map")

	status, codigo, mensagem := req.Status, req.ErrorCode, req.ErrorMessage
	resposta := map[string]any{"ok": true}
	if req.Index == nil {
		// O mapa inline — também de uma sessão que recebeu a oferta de lotes:
		// os lotes já mandados são descartados.
		s.Modo = "inline"
		semVersao := 0
		ops := make([]api.IndexEntry, 0, len(req.VersionMap))
		for i, e := range req.VersionMap {
			// Os tetos do caminho de lotes valem no mapa inline: entrada que o
			// catálogo não guarda recusa o mapa inteiro, e a sessão segue aberta.
			motivo := ""
			switch {
			case len(e.SHA256) != 64 || e.Key == "":
			case !textoGuardavel(e.Key):
				motivo = "key"
			case !textoGuardavel(e.VersionID):
				motivo = "version_id"
			case !dataGuardavel(e.ModifiedAt):
				motivo = "modified_at"
			}
			if motivo != "" {
				responder(w, 400, map[string]any{"error": "invalid_version_map", "reason": motivo, "index": i})
				return
			}
			if e.VersionID == "" {
				semVersao++
				continue
			}
			if len(e.SHA256) != 64 {
				continue
			}
			ops = append(ops, api.Upsert(e))
		}
		s.Indexados, s.SemVersao = len(ops), semVersao
		s.Lotes, s.Entradas = 0, 0
		p.fundir(par, s, ops, true, false)
		resposta["files_dropped"] = len(req.VersionMap) - len(ops)
	} else {
		if len(req.VersionMap) > 0 || (req.Index.Mode != "full" && req.Index.Mode != "delta") ||
			req.Index.Batches < 0 || req.Index.EntriesSent < 0 {
			responder(w, 400, map[string]any{"error": "invalid_index"})
			return
		}
		if s.Oferta != "batches" {
			responder(w, 400, map[string]any{"error": "batches_not_offered"})
			return
		}
		ultima := any(nil)
		if p.ultima[par] != "" {
			ultima = p.ultima[par]
		}
		if req.Index.Mode == "delta" && !s.OfertaDelta {
			responder(w, 409, map[string]any{"error": "base_mismatch", "base_session_id": ultima})
			return
		}
		if req.Index.Batches != s.Lotes || req.Index.EntriesSent != s.Entradas {
			responder(w, 409, map[string]any{"error": "index_mismatch", "batches": s.Lotes, "entries": s.Entradas})
			return
		}
		if req.Index.Mode == "delta" {
			base := ""
			if req.Index.BaseSessionID != nil {
				base = *req.Index.BaseSessionID
			}
			if p.baseErrada > 0 || base == "" || base != p.ultima[par] {
				if p.baseErrada > 0 {
					p.baseErrada--
				}
				responder(w, 409, map[string]any{"error": "base_mismatch", "base_session_id": ultima})
				return
			}
		}
		s.Modo = req.Index.Mode
		n := 0
		for _, e := range s.ops {
			if e.Op == api.OpUpsert && e.VersionID != "" {
				n++
			}
		}
		s.Indexados = n
		// O `removed` de sessão parcial só vale em delta e para quem recebeu
		// a oferta `removed_partial`.
		parcial := status == "partial" || s.SemVersao > 0
		p.fundir(par, s, s.ops, req.Index.Mode == "full", parcial && !(req.Index.Mode == "delta" && s.OfertaRemovidosEmParcial))
		resposta["files_dropped"] = s.SemVersao
		resposta["index"] = map[string]any{"mode": req.Index.Mode, "removed_ignored": s.RemovidosIgnorados}
	}
	if s.SemVersao > 0 && status == "complete" {
		// A mesma regra do mapa inline: entrada sem versão deixa a sessão parcial.
		status, codigo = "partial", "missing_version_id"
		mensagem = fmt.Sprintf("%d arquivo(s) sem version_id", s.SemVersao)
	}
	s.ops = nil
	s.Status, s.ErrorCode, s.ErrorMessage = status, codigo, mensagem
	s.Complete = resto
	p.ultima[par] = s.ID
	resposta["status"] = status
	resposta["files_indexed"] = s.Presentes
	if s.Modo == "inline" || p.SemCatalogo {
		resposta["files_indexed"] = s.Indexados
	}
	responder(w, 200, resposta)
}

// recusarInline é o 413 do painel de produção para o índice inline grande: a
// sessão fecha como falha na mesma requisição.
func (p *PainelFalso) recusarInline(w http.ResponseWriter, s *SessaoFalsa, arquivos, bytes int) {
	mensagem := fmt.Sprintf("index_too_large: %d arquivos; limite %d", arquivos, p.MaxInlineArquivos)
	if s.Status == "running" {
		s.Status, s.ErrorCode, s.ErrorMessage = "failed", "index_too_large", mensagem
		s.ops = nil
	}
	responder(w, 413, map[string]any{
		"error": "index_too_large", "message": mensagem, "files": arquivos, "bytes": bytes,
		"limit": map[string]any{"files": p.MaxInlineArquivos, "bytes": p.MaxInlineBytes},
	})
}

// fundir aplica o índice da sessão ao catálogo de presença do agente + plano,
// como o painel de verdade.
//
// full (e o mapa inline): os arquivos presentes passam a ser EXATAMENTE o
// inventário que veio — também na sessão parcial. O arquivo que o agente não
// conseguiu ler nesta sessão fica fora dela; `removed`/`unselected` só dizem o
// motivo. delta: as operações são aplicadas sobre a base; com
// ignorarRemovidos (sessão parcial que não recebeu a oferta `removed_partial`)
// o `removed` não é aplicado (o arquivo segue presente), o `unselected` é.
// Upsert sem versão não cataloga: em delta, o arquivo sai da sessão.
func (p *PainelFalso) fundir(par string, s *SessaoFalsa, ops []api.IndexEntry, full, ignorarRemovidos bool) {
	if p.SemCatalogo {
		return
	}
	// Chave repetida na sessão: vale a última entrada.
	ultimaDaChave := make(map[string]int, len(ops))
	for i, e := range ops {
		ultimaDaChave[e.Key] = i
	}
	cat := p.catalogo[par]
	if cat == nil || full {
		cat = map[string]api.FileEntry{}
	}
	for i, e := range ops {
		if ultimaDaChave[e.Key] != i {
			continue
		}
		switch {
		case e.Op == api.OpUpsert && e.VersionID != "":
			cat[e.Key] = api.FileEntry{Key: e.Key, VersionID: e.VersionID, Size: *e.Size, SHA256: e.SHA256, ModifiedAt: *e.ModifiedAt}
		case e.Op == api.OpUpsert:
			delete(cat, e.Key)
		case full:
		case e.Op == api.OpRemoved && ignorarRemovidos:
			s.RemovidosIgnorados++
		default:
			if _, tem := cat[e.Key]; !tem {
				s.RemocoesSemX++
			}
			delete(cat, e.Key)
		}
	}
	p.catalogo[par] = cat
	s.Presentes = len(cat)
}

func (p *PainelFalso) fail(w http.ResponseWriter, sid string, corpo []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessoes[sid]
	if s == nil {
		responder(w, 404, map[string]any{"error": "session_not_found"})
		return
	}
	var req struct {
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	}
	_ = json.Unmarshal(corpo, &req)
	if s.Status == "running" {
		s.Status, s.ErrorCode, s.ErrorMessage = "failed", req.ErrorCode, req.ErrorMessage
		s.ops = nil
	}
	responder(w, 200, map[string]any{"ok": true})
}
