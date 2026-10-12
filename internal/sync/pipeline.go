package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"log/slog"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/caminho"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/time/rate"
)

// EngineOptions configura uma execução de sync.
type EngineOptions struct {
	S3           *s3.Client
	Bucket       string
	PrefixRoot   string   // "data/<agent-ulid>/"
	HostRoot     string   // raiz do fs (em Docker: /host)
	SourcePaths  []string // paths do plano
	ExcludeGlobs []string
	MaxMbps      int // throttle, somado entre todos os envios da execução

	// Trabalhadores é quantos arquivos são lidos e enviados ao mesmo tempo.
	// Zero: min(4, número de CPUs).
	Trabalhadores int
	// Estado é o estado local do plano nesta sessão. nil: sem atalho — todo
	// arquivo é lido e conferido no bucket, como sempre foi.
	Estado *estado.Execucao
	// RehashTotal desliga o atalho mesmo com estado: relê e confere tudo.
	RehashTotal bool
	// Delta: o índice desta sessão é relativo à sessão anterior. Arquivo
	// inalterado não gera entrada, e o que saiu da origem ou da seleção sai
	// como `removed`/`unselected`. Só vale com Estado e Emitir.
	Delta bool
	// Emitir recebe cada entrada do índice, na ordem em que fica pronta.
	// nil: as entradas se acumulam em Result.VersionMap (o /complete inline).
	Emitir func(api.IndexEntry) error
	// Ignorar são pastas (caminho real) em que a varredura nunca entra: a do
	// estado local.
	Ignorar []string
	// VerificarDias é o ciclo da reconferência rotativa (releitura +
	// HeadObject de cada arquivo que o atalho pularia). Zero: 30.
	VerificarDias int
	// VerificarTeto é quantas reconferências cabem numa execução. Zero: a
	// parte do ciclo que passou desde a última execução confirmada (ver
	// tetoDaReconferencia).
	VerificarTeto int
	// Amostra é quantas entradas do estado são conferidas no bucket antes de
	// qualquer atalho (ver amostra.go). Zero: AmostraPadrao.
	Amostra int
	// Sortear devolve um inteiro em [0, n): o sorteio da amostra. nil: o do
	// sistema; os testes o trocam.
	Sortear func(n int) int
	// Agora é o relógio; os testes o trocam.
	Agora func() time.Time

	banda      *rate.Limiter
	multiparts chan struct{}
	portao     *portaoDoVersionamento
}

// limitador é o limite de banda dos envios: o da execução, quando há; senão
// (processFile chamado direto), um por leitor, como era.
func (o EngineOptions) limitador() *rate.Limiter {
	if o.banda != nil {
		return o.banda
	}
	return novoLimitador(o.MaxMbps)
}

// TrabalhadoresPadrao é quantos arquivos o backup trata ao mesmo tempo quando
// nada é configurado.
func TrabalhadoresPadrao() int { return min(4, runtime.NumCPU()) }

// MultipartsSimultaneos é o teto de envios multipart ao mesmo tempo.
const MultipartsSimultaneos = 2

// margemDoMtimeSuspeito: arquivo modificado a menos disso do início da leitura
// pode mudar de novo sem a data mudar; a entrada não serve de atalho.
const margemDoMtimeSuspeito = 2 * time.Second

// PrefixoDasChaves é o prefixo, já normalizado, de todas as chaves que o
// engine monta a partir de prefixRoot — o que o estado local usa para
// reconhecer as chaves de um plano.
func PrefixoDasChaves(prefixRoot string) string {
	p := path.Join(prefixRoot, "x")
	return p[:len(p)-1]
}

// Result é o retorno do Run.
type Result struct {
	Stats api.SessionStats
	// VersionMap só é preenchido sem EngineOptions.Emitir (o /complete
	// inline), na ordem da varredura.
	VersionMap  []api.FileEntry
	FilesFailed int // arquivos que falharam no upload (pulados); usado p/ decidir partial/failed
	// PrimeiraFalha é o primeiro arquivo que falhou e por quê — exemplo que
	// vai ao painel numa sessão parcial.
	PrimeiraFalha string
	// CloudOnlySkipped conta os arquivos do OneDrive que ficaram de fora por
	// estarem só na nuvem (lê-los baixaria o OneDrive inteiro). Vai ao painel
	// no /complete: a sessão não é um inventário completo da origem, e a
	// falta desses arquivos não pode virar "removido na origem".
	CloudOnlySkipped int
	// ReparseSkipped conta os reparse points que não são do OneDrive (Azure
	// File Sync em camada fria, HSM) ou que não deu para ler: ficam de fora e
	// vão ao painel no /complete pelo mesmo motivo do CloudOnlySkipped.
	ReparseSkipped int
	// NaoPermitido: alguma leitura (varredura ou arquivo) voltou EPERM. No
	// macOS é o TCC: o serviço precisa de Acesso Total ao Disco, e a causa da
	// sessão parcial diz isso.
	NaoPermitido bool

	// Contagens do índice (só fazem sentido com Estado).
	FilesNew        int64 // arquivos que o estado não conhecia
	FilesChanged    int64 // conteúdo diferente do registrado
	FilesRemoved    int64 // saíram da origem
	FilesUnselected int64 // saíram da seleção do plano
	// ChangedSameMtime: conteúdo diferente com tamanho e datas iguais — só a
	// reconferência (rotativa ou total) pega. Sinal de adulteração ou de
	// programa que repõe a data.
	ChangedSameMtime int64
	// Atalhos é quantos arquivos não foram lidos (estado conferiu);
	// Verificados, quantos o atalho pularia e a reconferência rotativa releu.
	Atalhos     int64
	Verificados int64
	// Amostrados é quantas entradas do estado foram conferidas no bucket no
	// começo da execução; AmostraFalhou, por que a amostra não conferiu (a
	// execução inteira leu e conferiu tudo). Vazio: conferiu.
	Amostrados    int
	AmostraFalhou string
	// InventarioCompleto: a varredura terminou sem erro, sem arquivo pulado e
	// sem falha de envio — a condição para emitir remoções.
	InventarioCompleto bool
	// EstadoFalhou: o estado local deixou de servir no meio (corrupção, disco
	// cheio). O backup seguiu sem ele; a sessão não deve confirmá-lo.
	EstadoFalhou bool
}

// Guardados é quantos arquivos esta sessão deixou guardados: enviados agora,
// já no bucket, ou conferidos pelo estado.
func (r *Result) Guardados() int64 {
	if r == nil {
		return 0
	}
	return max(r.Stats.FilesTotal, int64(len(r.VersionMap)))
}

// pisoDaReconferencia é o mínimo de reconferências rotativas que cabem numa
// execução, qualquer que seja o tamanho do plano.
var pisoDaReconferencia int64 = 1000

// tetoDaReconferencia é quantas reconferências rotativas cabem numa execução:
// a fração do ciclo (dias) que passou desde a última execução confirmada, com
// 10% de folga. Num plano diário é 1/30 dos arquivos; num semanal, 7/30 — o
// que vence entre uma execução e outra cabe, e o ciclo completo fecha em
// ~dias, qualquer que seja a frequência do plano.
//
// Antes era 3,5% fixos por execução: o plano semanal via vencer 23% e conferia
// 3,5% — um ciclo de 29 semanas, não de 30 dias.
//
// Sem data da última execução (primeira vez, estado de antes de a data
// existir) ou com o relógio andando para trás, vale um dia.
func tetoDaReconferencia(arquivos int64, ultima, agora time.Time, dias int) int64 {
	ciclo := time.Duration(dias) * 24 * time.Hour
	decorrido := 24 * time.Hour
	if !ultima.IsZero() {
		if d := agora.Sub(ultima); d > 0 {
			decorrido = min(d, ciclo)
		}
	}
	teto := int64(float64(arquivos) * 1.1 * float64(decorrido) / float64(ciclo))
	return max(pisoDaReconferencia, teto+1)
}

// blocoDoEstado é de quantas em quantas entradas o estado local é gravado.
var blocoDoEstado = 2048

// EntradaDoEstado monta, a partir do estado local, a entrada do índice de um
// arquivo presente — a mesma que o atalho emite.
func EntradaDoEstado(prefixo, rel string, ent estado.Entrada) api.FileEntry {
	return api.FileEntry{
		Key:        prefixo + rel,
		VersionID:  ent.Versao,
		Size:       ent.Tamanho,
		SHA256:     hex.EncodeToString(ent.SHA256[:]),
		ModifiedAt: dataParaOPainel(time.Unix(ent.MtimeSeg, int64(ent.MtimeNano))),
	}
}

// ErrIndice: uma entrada do índice não pôde ser entregue a EngineOptions.Emitir.
var ErrIndice = errors.New("não consegui registrar o índice da sessão")

type itemDoWalk struct {
	seq uint64
	fi  FileInfo
}

// tratado é o resultado de um arquivo, do trabalhador para o coletor.
type tratado struct {
	seq uint64
	fi  FileInfo
	err error

	entrada  api.FileEntry // presente na sessão com esta versão
	emitir   bool          // entra no índice desta sessão
	enviado  bool          // subiu agora (não era dedup nem atalho)
	gravar   *estado.Gravacao
	visto    uint64 // ID do estado a marcar como visto (atalho)
	atalho   bool
	verific  bool
	novo     bool
	alterado bool
	mesmaDat bool
}

// Run executa o sync de um plano.
//
// A varredura corre em fluxo; N trabalhadores tratam os arquivos (atalho pelo
// estado local, ou hash + HeadObject + envio) e um coletor, sozinho, soma as
// estatísticas, grava o estado em blocos e entrega as entradas do índice. A
// memória não cresce com o número de arquivos quando há Emitir.
func Run(ctx context.Context, o EngineOptions) (*Result, error) {
	// Cancelado na saída: um retorno antecipado (bucket sem versionamento)
	// não pode deixar o walker bloqueado no canal.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if o.Agora == nil {
		o.Agora = time.Now
	}
	if o.VerificarDias <= 0 {
		o.VerificarDias = 30
	}
	if o.Emitir == nil || o.Estado == nil {
		o.Delta = false
	}
	o.banda = novoLimitador(o.MaxMbps)
	o.multiparts = make(chan struct{}, MultipartsSimultaneos)
	o.portao = &portaoDoVersionamento{}
	n := o.Trabalhadores
	if n <= 0 {
		n = TrabalhadoresPadrao()
	}

	// Antes de qualquer atalho: o bucket ainda é o que o estado descreve?
	result0 := &Result{}
	if o.Estado != nil && !o.RehashTotal {
		result0.Amostrados, result0.AmostraFalhou = conferirAmostra(ctx, o)
		if result0.AmostraFalhou != "" {
			avisarAmostra(result0.Amostrados, result0.AmostraFalhou)
			o.RehashTotal = true
		}
	}

	c := &coletor{o: o, result: result0, cancelar: cancel}
	if o.Estado != nil {
		agora := o.Agora()
		teto := int64(o.VerificarTeto)
		if teto <= 0 {
			teto = tetoDaReconferencia(o.Estado.Arquivos(), o.Estado.UltimaConfirmadaEm(), agora, o.VerificarDias)
		}
		c.orcamento.Store(teto)
		o.Estado.MarcarInicio(agora)
	}

	var pulados puladosDoWalk
	fileCh, errCh := walkSem(ctx, o.HostRoot, o.SourcePaths, o.ExcludeGlobs, &pulados, o.Ignorar)

	numerados := make(chan itemDoWalk, 128)
	go func() {
		defer close(numerados)
		var seq uint64
		for fi := range fileCh {
			seq++
			select {
			case numerados <- itemDoWalk{seq: seq, fi: fi}:
			case <-ctx.Done():
				return
			}
		}
	}()

	tratados := make(chan tratado, 256)
	var wg gosync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range numerados {
				if ctx.Err() != nil {
					return
				}
				tratados <- c.tratar(ctx, it)
			}
		}()
	}
	go func() { wg.Wait(); close(tratados) }()

	for t := range tratados {
		c.receber(ctx, t)
	}
	c.descarregarEstado()
	result := c.result

	// O walker pode estar bloqueado no canal se os trabalhadores saíram por
	// cancelamento: o cancel o solta.
	if c.fatal != nil {
		cancel()
	}
	walkErr := <-errCh
	// A contagem do walker é gravada antes de o canal fechar: lê-se com ele
	// fechado.
	for range errCh {
	}
	result.CloudOnlySkipped = pulados.SoNaNuvem
	result.ReparseSkipped = pulados.Reparse
	result.NaoPermitido = result.NaoPermitido || pulados.NaoPermitido
	c.ordenarMapa()
	if c.fatal != nil {
		return result, c.fatal
	}
	// Cancelado no meio da varredura, o walker fecha o canal sem erro: sem
	// esta checagem, a sessão saía "complete" com só parte dos arquivos.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.InventarioCompleto = walkErr == nil && result.FilesFailed == 0 &&
		pulados.SoNaNuvem == 0 && pulados.Reparse == 0
	if result.InventarioCompleto {
		if err := c.fecharInventario(); err != nil {
			return result, err
		}
	}
	if walkErr != nil {
		return result, walkErr
	}
	return result, nil
}

// coletor reúne o que os trabalhadores produzem. tratar roda nos
// trabalhadores; o resto, só na goroutine do Run.
type coletor struct {
	o        EngineOptions
	result   *Result
	cancelar context.CancelFunc
	fatal    error

	orcamento  atomic.Int64 // reconferências rotativas que ainda cabem
	estadoRuim atomic.Bool  // o estado deixou de servir nesta execução

	seqs     []uint64 // ordem da varredura de cada entrada do VersionMap
	pendente []estado.Gravacao
}

func (c *coletor) estado() *estado.Execucao {
	if c.o.Estado == nil || c.estadoRuim.Load() {
		return nil
	}
	return c.o.Estado
}

func (c *coletor) estadoFalhou(onde string, err error) {
	if c.estadoRuim.CompareAndSwap(false, true) {
		slog.Warn("estado local deixou de servir; o backup segue sem ele (lê e confere tudo)", "onde", onde, "err", err)
	}
}

// tratar decide o que fazer com um arquivo: atalho, ou leitura + conferência
// no bucket + envio.
func (c *coletor) tratar(ctx context.Context, it itemDoWalk) tratado {
	o, fi := c.o, it.fi
	t := tratado{seq: it.seq, fi: fi}
	chave := path.Join(o.PrefixRoot, fi.RelativePath)
	rel := strings.TrimPrefix(chave, PrefixoDasChaves(o.PrefixRoot))

	var ent estado.Entrada
	tem, vale := false, false
	ex := c.estado()
	if ex != nil {
		var err error
		ent, tem, err = ex.Buscar(rel)
		if err != nil {
			c.estadoFalhou("leitura", err)
			ex, tem = nil, false
		}
		vale = tem && ex.Vale(ent) && !ent.Removida
	}
	agora := o.Agora()
	hoje := estado.Dia(agora)
	mesmasDatas := tem && ent.Tamanho == fi.Size &&
		ent.MtimeSeg == fi.ModTime.Unix() && ent.MtimeNano == uint32(fi.ModTime.Nanosecond()) &&
		ent.CtimeSeg == fi.CtimeSeg && ent.CtimeNano == fi.CtimeNano
	igual := vale && !ent.Suspeita && mesmasDatas

	if igual && !o.RehashTotal {
		// Reconferência rotativa: no dia marcado, o arquivo é relido e o
		// objeto conferido, enquanto couber no teto da execução.
		devido := ent.VerificarEm <= hoje && c.orcamento.Add(-1) >= 0
		if !devido {
			t.atalho = true
			t.visto = ent.ID
			t.entrada = api.FileEntry{
				Key:        chave,
				VersionID:  ent.Versao,
				Size:       ent.Tamanho,
				SHA256:     hex.EncodeToString(ent.SHA256[:]),
				ModifiedAt: dataParaOPainel(fi.ModTime),
			}
			t.emitir = !o.Delta
			return t
		}
		t.verific = true
	}

	entry, dedup, err := processFile(ctx, o, fi)
	if err != nil {
		t.err = err
		return t
	}
	t.entrada = *entry
	t.enviado = !dedup

	var sha [32]byte
	if b, derr := hex.DecodeString(entry.SHA256); derr == nil && len(b) == 32 {
		copy(sha[:], b)
	}
	conhecido := tem && ((vale) || (!ex.Vale(ent) && ent.NaBase))
	t.novo = !conhecido
	t.alterado = conhecido && !ent.Removida && ent.SHA256 != sha
	t.mesmaDat = igual && ent.SHA256 != sha
	mudou := !vale || ent.Versao != entry.VersionID || ent.SHA256 != sha || ent.Tamanho != entry.Size
	t.emitir = !o.Delta || mudou || !mesmasDatas

	if ex != nil {
		nova := estado.Entrada{
			NaBase:    conhecido,
			Suspeita:  agora.Sub(fi.ModTime) < margemDoMtimeSuspeito,
			Tamanho:   fi.Size,
			MtimeSeg:  fi.ModTime.Unix(),
			MtimeNano: uint32(fi.ModTime.Nanosecond()),
			CtimeSeg:  fi.CtimeSeg,
			CtimeNano: fi.CtimeNano,
			SHA256:    sha,
			Versao:    entry.VersionID,
		}
		if tem {
			nova.ID = ent.ID
			nova.VerificarEm = hoje + uint32(o.VerificarDias)
		} else {
			// Entrada nova: a primeira reconferência cai num dia espalhado
			// pelo ciclo, senão o primeiro backup inteiro venceria junto.
			h := fnv.New32a()
			h.Write([]byte(rel))
			nova.VerificarEm = hoje + 1 + h.Sum32()%uint32(o.VerificarDias)
		}
		t.gravar = &estado.Gravacao{Chave: rel, Ent: nova}
	}
	return t
}

// receber soma o resultado de um arquivo.
func (c *coletor) receber(ctx context.Context, t tratado) {
	r := c.result
	if c.fatal != nil {
		return
	}
	if t.err != nil {
		if errors.Is(t.err, ErrBucketSemVersionamento) {
			// Não é falha deste arquivo: é do bucket, e vale para todos.
			// Seguir subiria tudo de novo só para nada ser indexado.
			c.fatal = t.err
			c.cancelar()
			return
		}
		if ctx.Err() != nil && errors.Is(t.err, context.Canceled) {
			return // o serviço parando no meio do envio não é falha do arquivo
		}
		slog.Warn("arquivo falhou, pulando", "path", t.fi.RelativePath, "err", t.err)
		r.FilesFailed++
		r.NaoPermitido = r.NaoPermitido || naoPermitido(t.err)
		if r.PrimeiraFalha == "" {
			r.PrimeiraFalha = t.fi.RelativePath + ": " + t.err.Error()
		}
		return
	}
	r.Stats.FilesTotal++
	r.Stats.BytesTotal += t.fi.Size
	if t.enviado {
		r.Stats.FilesUploaded++
		r.Stats.BytesUploaded += t.fi.Size
	}
	switch {
	case t.atalho:
		r.Atalhos++
	case t.verific:
		r.Verificados++
	}
	if t.novo {
		r.FilesNew++
	}
	if t.alterado {
		r.FilesChanged++
	}
	if t.mesmaDat {
		r.ChangedSameMtime++
		slog.Warn("arquivo com conteúdo diferente e as mesmas datas e tamanho; enviado como versão nova", "path", t.fi.RelativePath)
	}

	if c.o.Emitir == nil {
		r.VersionMap = append(r.VersionMap, t.entrada)
		c.seqs = append(c.seqs, t.seq)
	} else if t.emitir {
		if err := c.o.Emitir(api.Upsert(t.entrada)); err != nil {
			c.fatal = errors.Join(ErrIndice, err)
			c.cancelar()
			return
		}
	}

	if ex := c.estado(); ex != nil {
		if t.gravar != nil {
			c.pendente = append(c.pendente, *t.gravar)
			if len(c.pendente) >= blocoDoEstado {
				c.descarregarEstado()
			}
		} else if t.visto != 0 {
			ex.MarcarVisto(t.visto)
		}
	}
}

func (c *coletor) descarregarEstado() {
	if len(c.pendente) == 0 {
		return
	}
	if ex := c.estado(); ex != nil {
		if err := ex.Gravar(c.pendente); err != nil {
			c.estadoFalhou("gravação", err)
		}
	}
	c.pendente = c.pendente[:0]
	c.result.EstadoFalhou = c.estadoRuim.Load()
}

// ordenarMapa deixa o VersionMap na ordem da varredura: com vários
// trabalhadores as entradas chegam fora de ordem, e o /complete inline segue
// igual ao de um trabalhador só.
func (c *coletor) ordenarMapa() {
	c.result.EstadoFalhou = c.estadoRuim.Load()
	if len(c.seqs) < 2 {
		return
	}
	sort.Sort(mapaPorSeq{c.seqs, c.result.VersionMap})
}

type mapaPorSeq struct {
	seqs []uint64
	mapa []api.FileEntry
}

func (m mapaPorSeq) Len() int           { return len(m.seqs) }
func (m mapaPorSeq) Less(i, j int) bool { return m.seqs[i] < m.seqs[j] }
func (m mapaPorSeq) Swap(i, j int) {
	m.seqs[i], m.seqs[j] = m.seqs[j], m.seqs[i]
	m.mapa[i], m.mapa[j] = m.mapa[j], m.mapa[i]
}

// fecharInventario roda depois de uma varredura completa e sem erro: o que o
// estado conhecia e não foi visto saiu da origem (`removed`) ou da seleção do
// plano (`unselected`). Em delta as remoções vão ao índice; sempre, a entrada
// vira uma lápide com a geração desta sessão — só some de vez quando o painel
// confirmar a sessão que levou a remoção.
func (c *coletor) fecharInventario() error {
	ex := c.estado()
	if ex == nil {
		return nil
	}
	sel := novaSelecao(c.o.HostRoot, c.o.SourcePaths, c.o.ExcludeGlobs)
	prefixo := PrefixoDasChaves(c.o.PrefixRoot)
	var falha error
	err := ex.Varrer(func(rel string, ent estado.Entrada, visto bool) (estado.Acao, estado.Entrada) {
		if falha != nil {
			return estado.Manter, ent
		}
		vale := ex.Vale(ent)
		switch {
		case visto && !ent.Removida:
			return estado.Manter, ent
		case ent.Removida && vale:
			return estado.Apagar, ent // remoção já confirmada pelo painel
		case !ent.Removida && !vale && !ent.NaBase:
			return estado.Apagar, ent // nunca chegou a uma sessão confirmada
		}
		ent.Removida = true
		ent.NaBase = true
		ent.ForaDaSelecao = !sel.contem(rel)
		op := api.OpRemoved
		if ent.ForaDaSelecao {
			op = api.OpUnselected
			c.result.FilesUnselected++
		} else {
			c.result.FilesRemoved++
		}
		if c.o.Delta {
			if err := c.o.Emitir(api.IndexEntry{Op: op, Key: prefixo + rel}); err != nil {
				falha = errors.Join(ErrIndice, err)
				return estado.Manter, ent
			}
		}
		return estado.Regravar, ent
	})
	if falha != nil {
		return falha
	}
	if err != nil {
		c.estadoFalhou("varredura final", err)
		c.result.EstadoFalhou = true
		if c.o.Delta {
			// Sem a varredura não se sabe o que saiu: a sessão segue válida
			// (o que foi emitido está certo), só sem as remoções.
			c.result.InventarioCompleto = false
		}
	}
	return nil
}

// selecao responde se uma chave (relativa ao prefixo do plano) está dentro
// das pastas e fora das exclusões do plano — a mesma regra da varredura.
type selecao struct {
	raizes []string // chave de cada pasta do plano, sem barra no fim
	globs  []string
}

func novaSelecao(hostRoot string, sourcePaths, excludeGlobs []string) selecao {
	s := selecao{globs: prepararExclusoes(excludeGlobs)}
	for _, sp := range sourcePaths {
		k := caminho.NaChave(hostRoot, caminho.NoDisco(hostRoot, sp))
		if k == "." {
			k = ""
		}
		s.raizes = append(s.raizes, strings.Trim(k, "/"))
	}
	return s
}

func (s selecao) contem(rel string) bool {
	for _, raiz := range s.raizes {
		dentro := rel
		if raiz != "" {
			var ok bool
			dentro, ok = strings.CutPrefix(rel, raiz+"/")
			if !ok {
				continue
			}
		}
		if !s.excluida(dentro) {
			return true
		}
	}
	return false
}

// excluida aplica as exclusões a cada nível do caminho, como a varredura (que
// nem entra numa pasta excluída).
func (s selecao) excluida(dentro string) bool {
	if len(s.globs) == 0 {
		return false
	}
	sep := "/"
	if caminho.Windows {
		sep = `\`
	}
	p := sep + filepath.FromSlash(dentro)
	if caminho.Windows {
		p = sep + strings.ReplaceAll(dentro, "/", sep)
	}
	for i := 1; i <= len(p); i++ {
		if i == len(p) || p[i] == sep[0] {
			if excluido(caminho.Windows, "", p[:i], s.globs) {
				return true
			}
		}
	}
	return false
}
