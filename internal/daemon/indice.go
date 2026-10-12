package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/segredo"
)

// Limites de um lote quando o painel não os diz (os do contrato).
const (
	maxEntradasPadrao = 5000
	maxBytesPadrao    = 2 << 20
	// folgaDoLote fica fora da conta do corpo: `{"seq":N,"entries":[` … `]}`.
	folgaDoLote = 64
)

// errIndiceRecusado: o painel recusou o índice desta sessão de um jeito que
// reenviar não resolve (seq_conflict, sessão fechada, contagem que não fecha,
// base errada). Os arquivos subiram; quem recebe refaz o índice numa sessão
// nova, a partir do estado local.
var errIndiceRecusado = errors.New("o painel recusou o índice da sessão")

// errIndiceGrandeDemais: o painel recusou de vez, por tamanho — a sessão
// passou do teto de entradas (413 `index_too_large`, e o painel a fecha como
// falha na mesma resposta), ou uma única entrada não cabe num lote. Refazer
// em outra sessão daria no mesmo: o backup falha com o motivo, como no 413 do
// /complete inline.
var errIndiceGrandeDemais = errors.New("o índice não cabe no painel")

// recuoMaximoDoLote é o teto do recuo entre tentativas de um lote enquanto o
// backup ainda roda.
var recuoMaximoDoLote = 30 * time.Second

// enviadorDeLotes manda o índice de uma sessão ao painel em lotes
// (POST …/sessions/{sid}/files), enquanto o backup roda.
//
// Cada lote fechado vai primeiro para um arquivo temporário no disco, e é de
// lá que o envio lê: a memória fica no lote em montagem, o painel fora do ar
// não para o backup (os lotes esperam no disco), e um lote que o painel
// perdeu pode ser mandado de novo, igual — a rota é idempotente por
// (sessão, seq) só para o mesmo conteúdo.
type enviadorDeLotes struct {
	c           *api.Client
	rota        string
	maxEntradas int
	maxBytes    int

	// lote em montagem (só a goroutine do engine mexe)
	atual  bytes.Buffer
	nAtual int

	mu       sync.Mutex
	acordar  *sync.Cond
	arquivo  *os.File
	fim      int64         // tamanho gravado do arquivo
	lotes    []loteNoDisco // lotes[i] é o seq i+1
	enviados int           // quantos seq o painel confirmou
	entradas int64         // entradas em todos os lotes fechados
	fechando bool          // não vem mais lote: as tentativas passam a ter fim
	erro     error         // falha definitiva (do painel ou do disco)
	parado   chan struct{}
	aviso    chan struct{}
	cancelar context.CancelFunc
}

type loteNoDisco struct {
	pos, tam int64
	entradas int
}

// novoEnviadorDeLotes cria o enviador e põe o envio para rodar. dir é onde
// fica o arquivo temporário (a pasta do estado; vazio: a temporária do
// sistema).
func novoEnviadorDeLotes(ctx context.Context, c *api.Client, rota string, oferta *api.IndexOffer, dir string) (*enviadorDeLotes, error) {
	e := &enviadorDeLotes{c: c, rota: rota, maxEntradas: maxEntradasPadrao, maxBytes: maxBytesPadrao, parado: make(chan struct{}), aviso: make(chan struct{}, 1)}
	if oferta != nil && oferta.MaxEntries > 0 {
		e.maxEntradas = oferta.MaxEntries
	}
	if oferta != nil && oferta.MaxBytes > 0 {
		e.maxBytes = oferta.MaxBytes
	}
	e.acordar = sync.NewCond(&e.mu)
	// dir é a pasta do estado que o backup já preparou (segredo.PastaPrivada).
	// Ela não é criada aqui: recriada por um MkdirAll, no Windows nasceria com
	// a permissão herdada, aberta a qualquer usuário.
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			dir = ""
		}
	}
	f, err := os.CreateTemp(dir, "indice-*.lotes")
	if err != nil && dir != "" {
		f, err = os.CreateTemp("", "arkame-indice-*.lotes")
	}
	if err != nil {
		return nil, fmt.Errorf("criando o arquivo temporário do índice: %w", err)
	}
	// O arquivo leva o nome de todos os arquivos da sessão. Na pasta do estado
	// ele já herda a proteção; na temporária do sistema (ou numa pasta do
	// estado que já existia, com outra permissão), é protegido aqui.
	if perr := segredo.Proteger(f.Name()); perr != nil {
		nome := f.Name()
		_ = f.Close()
		_ = os.Remove(nome)
		return nil, fmt.Errorf("protegendo o arquivo temporário do índice: %w", perr)
	}
	// Onde o sistema deixa, o arquivo já sai do diretório: um `kill -9` não
	// deixa resto. No Windows ele sai no fechar.
	_ = os.Remove(f.Name())
	e.arquivo = f
	// O envio sobrevive ao cancelamento do backup: o índice do que subiu
	// antes da parada ainda tem de chegar (com o prazo da finalização).
	ectx, cancelar := context.WithCancel(context.WithoutCancel(ctx))
	e.cancelar = cancelar
	go e.enviar(ectx)
	return e, nil
}

// limparRestosDoIndice apaga arquivos temporários de índice que uma execução
// morta deixou em dir (só acontece onde o sistema não deixa apagar arquivo
// aberto).
func limparRestosDoIndice(dir string) {
	if dir == "" {
		return
	}
	restos, _ := filepath.Glob(filepath.Join(dir, "indice-*.lotes"))
	for _, r := range restos {
		if st, err := os.Stat(r); err == nil && time.Since(st.ModTime()) > time.Hour {
			_ = os.Remove(r)
		}
	}
}

// Emitir acrescenta uma entrada ao índice da sessão. É o EngineOptions.Emitir.
func (e *enviadorDeLotes) Emitir(ent api.IndexEntry) error {
	bruto, err := json.Marshal(ent)
	if err != nil {
		return err
	}
	if e.nAtual > 0 && (e.nAtual >= e.maxEntradas || e.atual.Len()+len(bruto)+1+folgaDoLote > e.maxBytes) {
		if err := e.fecharLote(); err != nil {
			return err
		}
	}
	if e.nAtual > 0 {
		e.atual.WriteByte(',')
	}
	e.atual.Write(bruto)
	e.nAtual++
	return nil
}

// fecharLote grava o lote em montagem no disco e o entrega ao envio.
func (e *enviadorDeLotes) fecharLote() error {
	if e.nAtual == 0 {
		return nil
	}
	err := e.guardar(e.atual.Bytes(), e.nAtual, -1)
	e.atual.Reset()
	e.nAtual = 0
	return err
}

// guardar grava um lote (as entradas, sem o envelope) no fim do arquivo. em
// < 0 acrescenta; senão põe no lugar do lote de índice em (a metade de um
// lote que o painel achou grande).
func (e *enviadorDeLotes) guardar(entradas []byte, n int, em int) error {
	var z bytes.Buffer
	w, _ := gzip.NewWriterLevel(&z, gzip.BestSpeed)
	_, _ = w.Write(entradas)
	_ = w.Close()

	e.mu.Lock()
	defer e.mu.Unlock()
	if errors.Is(e.erro, errIndiceRecusado) {
		// O painel já recusou este índice: o backup segue (os arquivos sobem
		// e o estado local é gravado) e o índice é refeito numa sessão nova.
		return nil
	}
	if e.erro != nil {
		return e.erro
	}
	if _, err := e.arquivo.WriteAt(z.Bytes(), e.fim); err != nil {
		// Sem disco para o índice não há como garantir o reenvio; a sessão
		// não pode fechar afirmando o que não mandou.
		e.erro = fmt.Errorf("gravando o índice no disco: %w", err)
		e.acordar.Broadcast()
		return e.erro
	}
	l := loteNoDisco{pos: e.fim, tam: int64(z.Len()), entradas: n}
	e.fim += l.tam
	if em < 0 {
		e.lotes = append(e.lotes, l)
		e.entradas += int64(n)
	} else {
		e.lotes = append(e.lotes, loteNoDisco{})
		copy(e.lotes[em+1:], e.lotes[em:])
		e.lotes[em] = l
	}
	e.acordar.Broadcast()
	return nil
}

// corpoDoLote lê o lote do disco e monta o corpo do POST.
func (e *enviadorDeLotes) corpoDoLote(l loteNoDisco, seq int) ([]byte, error) {
	z := make([]byte, l.tam)
	if _, err := e.arquivo.ReadAt(z, l.pos); err != nil {
		return nil, err
	}
	r, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	var corpo bytes.Buffer
	fmt.Fprintf(&corpo, `{"seq":%d,"entries":[`, seq)
	if _, err := io.Copy(&corpo, r); err != nil {
		return nil, err
	}
	corpo.WriteString("]}")
	return corpo.Bytes(), nil
}

// enviar é a goroutine do envio: manda os lotes em ordem, um por vez.
func (e *enviadorDeLotes) enviar(ctx context.Context) {
	defer close(e.parado)
	tentativa, noFim := 0, 0
	for {
		e.mu.Lock()
		for e.erro == nil && e.enviados >= len(e.lotes) && !e.fechando {
			e.acordar.Wait()
		}
		if e.erro != nil || e.enviados >= len(e.lotes) {
			e.mu.Unlock()
			return
		}
		seq := e.enviados + 1
		l := e.lotes[seq-1]
		fechando := e.fechando
		e.mu.Unlock()

		corpo, err := e.corpoDoLote(l, seq)
		if err != nil {
			e.falhar(fmt.Errorf("lendo o índice do disco: %w", err))
			return
		}
		var resp api.IndexBatchResponse
		err = e.c.POST(ctx, e.rota, json.RawMessage(corpo), &resp)
		if err == nil {
			tentativa = 0
			e.mu.Lock()
			e.enviados = seq
			e.acordar.Broadcast()
			e.mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			e.falhar(err)
			return
		}
		var he *api.HTTPError
		if errors.As(err, &he) && recusaDefinitiva(err) {
			switch {
			case he.Status == 413 && strings.Contains(he.Body, "index_too_large"):
				// O teto é da sessão, não do lote: dividir não resolve.
				e.falhar(fmt.Errorf("%w: lote %d: %v", errIndiceGrandeDemais, seq, err))
				return
			case he.Status == 413 && l.entradas > 1:
				// `batch_too_large` — o lote não coube no painel (limite menor
				// que o anunciado), ou um proxy no caminho o barrou pelo
				// tamanho: vira dois, e a contagem de lotes acompanha.
				if derr := e.dividir(seq-1, corpo); derr != nil {
					e.falhar(derr)
					return
				}
				continue
			case he.Status == 413:
				// Uma entrada só e ainda não cabe: não há o que dividir.
				e.falhar(fmt.Errorf("%w: lote %d, de uma entrada só: %v", errIndiceGrandeDemais, seq, err))
				return
			case he.Status == 409 && strings.Contains(he.Body, "seq_gap"):
				// O painel perdeu lotes já aceitos: manda de novo a partir do
				// que ele espera.
				var g struct {
					Expected int `json:"expected"`
				}
				if json.Unmarshal([]byte(he.Body), &g) == nil && g.Expected >= 1 && g.Expected < seq {
					slog.Warn("o painel perdeu lotes do índice; reenviando", "esperado", g.Expected, "enviando", seq)
					e.mu.Lock()
					e.enviados = g.Expected - 1
					e.mu.Unlock()
					continue
				}
			}
			e.falhar(fmt.Errorf("%w: lote %d: %v", errIndiceRecusado, seq, err))
			return
		}
		// Passageiro (rede, prazo, 5xx, 408, 429): o lote espera no disco.
		// Com o backup ainda rodando, insiste; na finalização, as tentativas
		// têm fim, como as do /complete.
		if fechando {
			noFim++
			if noFim > len(recuosDoComplete) {
				e.falhar(err)
				return
			}
		}
		recuo := recuoMaximoDoLote
		if tentativa < len(recuosDoComplete) {
			recuo = min(recuosDoComplete[tentativa], recuoMaximoDoLote)
		}
		tentativa++
		slog.Warn("lote do índice não chegou ao painel; tentando de novo", "seq", seq, "tentativa", tentativa, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(recuo):
		case <-e.aviso: // a finalização começou: não espera o recuo inteiro
		}
	}
}

func (e *enviadorDeLotes) falhar(err error) {
	e.mu.Lock()
	if e.erro == nil {
		e.erro = err
	}
	e.acordar.Broadcast()
	e.mu.Unlock()
}

// dividir troca o lote de índice i por duas metades.
func (e *enviadorDeLotes) dividir(i int, corpo []byte) error {
	var lote struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(corpo, &lote); err != nil {
		return err
	}
	juntar := func(es []json.RawMessage) []byte {
		var b bytes.Buffer
		for j, x := range es {
			if j > 0 {
				b.WriteByte(',')
			}
			b.Write(x)
		}
		return b.Bytes()
	}
	meio := len(lote.Entries) / 2
	e.mu.Lock()
	e.lotes = append(e.lotes[:i], e.lotes[i+1:]...)
	e.mu.Unlock()
	if err := e.guardar(juntar(lote.Entries[meio:]), len(lote.Entries)-meio, i); err != nil {
		return err
	}
	return e.guardar(juntar(lote.Entries[:meio]), meio, i)
}

// finalizar fecha o último lote e espera o envio de todos. Devolve quantos
// lotes e entradas o painel aceitou — o que o /complete declara.
func (e *enviadorDeLotes) finalizar(ctx context.Context) (lotes int, entradas int64, err error) {
	if ferr := e.fecharLote(); ferr != nil {
		return 0, 0, ferr
	}
	e.mu.Lock()
	e.fechando = true
	e.acordar.Broadcast()
	e.mu.Unlock()
	select {
	case e.aviso <- struct{}{}:
	default:
	}
	select {
	case <-e.parado:
	case <-ctx.Done():
		e.cancelar()
		<-e.parado
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.erro != nil {
		return e.enviados, e.entradas, e.erro
	}
	return len(e.lotes), e.entradas, nil
}

// reenviarDesde manda de novo os lotes a partir de seq > confirmados (o
// painel respondeu index_mismatch dizendo quantos tem) e espera.
func (e *enviadorDeLotes) reenviarDesde(ctx context.Context, confirmados int) error {
	<-e.parado
	e.mu.Lock()
	if e.erro != nil {
		defer e.mu.Unlock()
		return e.erro
	}
	e.enviados = confirmados
	e.parado = make(chan struct{})
	e.mu.Unlock()
	ectx, cancelar := context.WithCancel(context.WithoutCancel(ctx))
	e.cancelar = cancelar
	go e.enviar(ectx)
	select {
	case <-e.parado:
	case <-ctx.Done():
		cancelar()
		<-e.parado
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.erro
}

// recusado diz se o painel já recusou o índice de vez.
func (e *enviadorDeLotes) recusado() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return errors.Is(e.erro, errIndiceRecusado)
}

// fechar encerra o envio e apaga o arquivo temporário.
func (e *enviadorDeLotes) fechar() {
	if e == nil {
		return
	}
	e.cancelar()
	e.falhar(context.Canceled)
	<-e.parado
	nome := e.arquivo.Name()
	_ = e.arquivo.Close()
	_ = os.Remove(nome)
}
