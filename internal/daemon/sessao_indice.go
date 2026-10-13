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
	"github.com/arkame-app/agent/internal/segredo"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// sessaoDeIndice junta, para uma sessão de backup, o estado local do plano e
// o caminho do índice (em lotes ou inline). Tudo nela é opcional: sem estado
// o backup lê e confere tudo; sem `index` na resposta do /start (painel de
// antes do contrato de lotes) o /complete leva o version_map inteiro.
type sessaoDeIndice struct {
	emLotes bool
	delta   bool
	// removidosEmParcial: o painel ofereceu `removed_partial` — a sessão em
	// delta emite `removed` com prova positiva mesmo fechando parcial.
	removidosEmParcial bool
	base               string // última sessão concluída, segundo o painel
	prefixo            string // prefixo normalizado das chaves do plano
	dir                string // pasta do estado em uso
	planID             string
	// rehashUnico: esta execução relê tudo por causa da flag --rehash-all, que
	// vale para um backup de cada plano (ver rehashFeito).
	rehashUnico bool
	// semConfirmar: o estado não pôde ser alinhado ao que o painel vai ter
	// desta sessão; ela não o confirma.
	semConfirmar bool
	// outraEpoca: a última sessão que o estado tem por confirmada não é a que
	// o painel tem por última concluída. O estado é de outro momento (cópia
	// antiga restaurada, VM clonada, painel que perdeu sessões) e não serve
	// de atalho: esta execução lê e confere tudo.
	outraEpoca bool

	cfg     *config.Config
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
		// A pasta do teste de restauração fica dentro da do estado e já sai
		// com ela; vai também pelo nome, para não depender disso.
		for _, q := range []string{p, filepath.Join(p, pastaDaVerificacao)} {
			out = append(out, q)
			if real, err := filepath.EvalSymlinks(q); err == nil && real != q {
				out = append(out, real)
			}
		}
	}
	return out
}

// ArquivosDoEstado são os arquivos que o estado local deste agente +
// armazenamento pode ter deixado no disco (o estado dos arquivos, a última
// medição de ocupação do bucket e os temporários deles), e as pastas onde
// ficam. É o que o `uninstall` apaga: os nomes dos arquivos do servidor estão
// lá dentro. Os arquivos levam o agente e o armazenamento no nome — os de
// outro agente da mesma máquina, na mesma pasta, não entram.
func ArquivosDoEstado(cfg *config.Config) (arquivos, pastas []string) {
	if cfg.StorageID == "" || cfg.AgentID == "" {
		return nil, nil
	}
	for _, dir := range pastasDoEstado(cfg) {
		uso := arquivoDoUso(cfg, dir)
		arquivos = append(arquivos, arquivoDoEstado(cfg, dir), uso, uso+".tmp")
		pastas = append(pastas, dir)
	}
	return arquivos, pastas
}

// EstadosDoAgente são os arquivos de estado local que ESTE AGENTE deixou no
// disco, de qualquer armazenamento, e as pastas onde estão. É o que o
// `uninstall` procura além dos de ArquivosDoEstado, que só conhece o
// armazenamento da configuração de hoje:
//
//   - o estado de um armazenamento anterior (o agente trocou de bucket) fica
//     no disco com outro nome, e ninguém mais o apagaria;
//   - sem STORAGE_ID na configuração, ArquivosDoEstado não devolve nada;
//   - a pasta pode não ser a da configuração: procura-se também na padrão do
//     sistema, na da configuração do agente e nas que quem chama souber
//     (outrasPastas: a ARKAME_STATE_DIR posta só na definição do serviço).
//
// Só entram arquivos que existem e cujo nome começa pelo id deste agente.
func EstadosDoAgente(cfg *config.Config, outrasPastas ...string) (arquivos, pastas []string) {
	if cfg.AgentID == "" {
		return nil, nil
	}
	candidatas := append(pastasDoEstado(cfg), config.PastaDoEstado(cfg.HostRoot))
	if cfg.ConfigPath != "" && filepath.IsAbs(cfg.ConfigPath) {
		candidatas = append(candidatas, filepath.Join(filepath.Dir(cfg.ConfigPath), "estado"))
	}
	candidatas = append(candidatas, outrasPastas...)
	prefixo := estado.PrefixoDoAgente(cfg.AgentID)
	vistas := map[string]bool{}
	for _, dir := range candidatas {
		if dir == "" || !filepath.IsAbs(dir) || vistas[filepath.Clean(dir)] {
			continue
		}
		vistas[filepath.Clean(dir)] = true
		itens, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		achou := false
		for _, it := range itens {
			n := it.Name()
			if it.IsDir() || !strings.HasPrefix(n, prefixo) {
				continue
			}
			if strings.HasSuffix(n, ".db") || strings.HasSuffix(n, ".uso.json") || strings.HasSuffix(n, ".uso.json.tmp") {
				arquivos = append(arquivos, filepath.Join(dir, n))
				achou = true
			}
		}
		if achou {
			pastas = append(pastas, dir)
		}
	}
	return arquivos, pastas
}

// identidadeDoEstado é de quem é o estado que um backup abre: o agente, o
// armazenamento instalado e o bucket em que o plano grava. Só o backup a usa
// — é ele quem grava o arquivo e quem o descarta se for de outro lugar. O
// nome do arquivo (arquivoDoEstado) leva só o agente e o armazenamento.
func identidadeDoEstado(cfg *config.Config, bucket string) estado.Identidade {
	return estado.Identidade{Agente: cfg.AgentID, Armazenamento: cfg.StorageID, Bucket: bucket,
		Endpoint: strings.TrimRight(cfg.StorageEndpoint, "/")}
}

// arquivoDoEstado é o arquivo de estado deste agente + armazenamento em dir.
func arquivoDoEstado(cfg *config.Config, dir string) string {
	return estado.NomeDoArquivo(dir, estado.Identidade{Agente: cfg.AgentID, Armazenamento: cfg.StorageID})
}

// arquivoDoUso é onde fica, em dir, a última medição de ocupação do bucket.
func arquivoDoUso(cfg *config.Config, dir string) string {
	base := arquivoDoEstado(cfg, dir)
	return base[:len(base)-len(filepath.Ext(base))] + ".uso.json"
}

// pastaPrivada e conferirDono são os do pacote segredo; os testes os trocam
// para simular a pasta deixada pronta por outro usuário.
var (
	pastaPrivada = segredo.PastaPrivada
	conferirDono = segredo.ConferirDono
)

// abrirEstadoEm abre (ou cria) o estado do backup em dir, que tem de ser uma
// pasta só do administrador.
//
// O estado decide o que o backup lê e o que ele afirma ao painel: uma entrada
// forjada faz um arquivo escolhido deixar de ser guardado, ou leva ao catálogo
// uma versão que não é a dele. O CRC de cada entrada pega estrago, não
// adulteração. A defesa é ninguém além do administrador conseguir gravar ali:
// a pasta é criada fechada (no Windows, com a DACL protegida de
// Administradores e SYSTEM — o modo 0700 lá não restringe nada), e uma pasta
// ou um arquivo de estado que já existia em nome de outro usuário não é usado.
// Erro aqui faz o backup tentar a pasta seguinte (a da configuração) e, sem
// nenhuma, rodar sem estado.
func abrirEstadoEm(cfg *config.Config, dir string, id estado.Identidade) (*estado.Base, error) {
	if err := pastaPrivada(dir); err != nil {
		return nil, err
	}
	arq := arquivoDoEstado(cfg, dir)
	if err := conferirDono(arq); errors.Is(err, segredo.ErrDonoNaoConfiavel) {
		// Numa pasta que já existia com permissão larga, o arquivo pode ter
		// sido posto por outro usuário. É derivável: sai, e o backup confere tudo.
		slog.Warn("arquivo de estado local de outro dono; descartado (este backup lê e confere todos os arquivos)", "arquivo", arq, "err", err)
		if rerr := os.Remove(arq); rerr != nil {
			return nil, fmt.Errorf("descartando o estado de outro dono: %w", rerr)
		}
	}
	b, err := estado.Abrir(dir, id)
	if err != nil {
		return nil, err
	}
	// Na pasta criada pelo agente o arquivo já herda a proteção; numa que já
	// existia, é ele que tem de ficar fechado.
	if err := segredo.Proteger(b.Caminho()); err != nil {
		_ = b.Fechar()
		return nil, err
	}
	return b, nil
}

// abrirSessaoDeIndice prepara o estado local e o envio em lotes da sessão.
// Só devolve erro quando o painel pediu lotes e não há onde guardá-los.
func abrirSessaoDeIndice(ctx context.Context, c *api.Client, cfg *config.Config, plan api.Plan, sessionID string, oferta *api.IndexOffer, prefixRoot string) (*sessaoDeIndice, error) {
	s := &sessaoDeIndice{prefixo: syncengine.PrefixoDasChaves(prefixRoot), planID: plan.ID, cfg: cfg}
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
		baixasDoEstado.mu.Lock()
		for _, dir := range pastasDoEstado(cfg) {
			b, err := abrirEstadoEm(cfg, dir, identidadeDoEstado(cfg, plan.StorageRef.Bucket))
			if errors.Is(err, estado.ErrEmUso) {
				// Outro processo está com o estado deste agente + armazenamento:
				// este backup roda sem estado (lê e confere tudo). A segunda
				// pasta é para quando a primeira não aceita escrita — abrir um
				// segundo estado aqui criaria duas versões do mesmo, cada uma
				// envelhecendo por conta própria.
				slog.Warn("estado local em uso por outro processo; este backup roda sem ele (lê e confere tudo)", "pasta", dir, "err", err)
				break
			}
			if err != nil {
				slog.Warn("não consegui abrir o estado local nesta pasta", "pasta", dir, "err", err)
				continue
			}
			s.arquivo, s.dir = b, dir
			if !estadoLocalEmUso.Swap(true) {
				guardarUsoPendente()
			}
			baixasDoEstado.abertos++
			// Versões que saíram do bucket enquanto o arquivo não pôde ser
			// aberto: saem do estado antes de qualquer atalho.
			aplicarBaixasPendentes(cfg, b)
			break
		}
		baixasDoEstado.mu.Unlock()
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
	// se o painel a dá como a última concluída, ela está confirmada — desde
	// que o agente a tenha marcado como pronta antes de mandar o /complete.
	// Sem a marca ele tinha decidido não confirmar (estado que falhou no meio,
	// ou que não pôde ser alinhado ao inventário): adotá-la daria por
	// guardados arquivos que o painel não tem.
	if g, pend := plano.Pendente(); s.base != "" && pend == s.base && plano.PendentePronta() {
		if err := plano.Confirmar(s.base, -1, g); err != nil {
			return err
		}
	}
	// O atalho só vale na época do painel: a última sessão confirmada daqui
	// tem de ser a última concluída de lá. Contra o painel sem lotes
	// (ARKAME_STATE=always) não há base para comparar.
	if s.emLotes && plano.UltimaSessao() != s.base {
		s.outraEpoca = true
		if !plano.Vazio() {
			slog.Warn("o estado local não é da última sessão concluída no painel; este backup lê e confere todos os arquivos",
				"plan_id", planID, "ultima_do_estado", plano.UltimaSessao(), "ultima_do_painel", s.base)
		}
	}
	s.delta = s.emLotes && oferta.Delta && s.base != "" && !s.outraEpoca
	// `removed` em sessão parcial só para o painel que disse que o aplica.
	s.removidosEmParcial = s.delta && oferta.RemovedPartial
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
	o.RehashTotal = cfg.RehashAll || s.outraEpoca
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
		o.RemovidosEmParcial = o.Delta && s.removidosEmParcial
	}
}

// rehashFeito guarda os planos cujo backup com a releitura total pedida pela
// flag `run --rehash-all` já fechou neste processo: a flag vale para UMA
// execução de cada plano. Dada num serviço, ela ficaria na linha de comando
// para sempre, e todo backup releria tudo — para isso existe a variável
// ARKAME_REHASH_ALL, que vale enquanto estiver ligada.
var rehashFeito gosync.Map

// podeConfirmar diz se o estado desta execução serve para ser confirmado: ele
// existe, não falhou no meio e está alinhado ao índice que o painel tem.
func (s *sessaoDeIndice) podeConfirmar(result *syncengine.Result) bool {
	return s.plano != nil && s.ex != nil && result != nil && !result.EstadoFalhou && !s.ex.Falhou() && !s.semConfirmar
}

// marcarPronta registra no estado, antes do /complete de sessionID, que ele
// pode ser confirmado por essa sessão — o que autoriza a execução seguinte a
// dá-la por confirmada se só a resposta se perder. Quando o agente decide não
// confirmar, a marca não é gravada, e a sessão nunca é adotada.
func (s *sessaoDeIndice) marcarPronta(sessionID string, result *syncengine.Result) {
	if !s.podeConfirmar(result) {
		return
	}
	if err := s.plano.MarcarPronta(sessionID); err != nil {
		s.semConfirmar = true
		slog.Warn("não consegui marcar a sessão no estado local; ele não será confirmado (o próximo backup confere tudo)", "err", err)
	}
}

// confirmar registra no estado que o painel aceitou a sessão.
func (s *sessaoDeIndice) confirmar(sessionID string, result *syncengine.Result) {
	if s.rehashUnico {
		rehashFeito.Store(s.planID, true)
	}
	if !s.podeConfirmar(result) {
		return
	}
	// Quantos arquivos o plano tem é a base do teto da reconferência rotativa.
	// Vale também a sessão parcial: num servidor em que toda sessão é parcial
	// (um arquivo ilegível que nunca sai) a contagem ficava em zero para
	// sempre, e o teto, no piso. Parcial só sobe a contagem — ela pode ter
	// visto uma fração da árvore (serviço parando no meio).
	arquivos := result.Stats.FilesTotal
	if !result.InventarioCompleto {
		arquivos = max(arquivos, s.plano.Arquivos())
	}
	if err := s.plano.Confirmar(sessionID, arquivos, s.ex.Ger); err != nil {
		slog.Warn("não consegui confirmar a sessão no estado local; o próximo backup confere os arquivos de novo", "err", err)
	}
}

// soOInventario tira do estado local o que esta execução não viu, quando a
// sessão vai ao painel como inventário (modo `full`, ou o mapa inline) sem a
// varredura ter sido completa — pasta ilegível, arquivo que falhou, serviço
// parando no meio.
//
// No painel, depois de um inventário, os arquivos presentes são EXATAMENTE os
// que vieram: o que não foi lido fica fora da sessão. Se a entrada dele
// seguisse valendo aqui, o próximo backup, em delta, o daria por igual à base
// e não o mandaria — e ele ficaria fora do catálogo, com o backup "completo",
// até mudar de conteúdo. Sem a entrada, ele é lido, conferido no bucket (sem
// reenvio) e volta ao índice. Com a varredura completa não há o que tirar: o
// que não foi visto já virou lápide (fecharInventario).
func (s *sessaoDeIndice) soOInventario(result *syncengine.Result) {
	if s.ex == nil || result == nil || result.InventarioCompleto || result.EstadoFalhou || s.ex.Falhou() {
		return
	}
	tirados := 0
	err := s.ex.Varrer(func(_ string, ent estado.Entrada, visto bool) (estado.Acao, estado.Entrada) {
		if visto {
			return estado.Manter, ent
		}
		tirados++
		return estado.Apagar, ent
	})
	if err != nil {
		s.semConfirmar = true
		slog.Warn("não consegui alinhar o estado local ao inventário desta sessão; ele não será confirmado (o próximo backup confere tudo)", "err", err)
		return
	}
	if tirados > 0 {
		slog.Info("estado local: entradas de arquivos que esta sessão não leu foram descartadas; o próximo backup os confere e os devolve ao índice", "entradas", tirados)
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
		// As baixas que chegaram durante o backup (restauração que não achou
		// a versão) entram agora: o próximo backup confere esses arquivos.
		baixasDoEstado.mu.Lock()
		if !s.arquivo.Corrompida() {
			aplicarBaixasPendentes(s.cfg, s.arquivo)
		}
		baixasDoEstado.abertos--
		_ = s.arquivo.Fechar()
		baixasDoEstado.mu.Unlock()
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
	} else {
		s.soOInventario(result)
	}
	lotes, entradas, err := s.env.finalizar(cctx)
	motivo := ""
	switch {
	case errors.Is(err, errIndiceRecusado):
		motivo = err.Error()
	case errors.Is(err, errLoteInvalido):
		// Recusa definitiva: o painel não aceita uma entrada do jeito que o
		// agente a escreve. Mandar de novo — nesta sessão ou no inventário
		// refeito em outra — levaria a mesma entrada. O backup falha com o
		// motivo (o campo e a posição que o painel devolveu).
		slog.Error("o painel recusou um lote do índice como inválido; é defeito do agente, e repetir não resolve", "session_id", sessionID, "err", err)
		marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
			ErrorCode:    "complete_failed",
			ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o painel recusou o índice da sessão como inválido: %v", err),
		})
		return fmt.Errorf("session index: %w", err)
	case errors.Is(err, errIndiceGrandeDemais):
		// Recusa definitiva, como o 413 do /complete inline: o índice desta
		// sessão não cabe no painel, e mandá-lo de novo (nesta ou em outra
		// sessão) daria no mesmo. O painel já fechou a sessão como falha, com o
		// motivo; o /fail só a alcança se ela ainda estiver aberta.
		slog.Error("o painel recusou o índice da sessão: ele passa do que o painel aceita", "session_id", sessionID, "err", err)
		marcarFalha(ctx, c, cfg, sessionID, corpoDaFalha{
			ErrorCode:    "complete_failed",
			ErrorMessage: fmt.Sprintf("os arquivos subiram, mas o painel recusou a conclusão: %v", err),
		})
		return fmt.Errorf("session index: %w", err)
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
	if motivo == "" {
		s.marcarPronta(sessionID, result)
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

	return s.refazerIndice(ctx, cctx, c, cfg, sessionID, motivo, corpo, result, reabrir)
}

// refazerIndice trata a recusa do índice de uma sessão que reenviar não
// resolve (item 7 do contrato). Os arquivos estão no bucket e o estado local
// sabe quais: a sessão recusada é fechada como falha e o índice vai inteiro
// numa sessão nova, tirado do estado — sem reler arquivo nem repetir os
// comandos do plano. Uma vez só: se a sessão nova também for recusada, o
// backup falha.
//
// ctx é o do backup (o /fail sai mesmo com o serviço parando); cctx, o da
// finalização.
func (s *sessaoDeIndice) refazerIndice(ctx, cctx context.Context, c *api.Client, cfg *config.Config, sessionID, motivo string, corpo *corpoDeConclusao, result *syncengine.Result, reabrir func(context.Context) (string, *api.IndexOffer, error)) error {
	rota := func(sid string) string { return "/api/agents/" + cfg.AgentID + "/sessions/" + sid + "/complete" }
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
	// A sessão nova é um inventário: vale a mesma regra do modo `full`.
	s.soOInventario(result)
	var (
		eerr     error
		lotes    int
		entradas int64
	)
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
	// A geração desta execução vai ao painel pela sessão nova: é ela que a
	// execução seguinte pode receber como base se a resposta se perder.
	s.marcarPronta(nova, result)
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

// baixasDoEstado são as versões que o estado local tem de deixar de afirmar
// (apagadas pelo expurgo, ou que uma restauração não achou no bucket), e a
// conta de quem está com o arquivo de estado aberto. O mutex também põe em
// fila a abertura do arquivo: o bbolt trava o arquivo, e a baixa aberta ao
// mesmo tempo que um backup faria um dos dois desistir dele.
var baixasDoEstado struct {
	mu        gosync.Mutex
	pendentes []estado.VersaoApagada
	abertos   int
}

// tetoDeBaixasPendentes limita a fila de baixas à espera do arquivo de
// estado. O que passar disso fica para a reconferência rotativa.
const tetoDeBaixasPendentes = 100_000

// darBaixaNoEstado tira do estado local as entradas dessas versões, para o
// próximo backup ler o arquivo e conferi-lo (ou enviá-lo) de novo. Com um
// backup em curso — o arquivo de estado está aberto —, a baixa espera e é
// aplicada quando ele fechar.
func darBaixaNoEstado(cfg *config.Config, versoes []estado.VersaoApagada) {
	if len(versoes) == 0 || cfg.StorageID == "" || cfg.AgentID == "" {
		return
	}
	b := &baixasDoEstado
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pendentes)+len(versoes) <= tetoDeBaixasPendentes {
		b.pendentes = append(b.pendentes, versoes...)
	}
	if b.abertos > 0 {
		return
	}
	aplicarBaixasPendentes(cfg, nil)
}

// aplicarBaixasPendentes aplica a fila a TODOS os arquivos de estado deste
// agente + armazenamento: o que o backup tem aberto (aberto, ou nil) e os que
// existirem nas outras pastas do estado. Pode haver mais de um — a pasta
// principal ficou sem escrita por um tempo e o estado foi para a da
// configuração —, e uma baixa que só chegasse ao que está em uso deixaria o
// outro afirmando a versão que saiu do bucket.
//
// Os outros são abertos como estão (estado.AbrirExistente): a baixa não cria
// arquivo, não confere de quem ele é e não o descarta. A fila só é zerada
// quando todos a receberam; arquivo em uso por outro processo a segura até a
// próxima vez. Quem chama segura baixasDoEstado.mu.
func aplicarBaixasPendentes(cfg *config.Config, aberto *estado.Base) {
	b := &baixasDoEstado
	if len(b.pendentes) == 0 {
		return
	}
	ficou := false
	aplicar := func(arq *estado.Base) {
		n, err := arq.Esquecer(b.pendentes)
		if err != nil {
			slog.Warn("baixa de versões no estado local falhou; a reconferência rotativa acerta", "arquivo", arq.Caminho(), "err", err)
		} else if n > 0 {
			slog.Info("estado local: entradas de versões que saíram do bucket invalidadas", "arquivo", arq.Caminho(), "entradas", n)
		}
	}
	if aberto != nil {
		aplicar(aberto)
	}
	for _, dir := range pastasDoEstado(cfg) {
		caminho := arquivoDoEstado(cfg, dir)
		if aberto != nil && caminho == aberto.Caminho() {
			continue
		}
		if errors.Is(conferirDono(caminho), segredo.ErrDonoNaoConfiavel) {
			continue // não é nosso: o backup que passar por ele o descarta
		}
		arq, err := estado.AbrirExistente(caminho)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case errors.Is(err, estado.ErrEmUso):
			slog.Warn("estado local em uso; a baixa de versões que saíram do bucket fica para a próxima vez", "arquivo", caminho)
			ficou = true
			continue
		case err != nil:
			// Ilegível: o backup que o abrir o descarta e confere tudo.
			slog.Warn("não consegui abrir o estado local para dar baixa em versões que saíram do bucket", "arquivo", caminho, "err", err)
			continue
		}
		aplicar(arq)
		_ = arq.Fechar()
	}
	if !ficou {
		b.pendentes = nil
	}
}

// esquecerVersoesApagadas tira do estado local as entradas das versões que o
// expurgo apagou do bucket.
func esquecerVersoesApagadas(cfg *config.Config, apagadas []purge.Version) {
	versoes := make([]estado.VersaoApagada, len(apagadas))
	for i, v := range apagadas {
		versoes[i] = estado.VersaoApagada{Chave: v.Key, Versao: v.VersionID}
	}
	darBaixaNoEstado(cfg, versoes)
}
