// Package estado guarda, no disco do servidor, o que o agente já sabe de cada
// arquivo de um plano: tamanho, datas, sha256 e a versão que está no bucket.
//
// É um acelerador, e só isso. Com ele, o arquivo que não mudou (mesmo tamanho,
// mesma data de modificação, mesmo ctime) não é relido nem conferido no bucket;
// sem ele — arquivo ausente, corrompido, de outro armazenamento —, o backup
// roda como sempre rodou (hash de tudo + HeadObject) e o reconstrói. Nada na
// restauração depende dele, e nada do que ele diz vale antes de o painel ter
// confirmado: toda entrada nasce com a "geração" da sessão que a gravou, e a
// geração só passa a valer com o /complete 200 daquela sessão (Confirmar).
// Um `kill -9` no meio deixa entradas de uma geração que nunca foi
// confirmada, e a execução seguinte as trata como ausentes.
//
// Um arquivo bbolt por agente + armazenamento, com um bucket por plano. A
// chave é a chave do objeto sem o prefixo do plano; o valor é binário e leva
// um CRC próprio, porque o bbolt não confere as páginas de dados.
package estado

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// formato do arquivo. Mudou o valor das entradas → muda o número, e o arquivo
// antigo é descartado (modo completo, reconstrói).
const formato = 1

// ErrCorrompido: o arquivo não pôde ser lido como estado válido. Quem recebe
// para de usá-lo nesta execução e o descarta.
var ErrCorrompido = errors.New("estado local corrompido")

var (
	bucketMeta   = []byte("meta")
	bucketPlanos = []byte("planos")
	chaveID      = []byte("id")
	tabelaCRC    = crc32.MakeTable(crc32.Castagnoli)
)

// Identidade diz de quem é o arquivo de estado. Qualquer campo diferente e o
// arquivo é de outro lugar: é descartado e o backup confere tudo.
type Identidade struct {
	Agente        string `json:"agente"`
	Armazenamento string `json:"armazenamento"`
	Bucket        string `json:"bucket"`
	// Endpoint é o endereço do armazenamento ("" na AWS). O mesmo
	// armazenamento do painel reapontado para outro provedor mantém o id e o
	// nome do bucket, mas as versões registradas não existem lá.
	Endpoint string `json:"endpoint,omitempty"`
}

type cabecalho struct {
	Formato int `json:"formato"`
	Identidade
}

// Base é o arquivo de estado aberto.
type Base struct {
	db      *bolt.DB
	caminho string
	// Motivo diz por que o arquivo começou vazio: "" (já existia e vale),
	// "ausente", "corrompido" ou "outro_armazenamento".
	Motivo string
	morta  atomic.Bool
}

// NomeDoArquivo é o arquivo de estado de um agente + armazenamento.
func NomeDoArquivo(dir string, id Identidade) string {
	return filepath.Join(dir, nomeSeguro(id.Agente)+"_"+nomeSeguro(id.Armazenamento)+".db")
}

func nomeSeguro(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Abrir abre (ou cria) o estado do agente + armazenamento em dir. Arquivo
// truncado, ilegível, de outro formato ou de outro armazenamento é apagado e
// recriado vazio — Motivo diz qual foi o caso. Só devolve erro quando nem o
// arquivo novo pôde ser criado (pasta sem escrita, arquivo em uso por outro
// processo): aí o backup roda sem estado.
func Abrir(dir string, id Identidade) (*Base, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	caminho := NomeDoArquivo(dir, id)
	motivo := ""
	if _, err := os.Stat(caminho); errors.Is(err, os.ErrNotExist) {
		motivo = "ausente"
	}
	b, err := abrirArquivo(caminho, id)
	if err == nil {
		b.Motivo = motivo
		return b, nil
	}
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("estado em uso por outro processo: %w", err)
	}
	motivo = "corrompido"
	if errors.Is(err, errOutro) {
		motivo = "outro_armazenamento"
	}
	if rerr := os.Remove(caminho); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return nil, fmt.Errorf("descartando o estado inválido: %w", rerr)
	}
	b, err = abrirArquivo(caminho, id)
	if err != nil {
		return nil, err
	}
	b.Motivo = motivo
	return b, nil
}

var errOutro = errors.New("estado de outro armazenamento")

func abrirArquivo(caminho string, id Identidade) (b *Base, err error) {
	var db *bolt.DB
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrCorrompido, r)
		}
		if err != nil && db != nil {
			_ = fecharComPrazo(db)
		}
	}()
	antes := debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(antes)

	db, err = bolt.Open(caminho, 0o600, &bolt.Options{
		Timeout: 2 * time.Second,
		// A lista de páginas livres vai gravada no arquivo: sem ela o bbolt
		// a refaz na abertura percorrendo o arquivo inteiro — 250 MB lidos
		// e mapeados a cada backup de 1 milhão de arquivos.
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, err
	}
	// Arquivo cortado: o bbolt mapeia além do fim e só descobriria numa
	// leitura, com uma falha de página. O tamanho lógico (páginas em uso)
	// tem de caber no arquivo.
	fi, err := os.Stat(caminho)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if fi.Size() < tx.Size() {
			return fmt.Errorf("%w: arquivo com %d bytes, estado com %d", ErrCorrompido, fi.Size(), tx.Size())
		}
		meta, err := tx.CreateBucketIfNotExists(bucketMeta)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bucketPlanos); err != nil {
			return err
		}
		quero := cabecalho{Formato: formato, Identidade: id}
		bruto := meta.Get(chaveID)
		if bruto == nil {
			novo, _ := json.Marshal(quero)
			return meta.Put(chaveID, novo)
		}
		var tem cabecalho
		if json.Unmarshal(bruto, &tem) != nil || tem.Formato != formato {
			return fmt.Errorf("%w: cabeçalho ilegível ou de outro formato", ErrCorrompido)
		}
		if tem.Identidade != id {
			return errOutro
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Base{db: db, caminho: caminho}, nil
}

// Caminho é o arquivo do estado.
func (b *Base) Caminho() string { return b.caminho }

// Fechar fecha o arquivo. Se alguma leitura acusou corrupção, apaga-o: a
// execução seguinte começa sem estado (modo completo).
func (b *Base) Fechar() error {
	if b == nil || b.db == nil {
		return nil
	}
	err := b.fecharSemApagar()
	if b.morta.Load() {
		_ = os.Remove(b.caminho)
	}
	return err
}

func (b *Base) fecharSemApagar() error {
	return fecharComPrazo(b.db)
}

// prazoParaFechar é quanto o fechamento do arquivo pode levar.
var prazoParaFechar = 5 * time.Second

// fecharComPrazo fecha o arquivo sem ficar preso nele. Um pânico dentro do
// bbolt (arquivo adulterado ou cortado) pode deixar uma trava interna dele
// tomada, e o Close esperaria por ela para sempre — com o backup e a trava do
// bucket presos junto. Passado o prazo o arquivo é largado aberto: é um
// descritor a mais até o processo sair, e o estado é descartado de qualquer
// forma.
func fecharComPrazo(db *bolt.DB) error {
	feito := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				feito <- fmt.Errorf("%w: %v", ErrCorrompido, r)
			}
		}()
		feito <- db.Close()
	}()
	select {
	case err := <-feito:
		return err
	case <-time.After(prazoParaFechar):
		return fmt.Errorf("%w: o arquivo não fechou em %s", ErrCorrompido, prazoParaFechar)
	}
}

// Corrompida diz se alguma operação acusou corrupção nesta abertura.
func (b *Base) Corrompida() bool { return b.morta.Load() }

// protegido roda fn convertendo pânico e falha de página (arquivo mapeado
// adulterado ou cortado depois de aberto) em ErrCorrompido.
func (b *Base) protegido(fn func() error) (err error) {
	if b.morta.Load() {
		return ErrCorrompido
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrCorrompido, r)
		}
		if errors.Is(err, ErrCorrompido) {
			b.morta.Store(true)
		}
	}()
	antes := debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(antes)
	return fn()
}

// --- entradas -------------------------------------------------------------

// Entrada é o que o estado sabe de um arquivo.
type Entrada struct {
	// ID é denso e nunca muda: é o bit do arquivo no mapa de "vistos".
	ID uint64
	// Ger é a geração (sessão) que gravou a entrada.
	Ger uint32
	// Removida: o arquivo saiu da origem (ou da seleção, com ForaDaSelecao) e
	// o agente já emitiu a remoção na geração Ger. A lápide fica até essa
	// geração ser confirmada; se não for, a remoção é emitida de novo.
	Removida      bool
	ForaDaSelecao bool
	// NaBase: a entrada (ou a que ela substituiu) fazia parte de uma sessão
	// confirmada. É o que diz, para uma entrada de geração não confirmada, se
	// o painel conhece o arquivo — e portanto se a falta dele é `removed`.
	NaBase bool
	// Suspeita: a data de modificação estava a menos de 2 s do início da
	// leitura. Uma alteração logo depois pode ter a mesma data; a entrada
	// não serve de atalho na próxima execução.
	Suspeita bool

	Tamanho   int64
	MtimeSeg  int64
	MtimeNano uint32
	CtimeSeg  int64
	CtimeNano uint32
	// VerificarEm é o dia (dias desde 1970, UTC) da próxima reconferência
	// rotativa: releitura do arquivo + HeadObject.
	VerificarEm uint32
	SHA256      [32]byte
	Versao      string
}

const (
	bitRemovida = 1 << iota
	bitForaDaSelecao
	bitNaBase
	bitSuspeita
)

func codificar(chave []byte, e Entrada) []byte {
	v := make([]byte, 4, 4+1+6*binary.MaxVarintLen64+32+len(e.Versao))
	var f byte
	if e.Removida {
		f |= bitRemovida
	}
	if e.ForaDaSelecao {
		f |= bitForaDaSelecao
	}
	if e.NaBase {
		f |= bitNaBase
	}
	if e.Suspeita {
		f |= bitSuspeita
	}
	v = append(v, f)
	v = binary.AppendUvarint(v, e.ID)
	v = binary.AppendUvarint(v, uint64(e.Ger))
	v = binary.AppendVarint(v, e.Tamanho)
	v = binary.AppendVarint(v, e.MtimeSeg)
	v = binary.AppendUvarint(v, uint64(e.MtimeNano))
	v = binary.AppendVarint(v, e.CtimeSeg)
	v = binary.AppendUvarint(v, uint64(e.CtimeNano))
	v = binary.AppendUvarint(v, uint64(e.VerificarEm))
	v = append(v, e.SHA256[:]...)
	v = append(v, e.Versao...)
	h := crc32.New(tabelaCRC)
	h.Write(chave)
	h.Write(v[4:])
	binary.LittleEndian.PutUint32(v[:4], h.Sum32())
	return v
}

func decodificar(chave, v []byte) (Entrada, error) {
	var e Entrada
	if len(v) < 5+32 {
		return e, fmt.Errorf("%w: entrada curta", ErrCorrompido)
	}
	h := crc32.New(tabelaCRC)
	h.Write(chave)
	h.Write(v[4:])
	if binary.LittleEndian.Uint32(v[:4]) != h.Sum32() {
		return e, fmt.Errorf("%w: entrada com CRC errado", ErrCorrompido)
	}
	f := v[4]
	e.Removida = f&bitRemovida != 0
	e.ForaDaSelecao = f&bitForaDaSelecao != 0
	e.NaBase = f&bitNaBase != 0
	e.Suspeita = f&bitSuspeita != 0
	r := v[5:]
	ruim := false
	u := func() uint64 {
		x, n := binary.Uvarint(r)
		if n <= 0 {
			ruim = true
			return 0
		}
		r = r[n:]
		return x
	}
	i := func() int64 {
		x, n := binary.Varint(r)
		if n <= 0 {
			ruim = true
			return 0
		}
		r = r[n:]
		return x
	}
	e.ID = u()
	e.Ger = uint32(u())
	e.Tamanho = i()
	e.MtimeSeg = i()
	e.MtimeNano = uint32(u())
	e.CtimeSeg = i()
	e.CtimeNano = uint32(u())
	e.VerificarEm = uint32(u())
	if ruim || len(r) < 32 {
		return e, fmt.Errorf("%w: entrada truncada", ErrCorrompido)
	}
	copy(e.SHA256[:], r[:32])
	e.Versao = string(r[32:])
	return e, nil
}

// --- planos ---------------------------------------------------------------

// metaDoPlano é o que o estado guarda de um plano além das entradas.
type metaDoPlano struct {
	// Prefixo das chaves do plano ("<prefixo>data/<agente>/"). Mudou → as
	// entradas são de outro lugar do bucket, e o plano recomeça vazio.
	Prefixo string `json:"prefixo"`
	// UltimaSessao é a última sessão que o painel confirmou (/complete 200).
	UltimaSessao string `json:"ultima_sessao,omitempty"`
	// Confirmadas: intervalos [de, até] de gerações confirmadas.
	Confirmadas [][2]uint32 `json:"confirmadas,omitempty"`
	// Ger é a última geração aberta; PendSessao, a sessão dela enquanto não
	// foi confirmada.
	Ger        uint32 `json:"ger"`
	PendSessao string `json:"pend_sessao,omitempty"`
	// Arquivos é quantos arquivos a última execução confirmada viu — a base
	// do teto da reconferência rotativa.
	Arquivos int64 `json:"arquivos,omitempty"`
}

func (m *metaDoPlano) confirmada(g uint32) bool {
	for _, iv := range m.Confirmadas {
		if g >= iv[0] && g <= iv[1] {
			return true
		}
	}
	return false
}

func (m *metaDoPlano) confirmar(g uint32) {
	if g == 0 || m.confirmada(g) {
		return
	}
	m.Confirmadas = append(m.Confirmadas, [2]uint32{g, g})
	sort.Slice(m.Confirmadas, func(i, j int) bool { return m.Confirmadas[i][0] < m.Confirmadas[j][0] })
	junto := m.Confirmadas[:1]
	for _, iv := range m.Confirmadas[1:] {
		ult := &junto[len(junto)-1]
		if iv[0] <= ult[1]+1 {
			ult[1] = max(ult[1], iv[1])
			continue
		}
		junto = append(junto, iv)
	}
	m.Confirmadas = junto
}

// Plano é o estado de um plano dentro da Base.
type Plano struct {
	b    *Base
	id   string
	ent  []byte // nome do bucket das entradas
	meta metaDoPlano
	// Reiniciado: o plano tinha entradas de outro prefixo, descartadas.
	Reiniciado bool
}

// Plano abre o estado do plano, criando-o vazio se não existe ou se o
// prefixo das chaves mudou.
func (b *Base) Plano(id, prefixo string) (*Plano, error) {
	p := &Plano{b: b, id: id, ent: []byte("e/" + id)}
	err := b.protegido(func() error {
		return b.db.Update(func(tx *bolt.Tx) error {
			planos := tx.Bucket(bucketPlanos)
			if bruto := planos.Get([]byte(id)); bruto != nil {
				if json.Unmarshal(bruto, &p.meta) != nil {
					return fmt.Errorf("%w: meta do plano ilegível", ErrCorrompido)
				}
			}
			if p.meta.Prefixo != prefixo {
				if tx.Bucket(p.ent) != nil {
					if err := tx.DeleteBucket(p.ent); err != nil {
						return err
					}
					p.Reiniciado = true
				}
				p.meta = metaDoPlano{Prefixo: prefixo}
			}
			if _, err := tx.CreateBucketIfNotExists(p.ent); err != nil {
				return err
			}
			return p.gravarMeta(tx)
		})
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plano) gravarMeta(tx *bolt.Tx) error {
	bruto, err := json.Marshal(p.meta)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketPlanos).Put([]byte(p.id), bruto)
}

// UltimaSessao é a última sessão deste plano que o painel confirmou.
func (p *Plano) UltimaSessao() string { return p.meta.UltimaSessao }

// Pendente é a geração aberta e ainda não confirmada, com a sessão dela.
func (p *Plano) Pendente() (ger uint32, sessao string) {
	if p.meta.PendSessao == "" {
		return 0, ""
	}
	return p.meta.Ger, p.meta.PendSessao
}

// Arquivos é quantos arquivos a última execução confirmada viu.
func (p *Plano) Arquivos() int64 { return p.meta.Arquivos }

// Vazio diz se o plano não tem nenhuma geração confirmada: nada nele serve
// de atalho.
func (p *Plano) Vazio() bool { return len(p.meta.Confirmadas) == 0 }

// Confirmar registra que o painel aceitou a sessão: as gerações dadas passam
// a valer, e sessao vira a base do próximo delta. arquivos < 0 mantém a
// contagem.
func (p *Plano) Confirmar(sessao string, arquivos int64, gers ...uint32) error {
	return p.b.protegido(func() error {
		return p.b.db.Update(func(tx *bolt.Tx) error {
			for _, g := range gers {
				p.meta.confirmar(g)
			}
			p.meta.UltimaSessao = sessao
			p.meta.PendSessao = ""
			if arquivos >= 0 {
				p.meta.Arquivos = arquivos
			}
			return p.gravarMeta(tx)
		})
	})
}

// Execucao é uma sessão de backup lendo e gravando o estado do plano.
type Execucao struct {
	p *Plano
	// Ger é a geração desta execução.
	Ger        uint32
	adotadas   map[uint32]bool
	seqInicial uint64
	vistos     []atomic.Uint64
	consultas  atomic.Uint64
}

// consultasPorAlivio é de quantas em quantas consultas as páginas mapeadas
// do arquivo são devolvidas ao sistema (ver aliviar).
const consultasPorAlivio = 20000

// Comecar abre uma geração nova para a sessão. adotar são gerações de
// tentativas anteriores deste mesmo processo, que valem como confirmadas
// dentro desta execução.
func (p *Plano) Comecar(sessao string, adotar ...uint32) (*Execucao, error) {
	e := &Execucao{p: p, adotadas: map[uint32]bool{}}
	for _, g := range adotar {
		e.adotadas[g] = true
	}
	err := p.b.protegido(func() error {
		return p.b.db.Update(func(tx *bolt.Tx) error {
			p.meta.Ger++
			p.meta.PendSessao = sessao
			e.Ger = p.meta.Ger
			e.seqInicial = tx.Bucket(p.ent).Sequence()
			return p.gravarMeta(tx)
		})
	})
	if err != nil {
		return nil, err
	}
	e.vistos = make([]atomic.Uint64, e.seqInicial/64+1)
	return e, nil
}

// Arquivos é quantos arquivos a última sessão confirmada do plano viu.
func (e *Execucao) Arquivos() int64 { return e.p.meta.Arquivos }

// Vale diz se a entrada é de uma geração confirmada (ou adotada).
func (e *Execucao) Vale(ent Entrada) bool {
	return e.p.meta.confirmada(ent.Ger) || e.adotadas[ent.Ger]
}

// Falhou diz se o estado deixou de servir nesta execução (corrupção).
func (e *Execucao) Falhou() bool { return e.p.b.morta.Load() }

// Buscar devolve a entrada da chave (relativa ao prefixo do plano).
func (e *Execucao) Buscar(chave string) (ent Entrada, achou bool, err error) {
	if e.consultas.Add(1)%consultasPorAlivio == 0 {
		e.p.b.aliviar()
	}
	err = e.p.b.protegido(func() error {
		return e.p.b.db.View(func(tx *bolt.Tx) error {
			k := []byte(chave)
			v := tx.Bucket(e.p.ent).Get(k)
			if v == nil {
				return nil
			}
			var derr error
			ent, derr = decodificar(k, v)
			achou = derr == nil
			return derr
		})
	})
	return ent, achou && err == nil, err
}

// MarcarVisto registra que o arquivo da entrada foi visto e está guardado
// nesta sessão.
func (e *Execucao) MarcarVisto(id uint64) {
	if id == 0 || id > e.seqInicial {
		return
	}
	e.vistos[id/64].Or(1 << (id % 64))
}

func (e *Execucao) visto(id uint64) bool {
	if id > e.seqInicial {
		return true // nasceu nesta execução
	}
	return e.vistos[id/64].Load()&(1<<(id%64)) != 0
}

// Gravacao é uma entrada a gravar. Ent.ID zero pede um ID novo.
type Gravacao struct {
	Chave string
	Ent   Entrada
}

// Gravar grava as entradas com a geração desta execução e as marca vistas.
func (e *Execucao) Gravar(itens []Gravacao) error {
	if len(itens) == 0 {
		return nil
	}
	return e.p.b.protegido(func() error {
		return e.p.b.db.Update(func(tx *bolt.Tx) error {
			bk := tx.Bucket(e.p.ent)
			for i := range itens {
				ent := itens[i].Ent
				if ent.ID == 0 {
					id, err := bk.NextSequence()
					if err != nil {
						return err
					}
					ent.ID = id
				}
				ent.Ger = e.Ger
				k := []byte(itens[i].Chave)
				if err := bk.Put(k, codificar(k, ent)); err != nil {
					return err
				}
				e.MarcarVisto(ent.ID)
			}
			return nil
		})
	})
}

// Acao é o que a varredura final faz com uma entrada.
type Acao int

const (
	Manter Acao = iota
	Apagar
	Regravar
)

// Varrer passa por todas as entradas do plano, em ordem de chave. decidir
// recebe a entrada e se o arquivo dela foi visto nesta execução; devolvendo
// Regravar, a entrada devolvida é gravada com a geração desta execução.
// As mudanças são aplicadas em blocos, fora da leitura.
func (e *Execucao) Varrer(decidir func(chave string, ent Entrada, visto bool) (Acao, Entrada)) error {
	const bloco = 4096
	type mudanca struct {
		chave []byte
		acao  Acao
		ent   Entrada
	}
	var de []byte
	for {
		var mudancas []mudanca
		var ultima []byte
		fim := false
		err := e.p.b.protegido(func() error {
			return e.p.b.db.View(func(tx *bolt.Tx) error {
				c := tx.Bucket(e.p.ent).Cursor()
				var k, v []byte
				if de == nil {
					k, v = c.First()
				} else {
					k, v = c.Seek(de)
					if k != nil && bytes.Equal(k, de) {
						k, v = c.Next()
					}
				}
				n := 0
				for ; k != nil; k, v = c.Next() {
					ent, err := decodificar(k, v)
					if err != nil {
						return err
					}
					acao, nova := decidir(string(k), ent, e.visto(ent.ID))
					if acao != Manter {
						mudancas = append(mudancas, mudanca{chave: bytes.Clone(k), acao: acao, ent: nova})
					}
					n++
					if n >= bloco {
						ultima = bytes.Clone(k)
						return nil
					}
				}
				fim = true
				return nil
			})
		})
		if err != nil {
			return err
		}
		if len(mudancas) > 0 {
			err := e.p.b.protegido(func() error {
				return e.p.b.db.Update(func(tx *bolt.Tx) error {
					bk := tx.Bucket(e.p.ent)
					for _, m := range mudancas {
						if m.acao == Apagar {
							if err := bk.Delete(m.chave); err != nil {
								return err
							}
							continue
						}
						m.ent.Ger = e.Ger
						if err := bk.Put(m.chave, codificar(m.chave, m.ent)); err != nil {
							return err
						}
					}
					return nil
				})
			})
			if err != nil {
				return err
			}
		}
		e.p.b.aliviar()
		if fim {
			return nil
		}
		de = ultima
	}
}

// VersaoApagada é uma versão que saiu do bucket (expurgo).
type VersaoApagada struct {
	Chave  string // chave completa no bucket
	Versao string
}

// Esquecer apaga do estado as entradas cuja versão registrada saiu do bucket:
// sem isso, o atalho seguiria afirmando uma versão que não existe mais. O
// arquivo, se ainda estiver na origem, volta a ser lido e enviado no próximo
// backup. Devolve quantas entradas saíram.
func (b *Base) Esquecer(versoes []VersaoApagada) (int, error) {
	n := 0
	err := b.protegido(func() error {
		return b.db.Update(func(tx *bolt.Tx) error {
			planos := tx.Bucket(bucketPlanos)
			return planos.ForEach(func(id, bruto []byte) error {
				var m metaDoPlano
				if json.Unmarshal(bruto, &m) != nil {
					return fmt.Errorf("%w: meta do plano ilegível", ErrCorrompido)
				}
				bk := tx.Bucket([]byte("e/" + string(id)))
				if bk == nil {
					return nil
				}
				for _, v := range versoes {
					rel, ok := strings.CutPrefix(v.Chave, m.Prefixo)
					if !ok {
						continue
					}
					k := []byte(rel)
					bruto := bk.Get(k)
					if bruto == nil {
						continue
					}
					ent, err := decodificar(k, bruto)
					if err != nil {
						return err
					}
					if ent.Versao != v.Versao || ent.Removida {
						continue
					}
					if err := bk.Delete(k); err != nil {
						return err
					}
					n++
				}
				return nil
			})
		})
	})
	return n, err
}

// Dia é o dia (dias desde 1970, UTC) de um instante — a unidade da
// reconferência rotativa.
func Dia(t time.Time) uint32 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint32(s / 86400)
}
