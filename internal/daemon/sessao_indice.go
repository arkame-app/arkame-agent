package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/arkame-app/agent/internal/purge"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// sessaoDeIndice junta, para uma sessão de backup, o estado local do plano e
// o caminho do índice (em lotes ou inline). Tudo nela é opcional: sem estado
// o backup lê e confere tudo; sem `index` na resposta do /start (painel de
// antes do contrato de lotes) o /complete leva o version_map inteiro.
type sessaoDeIndice struct {
	emLotes bool
	delta   bool
	base    string // última sessão concluída, segundo o painel
	prefixo string // prefixo normalizado das chaves do plano
	dir     string // pasta do estado em uso
	planID  string
	// rehashUnico: esta execução relê tudo por causa da flag --rehash-all, que
	// vale para um backup de cada plano (ver rehashFeito).
	rehashUnico bool

	env     *enviadorDeLotes
	arquivo *estado.Base
	plano   *estado.Plano
	ex      *estado.Execucao
}

// pastasDoEstado são, em ordem de preferência, as pastas onde o estado pode
// ficar: a configurada (ou a padrão do sistema) e, se ela não aceita escrita
// (/host montado só para leitura), a da configuração do agente.
func pastasDoEstado(cfg *config.Config) []string {
	if cfg.StateDir == "" {
		return nil
	}
	pastas := []string{cfg.StateDir}
	if cfg.ConfigPath != "" && filepath.IsAbs(cfg.ConfigPath) {
		if alt := filepath.Join(filepath.Dir(cfg.ConfigPath), "estado"); alt != cfg.StateDir {
			pastas = append(pastas, alt)
		}
	}
	return pastas
}

// pastasForaDoBackup são as pastas do estado como a varredura as vê (caminho
// real): nunca entram no backup.
func pastasForaDoBackup(cfg *config.Config) []string {
	var out []string
	for _, p := range pastasDoEstado(cfg) {
		out = append(out, p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			out = append(out, real)
		}
	}
	return out
}

func identidadeDoEstado(cfg *config.Config, bucket string) estado.Identidade {
	return estado.Identidade{Agente: cfg.AgentID, Armazenamento: cfg.StorageID, Bucket: bucket}
}

// abrirSessaoDeIndice prepara o estado local e o envio em lotes da sessão.
// Só devolve erro quando o painel pediu lotes e não há onde guardá-los.
func abrirSessaoDeIndice(ctx context.Context, c *api.Client, cfg *config.Config, plan api.Plan, sessionID string, oferta *api.IndexOffer, prefixRoot string) (*sessaoDeIndice, error) {
	s := &sessaoDeIndice{prefixo: syncengine.PrefixoDasChaves(prefixRoot), planID: plan.ID}
	if oferta != nil && oferta.Mode == "batches" {
		s.emLotes = true
		s.base = oferta.BaseSessionID
	}
	// O estado local (e o atalho que ele dá) só entra em uso quando o painel
	// oferece o índice em lotes. Contra o painel de antes o agente se comporta
	// como a v0.4.31 — lê e confere tudo — e não cria o arquivo: assim o
	// agente novo pode ir a campo antes do painel novo sem mudar nada.
	// ARKAME_STATE=always força o estado também nesse caso.
	if cfg.StorageID != "" && cfg.AgentID != "" && (s.emLotes || cfg.StateAlways) {
		for _, dir := range pastasDoEstado(cfg) {
			b, err := estado.Abrir(dir, identidadeDoEstado(cfg, plan.StorageRef.Bucket))
			if err != nil {
				slog.Warn("não consegui abrir o estado local nesta pasta", "pasta", dir, "err", err)
				continue
			}
			s.arquivo, s.dir = b, dir
			estadoLocalEmUso.Store(true)
			break
		}
	}
	if s.arquivo != nil {
		limparRestosDoIndice(s.dir)
		if m := s.arquivo.Motivo; m != "" {
			slog.Info("estado local começa vazio; este backup lê e confere todos os arquivos", "motivo", m, "arquivo", s.arquivo.Caminho())
		}
		if err := s.abrirPlano(plan.ID, sessionID, oferta); err != nil {
			slog.Warn("estado local do plano indisponível; o backup segue sem ele", "plan_id", plan.ID, "err", err)
			s.plano, s.ex = nil, nil
		}
	}
	if s.emLotes {
		rota := "/api/agents/" + cfg.AgentID + "/sessions/" + sessionID + "/files"
		env, err := novoEnviadorDeLotes(ctx, c, rota, oferta, s.dir)
		if err != nil {
			s.fechar()
			return nil, err
		}
		s.env = env
	}
	return s, nil
}

func (s *sessaoDeIndice) abrirPlano(planID, sessionID string, oferta *api.IndexOffer) error {
	plano, err := s.arquivo.Plano(planID, s.prefixo)
	if err != nil {
		return err
	}
	if plano.Reiniciado {
		slog.Info("o prefixo das chaves do plano mudou; o estado local do plano recomeça", "plan_id", planID)
	}
	// O /complete de uma sessão pode ter chegado e só a resposta se perdido:
	// se o painel a dá como a última concluída, ela está confirmada.
	if g, pend := plano.Pendente(); s.base != "" && pend == s.base {
		if err := plano.Confirmar(s.base, -1, g); err != nil {
			return err
		}
	}
	s.delta = s.emLotes && oferta.Delta && s.base != "" && plano.UltimaSessao() == s.base
	ex, err := plano.Comecar(sessionID)
	if err != nil {
		return err
	}
	s.plano, s.ex = plano, ex
	return nil
}

// configurar põe no engine o estado, o modo do índice e para onde vão as
// entradas.
func (s *sessaoDeIndice) configurar(o *syncengine.EngineOptions, cfg *config.Config) {
	o.Trabalhadores = cfg.UploadConcurrency
	o.RehashTotal = cfg.RehashAll
	if _, feito := rehashFeito.Load(s.planID); cfg.RehashAllOnce && !feito {
		o.RehashTotal, s.rehashUnico = true, true
	}
	o.VerificarDias = cfg.VerifyDays
	o.Ignorar = pastasForaDoBackup(cfg)
	if s.ex != nil {
		o.Estado = s.ex
	}
	if s.env != nil {
		o.Emitir = s.env.Emitir
		o.Delta = s.delta && s.ex != nil
	}
}

// rehashFeito guarda os planos cujo backup com a releitura total pedida pela
// flag `run --rehash-all` já fechou neste processo: a flag vale para UMA
// execução de cada plano. Dada num serviço, ela ficaria na linha de comando
// para sempre, e todo backup releria tudo — para isso existe a variável
// ARKAME_REHASH_ALL, que vale enquanto estiver ligada.
var rehashFeito gosync.Map

// confirmar registra no estado que o painel aceitou a sessão.
func (s *sessaoDeIndice) confirmar(sessionID string, result *syncengine.Result) {
	if s.rehashUnico {
		rehashFeito.Store(s.planID, true)
	}
	if s.plano == nil || s.ex == nil || result == nil || result.EstadoFalhou || s.ex.Falhou() {
		return
	}
	arquivos := int64(-1)
	if result.InventarioCompleto {
		arquivos = result.Stats.FilesTotal
	}
	if err := s.plano.Confirmar(sessionID, arquivos, s.ex.Ger); err != nil {
		slog.Warn("não consegui confirmar a sessão no estado local; o próximo backup confere os arquivos de novo", "err", err)
	}
}

func (s *sessaoDeIndice) fechar() {
	if s == nil {
		return
	}
	s.env.fechar()
	s.env = nil
	if s.arquivo != nil {
		if s.arquivo.Corrompida() {
			slog.Warn("estado local corrompido; descartado (o próximo backup lê e confere tudo)", "arquivo", s.arquivo.Caminho())
		}
		_ = s.arquivo.Fechar()
		s.arquivo = nil
	}
}

// corpoDeConclusao é o corpo do POST /complete. Com o índice em lotes vai
// Index e não vai VersionMap; no caminho inline, o contrário — e o corpo é
// byte a byte o de antes do contrato de lotes.
type corpoDeConclusao struct {
	Status       string           `json:"status"`
	Stats        api.SessionStats `json:"stats"`
	VersionMap   *[]entradaDoMapa `json:"version_map,omitempty"`
	ErrorCode    string           `json:"error_code,omitempty"`
	ErrorMessage string           `json:"error_message,omitempty"`
	// Arquivos do OneDrive só na nuvem que ficaram de fora. Com ele, o
	// painel não trata a sessão como inventário da origem: a falta
	// desses arquivos não é remoção. Vai sempre, mesmo com 0: sem o
	// campo, o painel não separa "nada pulado" de "agente antigo".
	CloudOnlySkipped int `json:"cloud_only_skipped"`
	// Reparse points fora do OneDrive (Azure File Sync em camada fria,
	// HSM) ou ilegíveis que ficaram de fora. Mesma regra: vai sempre.
	ReparseSkipped int               `json:"reparse_skipped"`
	Index          *api.IndexSummary `json:"index,omitempty"`
}

// entradaDoMapa é uma entrada do version_map inline.
type entradaDoMapa struct {
	Key        string    `json:"key"`
	VersionID  string    `json:"version_id"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	ModifiedAt time.Time `json:"modified_at"`
}

type respostaDoComplete struct {
	// Ponteiro: ausente (resposta de "not_running", painel antigo) não é
	// zero indexado.
	FilesIndexed *int `json:"files_indexed"`
}

// codigoIndiceRecusado é o error_code da sessão cujo índice o painel recusou
// e que o agente refez em outra.
const codigoIndiceRecusado = "index_rejected"

// concluirEmLotes fecha uma sessão cujo índice foi em lotes: espera os lotes
// chegarem, manda o /complete com a contagem e trata as recusas do contrato.
// reabrir abre outra sessão do mesmo plano (para refazer o índice).
func (s *sessaoDeIndice) concluirEmLotes(ctx context.Context, c *api.Client, cfg *config.Config, sessionID string, corpo *corpoDeConclusao, result *syncengine.Result, reabrir func(context.Context) (string, *api.IndexOffer, error)) error {
	cctx, ccancel := contextoDeFinalizacao(ctx)
	defer ccancel()
	rota := func(sid string) string { return "/api/agents/" + cfg.AgentID + "/sessions/" + sid + "/complete" }

	corpo.Stats.FilesNew, corpo.Stats.FilesChanged = &result.FilesNew, &result.FilesChanged
	corpo.Stats.FilesRemoved, corpo.Stats.FilesUnselected = &result.FilesRemoved, &result.FilesUnselected
	corpo.Stats.ChangedSameMtime = &result.ChangedSameMtime

	modo := "full"
	var base *string
	if s.delta && s.ex != nil {
		modo = "delta"
		base = &s.base
	}
	lotes, entradas, err := s.env.finalizar(cctx)
	motivo := ""
	switch {
	case errors.Is(err, errIndiceRecusado):
		motivo = err.Error()
	case err != nil:
		// Os lotes não chegaram todos (painel fora do ar até o fim das
		// tentativas, ou sem disco para guardá-los) e o /complete nem foi
		// mandado: a sessão não tem como fechar. Vai como falha — se o painel
		// ainda não responde, ele a fecha quando o agente abrir a próxima. O
		// estado local não é confirmado; o próximo backup confere os
		// arquivos desta sessão no bucket e manda o índice, sem reenviá-los.
		slog.Warn("o índice não chegou inteiro ao painel; sessão marcada como falha", "session_id", sessionID, "err", err)
		marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
			ErrorCode:    "complete_failed",
			ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o índice não chegou inteiro ao painel; o próximo backup o reenvia sem reenviar os arquivos: %v", err),
		})
		return fmt.Errorf("session index: %w", err)
	}
	for rodada := 0; motivo == ""; rodada++ {
		corpo.Index = &api.IndexSummary{Mode: modo, BaseSessionID: base, Batches: lotes, EntriesSent: entradas}
		var resp respostaDoComplete
		err = concluirSessao(cctx, c, rota(sessionID), corpo, &resp)
		if err == nil {
			s.confirmar(sessionID, result)
			slog.Info("session concluída",
				"session_id", sessionID, "status", corpo.Status, "indice", modo,
				"files_total", result.Stats.FilesTotal, "files_uploaded", result.Stats.FilesUploaded,
				"lotes", lotes, "entradas", entradas,
				"atalhos", result.Atalhos, "reconferidos", result.Verificados)
			return nil
		}
		var he *api.HTTPError
		switch {
		case errors.As(err, &he) && he.Status == 409 && strings.Contains(he.Body, "index_mismatch"):
			var tem struct {
				Batches int `json:"batches"`
			}
			if rodada < 2 && json.Unmarshal([]byte(he.Body), &tem) == nil && tem.Batches >= 0 && tem.Batches < lotes {
				slog.Warn("a contagem do índice não fechou no painel; reenviando os lotes que faltam", "no_painel", tem.Batches, "enviados", lotes)
				rerr := s.env.reenviarDesde(cctx, tem.Batches)
				if rerr == nil {
					continue
				}
				if !errors.Is(rerr, errIndiceRecusado) {
					return fmt.Errorf("session index: %w", rerr)
				}
			}
			motivo = err.Error()
		case errors.As(err, &he) && he.Status == 409 && strings.Contains(he.Body, "base_mismatch"):
			motivo = err.Error()
		default:
			if recusaDefinitiva(err) && !errors.Is(err, api.ErrGone) {
				marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
					ErrorCode:    "complete_failed",
					ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o painel recusou a conclusão: %v", err),
				})
			} else {
				slog.Warn("/complete sem resposta definitiva; a sessão fica como está no painel", "session_id", sessionID, "err", err)
			}
			return fmt.Errorf("session complete: %w", err)
		}
	}

	// O painel recusou o índice desta sessão. Os arquivos estão no bucket e o
	// estado local sabe quais: a sessão é fechada como falha e o índice vai
	// inteiro numa sessão nova, sem reler arquivo nem repetir os comandos do
	// plano.
	if s.ex == nil || result.EstadoFalhou || s.ex.Falhou() {
		marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
			ErrorCode:    codigoIndiceRecusado,
			ErrorMessage: "os arquivos subiram, mas o painel recusou o índice da sessão e não há estado local para refazê-lo; o próximo backup o reenvia sem reenviar os arquivos: " + motivo,
		})
		return fmt.Errorf("session index: %s", motivo)
	}
	slog.Warn("o painel recusou o índice da sessão; refazendo em uma sessão nova, com o inventário completo", "session_id", sessionID, "motivo", motivo)
	marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
		ErrorCode:    codigoIndiceRecusado,
		ErrorMessage: "o painel recusou o índice desta sessão; ele foi refeito em outra, com o inventário completo: " + motivo,
	})
	nova, oferta, err := reabrir(cctx)
	if err != nil {
		return fmt.Errorf("session start (refazendo o índice): %w", err)
	}
	if oferta == nil || oferta.Mode != "batches" {
		marcarFalha(ctx, c, cfg, nova, corpoDaFalha{ErrorCode: codigoIndiceRecusado, ErrorMessage: "o painel deixou de oferecer o índice em lotes no meio do backup"})
		return fmt.Errorf("session index: o painel deixou de oferecer o índice em lotes")
	}
	env, err := novoEnviadorDeLotes(cctx, c, "/api/agents/"+cfg.AgentID+"/sessions/"+nova+"/files", oferta, s.dir)
	if err != nil {
		marcarFalha(ctx, c, cfg, nova, corpoDaFalha{ErrorCode: "sync_failed", ErrorMessage: err.Error()})
		return err
	}
	defer env.fechar()
	var eerr error
	verr := s.ex.Varrer(func(rel string, ent estado.Entrada, visto bool) (estado.Acao, estado.Entrada) {
		if eerr == nil && visto && !ent.Removida {
			eerr = env.Emitir(api.Upsert(syncengine.EntradaDoEstado(s.prefixo, rel, ent)))
		}
		return estado.Manter, ent
	})
	if verr == nil {
		verr = eerr
	}
	if verr == nil {
		lotes, entradas, verr = env.finalizar(cctx)
	}
	if verr != nil {
		marcarFalha(ctx, c, cfg, nova, corpoDaFalha{ErrorCode: codigoIndiceRecusado, ErrorMessage: "não consegui refazer o índice: " + verr.Error()})
		return fmt.Errorf("session index (refazendo): %w", verr)
	}
	corpo.Index = &api.IndexSummary{Mode: "full", Batches: lotes, EntriesSent: entradas}
	var resp respostaDoComplete
	if err := concluirSessao(cctx, c, rota(nova), corpo, &resp); err != nil {
		if recusaDefinitiva(err) && !errors.Is(err, api.ErrGone) {
			marcarFalha(ctx, c, cfg, nova, corpoDaFalha{
				ErrorCode:    "complete_failed",
				ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o painel recusou a conclusão: %v", err),
			})
		}
		return fmt.Errorf("session complete (refazendo o índice): %w", err)
	}
	s.confirmar(nova, result)
	slog.Info("session concluída com o índice refeito", "session_id", nova, "sessao_recusada", sessionID,
		"status", corpo.Status, "lotes", lotes, "entradas", entradas)
	return nil
}

// corpoDaFalha é o corpo do POST /fail.
type corpoDaFalha struct {
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// esquecerVersoesApagadas tira do estado local as entradas das versões que o
// expurgo apagou do bucket. Quem chama segura a exclusaoDoBucket: nenhum
// backup está com o estado aberto.
func esquecerVersoesApagadas(cfg *config.Config, apagadas []purge.Version) {
	if len(apagadas) == 0 || cfg.StorageID == "" || cfg.AgentID == "" {
		return
	}
	versoes := make([]estado.VersaoApagada, len(apagadas))
	for i, v := range apagadas {
		versoes[i] = estado.VersaoApagada{Chave: v.Key, Versao: v.VersionID}
	}
	id := identidadeDoEstado(cfg, cfg.StorageBucket)
	for _, dir := range pastasDoEstado(cfg) {
		if _, err := os.Stat(estado.NomeDoArquivo(dir, id)); err != nil {
			continue
		}
		b, err := estado.Abrir(dir, id)
		if err != nil {
			slog.Warn("não consegui abrir o estado local para dar baixa nas versões expurgadas; a reconferência rotativa acerta", "err", err)
			continue
		}
		n, err := b.Esquecer(versoes)
		_ = b.Fechar()
		if err != nil {
			slog.Warn("baixa das versões expurgadas no estado local falhou", "err", err)
			continue
		}
		if n > 0 {
			slog.Info("estado local: entradas de versões expurgadas removidas", "entradas", n)
		}
	}
}
