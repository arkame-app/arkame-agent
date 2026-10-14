// Package inventario confere o catálogo do painel contra o que o bucket
// lista: o inventário periódico do bucket (contrato, itens 48 a 52).
//
// O painel entrega as versões que o catálogo deste servidor diz existir, em
// páginas, na ordem de bytes da chave — a ordem em que o ListObjectVersions
// devolve o prefixo. O agente anda nas duas listas ao mesmo tempo (uma fusão):
// de cada chave do plano, vê quais versões a listagem trouxe.
//
// O que o pacote garante:
//
//   - memória limitada à janela: uma página do plano (até 10 mil versões),
//     uma página da listagem (até mil) e as listas do relato, que têm teto.
//     Nunca a listagem inteira;
//   - só se afirma "falta" para chave por onde a listagem PASSOU — veio uma
//     chave maior, ou a listagem acabou com resposta boa — e só depois de um
//     HeadObject daquela versão responder 404. Listagem de provedor compatível
//     que saia de ordem, ou atrasada, não vira faltante;
//   - erro que não é resposta definitiva do bucket (5xx, 408, 429, prazo,
//     rede, DNS), permissão negada, listagem fora de ordem, página do plano
//     que não veio: a rodada sai INCOMPLETA, sem lista de faltantes;
//   - a trava do bucket é pedida por janela (Opcoes.Janela) e nunca fica
//     segurada durante uma chamada ao painel (Opcoes.Proxima).
//
// O que cada achado quer dizer para a restauração (versão ausente, tamanho
// diferente, marcador de exclusão) está documentado no painel, em
// `apps/save/src/lib/inventario.ts`. Aqui o agente só relata o que viu.
package inventario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// Os códigos de Interrupted.
const (
	Transitorio   = "transient"
	AcessoNegado  = "access_denied"
	ErroDeLista   = "list_error"
	Ocupado       = "busy"
	Cancelado     = "canceled"
	Prazo         = "deadline"
	ErroDoPlano   = "plan_error"
	ForaDeOrdem   = "unordered"
	Inconsistente = "listing_inconsistent"
)

// Tetos do que um relato carrega (os do painel).
const (
	MaxFaltantes    = 1000
	MaxDivergencias = 200
	MaxAmostra      = 200
	// TetoDoCorpo: o painel aceita 1 MiB; o relato é cortado para caber com folga.
	TetoDoCorpo = 900 * 1024
	// requisicoesPorJanela: quantas requisições ao bucket cabem numa tomada da
	// trava. O backup que chega espera no máximo isso.
	requisicoesPorJanela = 20
	PrazoMinimo          = 60 * time.Second
	PrazoMaximo          = 6 * time.Hour
)

// Entrada é uma linha da listagem de versões: uma versão ou um marcador de
// exclusão.
type Entrada struct {
	Key       string
	VersionID string
	Size      int64
	// Marcador: é um marcador de exclusão (não tem conteúdo).
	Marcador bool
	// Atual: é a entrada mais recente da chave.
	Atual bool
}

// Pagina é uma resposta do ListObjectVersions, com versões e marcadores já
// juntos, em ordem de chave.
type Pagina struct {
	Entradas []Entrada
	// ProximaChave e ProximaVersao continuam a listagem; só valem com Truncada.
	ProximaChave  string
	ProximaVersao string
	Truncada      bool
}

// Cabecalho é o que o HeadObject de uma versão respondeu.
type Cabecalho struct {
	Size         int64
	SHA256       string // o metadado `sha256`
	LockMode     string
	RetainUntil  *time.Time
	LastModified *time.Time
}

// Bucket é o que o inventário pede ao armazenamento. A implementação de
// verdade é BucketS3; os testes usam a mesma, contra um S3 falso.
type Bucket interface {
	// Listar devolve a página da listagem de versões do prefixo que vem depois
	// dos marcadores (vazios: do começo).
	Listar(ctx context.Context, prefixo, depoisDaChave, depoisDaVersao string) (Pagina, error)
	// Cabecalho consulta uma versão. Ausente: erro em que Ausente(err) é verdade.
	Cabecalho(ctx context.Context, chave, versao string) (Cabecalho, error)
	// Versionamento devolve "Enabled", "Suspended" ou "Off".
	Versionamento(ctx context.Context) (string, error)
	// Classificar diz o que um erro deste bucket é: ausente (404 da versão),
	// negado (403), definitivo (outro 4xx) ou do momento.
	Classificar(err error) ClasseDoErro
}

// ClasseDoErro separa a resposta definitiva do bucket do erro do momento.
type ClasseDoErro int

const (
	// ErroDoMomento: 5xx, 408, 429, prazo, rede, DNS. Nunca vira faltante.
	ErroDoMomento ClasseDoErro = iota
	ErroAusente
	ErroNegado
	ErroDefinitivo
)

// Opcoes de uma rodada.
type Opcoes struct {
	Bucket Bucket
	// Prefixo é a pasta do servidor no bucket: o que se lista, e onde toda
	// chave do plano tem de estar.
	Prefixo string
	// Prazo da rodada inteira.
	Prazo time.Duration
	// Amostra: quantos objetos encontrados são conferidos por HeadObject.
	Amostra int
	// Janela roda fn com o bucket fechado para backup e limpeza, e solta em
	// seguida. Devolve false, sem rodar fn, se não conseguiu a vez até o
	// contexto vencer. Nil: roda direto (testes).
	Janela func(ctx context.Context, fn func()) bool
	// Proxima busca no painel a página do plano de um cursor. É chamada SEM a
	// trava do bucket.
	Proxima func(ctx context.Context, cursor string) (*api.InventoryPlanResponse, error)
	// Sorteio é a fonte da amostra; nil: uma nova.
	Sorteio *rand.Rand
}

// Limites aplica os tetos do agente ao prazo e à amostra que o plano pediu.
func Limites(prazoSegundos int64, amostra int) (time.Duration, int) {
	prazo := time.Duration(prazoSegundos) * time.Second
	if prazo < PrazoMinimo {
		prazo = PrazoMinimo
	}
	if prazo > PrazoMaximo {
		prazo = PrazoMaximo
	}
	if amostra < 0 {
		amostra = 0
	}
	if amostra > MaxAmostra {
		amostra = MaxAmostra
	}
	return prazo, amostra
}

// erroInterno carrega o código de Interrupted do que parou a rodada.
type erroInterno struct {
	codigo  string
	detalhe string
}

func (e *erroInterno) Error() string { return e.codigo + ": " + e.detalhe }

func parar(codigo, formato string, args ...any) error {
	return &erroInterno{codigo: codigo, detalhe: fmt.Sprintf(formato, args...)}
}

// motor é o estado da fusão.
type motor struct {
	o Opcoes
	r *api.InventoryResult

	// A listagem: a página corrente e de onde continuar.
	buf        []Entrada
	pos        int
	proxChave  string
	proxVersao string
	comecou    bool
	fimDaLista bool
	// ultimaChave é a última chave consumida da listagem (a ordem é conferida).
	ultimaChave string
	temUltima   bool

	// O plano: os itens da janela e o grupo (mesma chave) em curso.
	itens       []api.InventoryItem
	i           int
	aberto      bool
	fimDoGrupo  int
	doGrupo     map[string]int // versão → índice em itens
	visto       []bool
	tamanho     []int64
	marcador    bool
	ultimoPlano string // a última chave de plano fechada (a ordem do plano é conferida)
	temPlano    bool

	candidatos      []api.InventoryVersion
	candidatosTotal int64
	amostra         []api.InventoryVersion
	oferecidos      int64
	sorteio         *rand.Rand
}

// Rodar faz o inventário a partir da primeira página do plano e devolve o
// relato. interrompida=true: o contexto de quem chamou foi cancelado (o serviço
// está parando) — não há relato a mandar, e o painel reentrega a rodada.
//
// RunID, StartedAt e FinishedAt ficam por conta de quem chama.
func Rodar(ctx context.Context, o Opcoes, primeira *api.InventoryPlanResponse) (relato api.InventoryResult, interrompida bool) {
	m := &motor{o: o, r: &relato, doGrupo: map[string]int{}, sorteio: o.Sorteio}
	if m.sorteio == nil {
		m.sorteio = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x696e76))
	}
	if o.Amostra > MaxAmostra {
		m.o.Amostra = MaxAmostra
	}
	prazo := o.Prazo
	if prazo <= 0 {
		prazo = PrazoMaximo
	}
	rctx, cancelar := context.WithTimeout(ctx, prazo)
	defer cancelar()

	err := m.rodar(rctx, primeira)
	if err == nil {
		relato.Complete = true
		return relato, false
	}
	if ctx.Err() != nil {
		return api.InventoryResult{}, true
	}
	// Incompleta: os totais do que andou ficam; nenhuma lista vai junto — o
	// que a rodada viu até aqui não basta para afirmar faltante.
	codigo, detalhe := Transitorio, err.Error()
	var ei *erroInterno
	switch {
	case errors.As(err, &ei):
		codigo, detalhe = ei.codigo, ei.detalhe
	case rctx.Err() != nil:
		codigo, detalhe = Prazo, "a rodada passou do prazo"
	}
	relato.Complete = false
	relato.Interrupted = codigo
	relato.Detail = cortar(detalhe)
	relato.Missing, relato.MissingTotal = nil, 0
	relato.SizeMismatch, relato.SizeMismatchTotal = nil, 0
	relato.DeleteMarkers, relato.DeleteMarkersTotal = nil, 0
	relato.FoundAgain = nil
	relato.Sample = api.InventorySample{}
	relato.Found, relato.FoundBytes, relato.ExtraVersions, relato.Unconfirmed = 0, 0, 0, 0
	relato.Truncated = false
	return relato, false
}

func (m *motor) rodar(ctx context.Context, primeira *api.InventoryPlanResponse) error {
	if primeira == nil {
		return parar(ErroDoPlano, "sem plano")
	}
	if m.o.Prefixo == "" || !strings.HasSuffix(m.o.Prefixo, "/") {
		// Sem prefixo o inventário listaria o bucket inteiro — os arquivos dos
		// outros servidores inclusive.
		return parar(ErroDoPlano, "o plano veio sem o prefixo do servidor")
	}

	// O versionamento, no começo e no fim: se alguém o suspendeu no meio, a
	// rodada relata. Não saber (sem permissão de leitura) não impede.
	antes := m.versionamento(ctx)

	var pendente []api.InventoryItem
	pagina := primeira
	for {
		m.r.PlanVersions += int64(len(pagina.Items))
		itens := pagina.Items
		if len(pendente) > 0 {
			itens = append(pendente, pagina.Items...)
		}
		haMais := pagina.Next != ""
		pendente = nil
		if haMais && len(itens) > 0 {
			// As versões da última chave podem continuar na página seguinte: o
			// grupo dela espera.
			ultima := itens[len(itens)-1].Key
			corte := len(itens)
			for corte > 0 && itens[corte-1].Key == ultima {
				corte--
			}
			pendente = append([]api.InventoryItem(nil), itens[corte:]...)
			itens = itens[:corte]
		}
		if err := m.fundir(ctx, itens); err != nil {
			return err
		}
		if !haMais {
			break
		}
		if m.o.Proxima == nil {
			return parar(ErroDoPlano, "o plano tem mais páginas e não há como pedi-las")
		}
		// Sem a trava do bucket: a página pode demorar no painel.
		proxima, err := m.o.Proxima(ctx, pagina.Next)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return parar(ErroDoPlano, "página %d do plano: %v", pagina.Page+1, err)
		}
		if proxima == nil || proxima.RunID != pagina.RunID {
			return parar(ErroDoPlano, "página %d do plano: o painel respondeu outra rodada", pagina.Page+1)
		}
		pagina = proxima
	}

	// O plano acabou; o resto da listagem são objetos a mais (não é erro) — e
	// só com ela inteira os totais do prefixo valem.
	if err := m.esgotarListagem(ctx); err != nil {
		return err
	}
	if err := m.confirmarFaltantes(ctx); err != nil {
		return err
	}
	if err := m.conferirAmostra(ctx); err != nil {
		return err
	}
	depois := m.versionamento(ctx)
	m.r.Versioning = piorVersionamento(antes, depois)
	return nil
}

func (m *motor) versionamento(ctx context.Context) string {
	v, err := m.o.Bucket.Versionamento(ctx)
	if err != nil {
		return ""
	}
	return v
}

func piorVersionamento(a, b string) string {
	for _, ruim := range []string{"Off", "Suspended"} {
		if a == ruim || b == ruim {
			return ruim
		}
	}
	if a == "" {
		return b
	}
	return a
}

// naJanela roda fn com a trava do bucket (Opcoes.Janela).
func (m *motor) naJanela(ctx context.Context, fn func() error) error {
	if m.o.Janela == nil {
		return fn()
	}
	var err error
	if !m.o.Janela(ctx, func() { err = fn() }) {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return parar(Ocupado, "o bucket ficou ocupado (backup ou limpeza) até o prazo da rodada")
	}
	return err
}

// fundir confere os itens (em ordem de chave, grupos de chave inteiros) contra
// a listagem, em janelas de até requisicoesPorJanela requisições.
func (m *motor) fundir(ctx context.Context, itens []api.InventoryItem) error {
	m.itens, m.i, m.aberto = itens, 0, false
	for m.i < len(m.itens) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.naJanela(ctx, func() error { return m.avancar(ctx, requisicoesPorJanela) }); err != nil {
			return err
		}
	}
	m.itens = nil
	return nil
}

// carregar busca a página seguinte da listagem. Devolve false quando ela acabou.
func (m *motor) carregar(ctx context.Context) (bool, error) {
	if m.fimDaLista {
		return false, nil
	}
	pg, err := m.o.Bucket.Listar(ctx, m.o.Prefixo, m.proxChave, m.proxVersao)
	m.r.ListRequests++
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, m.erroDoBucket("listagem", err)
	}
	m.comecou = true
	m.buf, m.pos = pg.Entradas, 0
	if pg.Truncada {
		if pg.ProximaChave == "" || (pg.ProximaChave == m.proxChave && pg.ProximaVersao == m.proxVersao && len(pg.Entradas) == 0) {
			// Truncada e sem como continuar (ou parada no mesmo lugar): não é
			// fim de listagem, e seguir daria faltantes falsos.
			return false, parar(Inconsistente, "a listagem veio truncada sem marcador para continuar")
		}
		m.proxChave, m.proxVersao = pg.ProximaChave, pg.ProximaVersao
	} else {
		m.fimDaLista = true
	}
	return true, nil
}

func (m *motor) erroDoBucket(onde string, err error) error {
	switch m.o.Bucket.Classificar(err) {
	case ErroNegado:
		return parar(AcessoNegado, "%s: %v", onde, err)
	case ErroDefinitivo, ErroAusente:
		return parar(ErroDeLista, "%s: %v", onde, err)
	}
	return parar(Transitorio, "%s: %v", onde, err)
}

// consumir tira a entrada corrente da listagem e a põe nos totais do prefixo.
func (m *motor) consumir(e Entrada) error {
	if m.temUltima && e.Key < m.ultimaChave {
		return parar(ForaDeOrdem, "a listagem devolveu %q depois de %q", cortar(e.Key), cortar(m.ultimaChave))
	}
	m.ultimaChave, m.temUltima = e.Key, true
	m.pos++
	if !e.Marcador {
		m.r.BucketVersions++
		m.r.BucketBytes += e.Size
	}
	return nil
}

// avancar anda na fusão até acabar os itens ou gastar as requisições da janela.
func (m *motor) avancar(ctx context.Context, requisicoes int) error {
	for m.i < len(m.itens) {
		if !m.aberto {
			if err := m.abrirGrupo(); err != nil {
				return err
			}
		}
		chave := m.itens[m.i].Key
		for {
			if m.pos >= len(m.buf) {
				if m.fimDaLista {
					break
				}
				if requisicoes == 0 {
					return nil // a janela acabou; o grupo segue aberto
				}
				requisicoes--
				if _, err := m.carregar(ctx); err != nil {
					return err
				}
				continue
			}
			e := m.buf[m.pos]
			if e.Key > chave {
				break
			}
			if err := m.consumir(e); err != nil {
				return err
			}
			switch {
			case e.Key < chave:
				if !e.Marcador {
					m.r.ExtraVersions++
				}
			case e.Marcador:
				if e.Atual {
					m.marcador = true
				}
			default:
				if idx, ok := m.doGrupo[e.VersionID]; ok {
					m.visto[idx-m.i] = true
					m.tamanho[idx-m.i] = e.Size
				} else {
					m.r.ExtraVersions++
				}
			}
		}
		m.fecharGrupo()
	}
	return nil
}

// abrirGrupo prepara as versões da chave corrente do plano.
func (m *motor) abrirGrupo() error {
	chave := m.itens[m.i].Key
	if !strings.HasPrefix(chave, m.o.Prefixo) {
		return parar(ErroDoPlano, "o plano traz chave fora do prefixo do servidor: %q", cortar(chave))
	}
	if m.temPlano && chave <= m.ultimoPlano {
		return parar(ErroDoPlano, "o plano não está em ordem de chave: %q depois de %q", cortar(chave), cortar(m.ultimoPlano))
	}
	j := m.i
	clear(m.doGrupo)
	for j < len(m.itens) && m.itens[j].Key == chave {
		m.doGrupo[m.itens[j].VersionID] = j
		j++
	}
	n := j - m.i
	if cap(m.visto) < n {
		m.visto, m.tamanho = make([]bool, n), make([]int64, n)
	}
	m.visto, m.tamanho = m.visto[:n], m.tamanho[:n]
	for k := range m.visto {
		m.visto[k], m.tamanho[k] = false, 0
	}
	m.fimDoGrupo, m.marcador, m.aberto = j, false, true
	return nil
}

// fecharGrupo decide cada versão da chave: a listagem já passou por ela.
func (m *motor) fecharGrupo() {
	chave := m.itens[m.i].Key
	algumaPresente := false
	for k, it := range m.itens[m.i:m.fimDoGrupo] {
		v := api.InventoryVersion{Key: it.Key, VersionID: it.VersionID}
		switch {
		case !m.visto[k]:
			// Já dada por indisponível e ainda ausente: nada de novo a relatar.
			if !it.Unavailable {
				m.candidatosTotal++
				if len(m.candidatos) < MaxFaltantes {
					m.candidatos = append(m.candidatos, v)
				}
			}
		case m.tamanho[k] != it.Size:
			algumaPresente = true
			if !it.Unavailable {
				m.r.SizeMismatchTotal++
				if len(m.r.SizeMismatch) < MaxDivergencias {
					m.r.SizeMismatch = append(m.r.SizeMismatch, api.InventorySize{Key: it.Key, VersionID: it.VersionID, Size: m.tamanho[k]})
				}
			}
		default:
			algumaPresente = true
			m.r.Found++
			m.r.FoundBytes += it.Size
			if it.Unavailable {
				if len(m.r.FoundAgain) < MaxFaltantes {
					m.r.FoundAgain = append(m.r.FoundAgain, v)
				} else {
					m.r.Truncated = true
				}
			} else {
				m.oferecerParaAmostra(v)
			}
		}
	}
	// O marcador de exclusão só interessa por cima de versão que está lá.
	if m.marcador && algumaPresente {
		m.r.DeleteMarkersTotal++
		if len(m.r.DeleteMarkers) < MaxDivergencias {
			m.r.DeleteMarkers = append(m.r.DeleteMarkers, api.InventoryKey{Key: chave})
		}
	}
	m.ultimoPlano, m.temPlano = chave, true
	m.i, m.aberto = m.fimDoGrupo, false
}

// oferecerParaAmostra mantém uma amostra uniforme das versões encontradas
// (reservatório): o tamanho dela não cresce com o bucket.
func (m *motor) oferecerParaAmostra(v api.InventoryVersion) {
	if m.o.Amostra <= 0 {
		return
	}
	m.oferecidos++
	if len(m.amostra) < m.o.Amostra {
		m.amostra = append(m.amostra, v)
		return
	}
	if k := m.sorteio.Int64N(m.oferecidos); k < int64(m.o.Amostra) {
		m.amostra[k] = v
	}
}

// esgotarListagem lê o que sobrou do prefixo depois da última chave do plano.
func (m *motor) esgotarListagem(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		acabou := false
		err := m.naJanela(ctx, func() error {
			for n := requisicoesPorJanela; ; {
				for m.pos < len(m.buf) {
					e := m.buf[m.pos]
					if err := m.consumir(e); err != nil {
						return err
					}
					if !e.Marcador {
						m.r.ExtraVersions++
					}
				}
				if m.fimDaLista {
					acabou = true
					return nil
				}
				if n == 0 {
					return nil
				}
				n--
				if _, err := m.carregar(ctx); err != nil {
					return err
				}
			}
		})
		if err != nil {
			return err
		}
		if acabou {
			m.buf = nil
			return nil
		}
	}
}

// emLotes roda fn para cada item, em janelas de até requisicoesPorJanela.
func emLotes[T any](ctx context.Context, m *motor, itens []T, fn func(T) error) error {
	for ini := 0; ini < len(itens); ini += requisicoesPorJanela {
		if err := ctx.Err(); err != nil {
			return err
		}
		fim := min(ini+requisicoesPorJanela, len(itens))
		err := m.naJanela(ctx, func() error {
			for _, it := range itens[ini:fim] {
				if err := fn(it); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// confirmarFaltantes pergunta ao bucket, versão a versão, pelas que a listagem
// não trouxe. Só a que responde 404 vira faltante. A que responde 200 foi
// desmentida — a listagem do provedor falhou com ela —, e se forem a maioria a
// rodada não vale.
func (m *motor) confirmarFaltantes(ctx context.Context) error {
	confirmados := make([]api.InventoryVersion, 0, len(m.candidatos))
	err := emLotes(ctx, m, m.candidatos, func(v api.InventoryVersion) error {
		_, err := m.o.Bucket.Cabecalho(ctx, v.Key, v.VersionID)
		m.r.HeadRequests++
		if err == nil {
			m.r.Unconfirmed++
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.o.Bucket.Classificar(err) == ErroAusente {
			confirmados = append(confirmados, v)
			return nil
		}
		return m.erroDoBucket("confirmação de versão ausente", err)
	})
	if err != nil {
		return err
	}
	if m.r.Unconfirmed >= 10 && m.r.Unconfirmed > int64(len(confirmados)) {
		return parar(Inconsistente, "o HeadObject achou %d de %d versões que a listagem não trouxe", m.r.Unconfirmed, len(m.candidatos))
	}
	m.r.Missing = confirmados
	// As que passaram do teto não foram conferidas uma a uma: contam no total
	// (a lista vai cortada), a não ser que a listagem tenha sido desmentida.
	excedente := m.candidatosTotal - int64(len(m.candidatos))
	if m.r.Unconfirmed > 0 {
		excedente = 0
	}
	m.r.MissingTotal = int64(len(confirmados)) + excedente
	if m.candidatosTotal > int64(len(m.candidatos)) || m.r.SizeMismatchTotal > int64(len(m.r.SizeMismatch)) {
		m.r.Truncated = true
	}
	m.candidatos = nil
	return nil
}

// conferirAmostra faz o HeadObject da amostra. O que não responde conta como
// erro da amostra e a rodada segue: a amostra não decide se falta versão.
func (m *motor) conferirAmostra(ctx context.Context) error {
	itens := make([]api.InventorySampleItem, 0, len(m.amostra))
	err := emLotes(ctx, m, m.amostra, func(v api.InventoryVersion) error {
		h, err := m.o.Bucket.Cabecalho(ctx, v.Key, v.VersionID)
		m.r.HeadRequests++
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.r.Sample.Errors++
			return nil
		}
		m.r.Sample.Checked++
		itens = append(itens, api.InventorySampleItem{
			Key: v.Key, VersionID: v.VersionID, SHA256: h.SHA256, LockMode: h.LockMode,
			RetainUntil: h.RetainUntil, LastModified: h.LastModified,
		})
		return nil
	})
	m.r.Sample.Items = itens
	m.amostra = nil
	return err
}

// Caber corta o relato para o corpo ficar dentro de teto bytes: primeiro o que
// é só aviso (a amostra, os marcadores), depois as listas que importam — e aí
// marca Truncated, para o painel não tratar a lista como completa.
func Caber(r *api.InventoryResult, teto int) {
	cabe := func() bool {
		corpo, err := json.Marshal(r)
		return err == nil && len(corpo) <= teto
	}
	if cabe() {
		return
	}
	// A amostra vale pelo que o painel compara; cortada pela metade ainda vale.
	for len(r.Sample.Items) > 0 && !cabe() {
		r.Sample.Items = r.Sample.Items[:len(r.Sample.Items)/2]
	}
	for len(r.DeleteMarkers) > 0 && !cabe() {
		r.DeleteMarkers = r.DeleteMarkers[:len(r.DeleteMarkers)/2]
	}
	for len(r.FoundAgain) > 0 && !cabe() {
		r.FoundAgain = r.FoundAgain[:len(r.FoundAgain)/2]
		r.Truncated = true
	}
	for len(r.SizeMismatch) > 0 && !cabe() {
		r.SizeMismatch = r.SizeMismatch[:len(r.SizeMismatch)/2]
		r.Truncated = true
	}
	for len(r.Missing) > 0 && !cabe() {
		r.Missing = r.Missing[:len(r.Missing)*3/4]
		r.Truncated = true
	}
	if !cabe() {
		r.Detail = ""
	}
}

// cortar deixa um texto de diagnóstico em até 200 bytes, sem NUL nem controle.
func cortar(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			c = ' '
		}
		if b.Len()+len(string(c)) > 200 {
			break
		}
		b.WriteRune(c)
	}
	return b.String()
}
