package inventario

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
)

// Os testes das correções da revisão independente do S9.

// janelaMedida é uma Opcoes.Janela que mede quanto cada tomada da trava durou
// e quantas requisições ao bucket couberam nela.
type janelaMedida struct {
	dentro      atomic.Bool
	janelas     atomic.Int32
	naJanela    atomic.Int32
	maiorJanela atomic.Int32
	maisLonga   atomic.Int64 // ns
}

func (j *janelaMedida) janela(_ context.Context, fn func()) bool {
	j.janelas.Add(1)
	j.naJanela.Store(0)
	j.dentro.Store(true)
	ini := time.Now()
	fn()
	if d := int64(time.Since(ini)); d > j.maisLonga.Load() {
		j.maisLonga.Store(d)
	}
	j.dentro.Store(false)
	return true
}

// contar é o gancho do S3 falso: conta as requisições de cada janela.
func (j *janelaMedida) contar(op string) {
	if op == "GetBucketVersioning" {
		return
	}
	if n := j.naJanela.Add(1); n > j.maiorJanela.Load() {
		j.maiorJanela.Store(n)
	}
}

// M2 — o provedor aceita a conexão e não responde. Sem prazo por requisição a
// janela (e a trava do bucket, com o backup esperando) ficava segurada até o
// prazo da rodada inteira.
func TestProvedorQueNaoRespondeNaoSeguraATrava(t *testing.T) {
	for _, caso := range []struct {
		nome, op string
		apagar   bool
	}{
		{"listagem", "ListObjectVersions", false},
		{"confirmação de versão ausente", "HeadObjectVersion", true},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			f, b := novoBucket(t)
			semear(f, 3000, 1, arq)
			itens := catalogo(f)
			if caso.apagar {
				f.ApagarVersao(itens[7].Key, itens[7].VersionID)
			}
			// A primeira listagem de um S3 falso recém-semeado é lenta (ele ordena
			// as chaves): fica fora da medição.
			if _, err := b.Listar(context.Background(), prefixo, "", ""); err != nil {
				t.Fatal(err)
			}
			f.Zerar()
			var n atomic.Int32
			solta := make(chan struct{})
			t.Cleanup(func() { close(solta) })
			f.Antes(func(op string, _ *http.Request) {
				// A listagem para de responder na segunda página; o HEAD, no primeiro.
				if op == caso.op && (op != "ListObjectVersions" || n.Add(1) == 2) {
					select {
					case <-solta:
					case <-time.After(5 * time.Second):
					}
				}
			})
			var j janelaMedida
			ini := time.Now()
			r := rodar(t, b, itens, 10_000, func(o *Opcoes) {
				o.Prazo, o.PrazoDaRequisicao, o.Janela = time.Minute, 600*time.Millisecond, j.janela
			})
			if r.Complete || r.Interrupted != Transitorio {
				t.Fatalf("requisição sem resposta: esperava a rodada incompleta por erro do momento, veio complete=%v %q (%s)", r.Complete, r.Interrupted, r.Detail)
			}
			// Foi mesmo a requisição que o teste travou que parou a rodada (e não
			// uma anterior, lenta por outro motivo).
			if listas, heads := f.N("ListObjectVersions"), f.N("HeadObjectVersion"); (caso.apagar && (listas != 3 || heads != 1)) || (!caso.apagar && (listas != 2 || heads != 0)) {
				t.Fatalf("a rodada parou em outra requisição: %d listagens, %d HEAD (%s)", listas, heads, r.Detail)
			}
			if len(r.Missing) != 0 || r.MissingTotal != 0 || len(r.SizeMismatch) != 0 {
				t.Fatalf("requisição sem resposta nunca vira faltante: %+v", r.Missing)
			}
			if d := time.Since(ini); d > 3*time.Second {
				t.Fatalf("a rodada ficou %s presa na requisição sem resposta (o prazo da requisição era 600 ms; o da rodada, 1 min)", d)
			}
			if d := time.Duration(j.maisLonga.Load()); d > 3*time.Second {
				t.Fatalf("a trava do bucket ficou segurada por %s numa janela", d)
			}
		})
	}
}

// O prazo padrão de uma requisição é o do contrato.
func TestPrazoPadraoDaRequisicao(t *testing.T) {
	m := &motor{}
	ctx, soltar := m.requisicao(context.Background())
	defer soltar()
	fim, tem := ctx.Deadline()
	if falta := time.Until(fim); !tem || falta > PrazoDaRequisicao || falta < PrazoDaRequisicao-5*time.Second || PrazoDaRequisicao != 60*time.Second {
		t.Fatalf("o prazo padrão de uma requisição ao bucket é 60 s: tem=%v falta=%s", tem, falta)
	}
}

// M2 — com backup esperando, a janela acaba na requisição em curso: o backup
// não espera as até 20 da janela (cada uma com até 60 s).
func TestJanelaAcabaCedoComBackupEsperando(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 6000, 1, arq)
	itens := catalogo(f)
	// Cinco faltantes (cinco HEAD de confirmação) e amostra de 30: as três fases
	// que pedem janela — fusão, confirmação e amostra.
	for _, i := range []int{10, 2000, 2001, 4000, 5999} {
		f.ApagarVersao(itens[i].Key, itens[i].VersionID)
	}
	// E dois mil objetos a mais depois da última chave do plano (a quarta fase).
	semear(f, 2000, 1, func(i int) string { return fmt.Sprintf("zz-fora-do-plano/%05d", i) })

	var j janelaMedida
	f.Antes(func(op string, _ *http.Request) { j.contar(op) })
	var perguntas atomic.Int32
	r := rodar(t, b, itens, 10_000, func(o *Opcoes) {
		o.Amostra, o.Janela = 30, j.janela
		o.Ceder = func() bool { perguntas.Add(1); return true }
	})
	exigirCompleto(t, r)
	if r.MissingTotal != 5 || r.Found != 5995 || r.ExtraVersions != 2000 || r.Sample.Checked != 30 {
		t.Fatalf("cedendo a vez a rodada chega ao mesmo resultado: faltando=%d found=%d a mais=%d amostra=%d", r.MissingTotal, r.Found, r.ExtraVersions, r.Sample.Checked)
	}
	if j.maiorJanela.Load() != 1 {
		t.Fatalf("com backup esperando o tempo todo, cada janela faz UMA requisição e solta: a maior fez %d", j.maiorJanela.Load())
	}
	if perguntas.Load() == 0 {
		t.Fatal("ninguém perguntou se havia backup esperando")
	}

	// Sem backup esperando, as janelas seguem cheias (a rodada não fica mais lenta).
	var k janelaMedida
	f.Antes(func(op string, _ *http.Request) { k.contar(op) })
	r = rodar(t, b, itens, 10_000, func(o *Opcoes) {
		o.Amostra, o.Janela = 30, k.janela
		o.Ceder = func() bool { return false }
	})
	exigirCompleto(t, r)
	if k.maiorJanela.Load() != requisicoesPorJanela || k.janelas.Load() >= j.janelas.Load() {
		t.Fatalf("sem backup esperando a janela vai até %d requisições: maior=%d, %d janelas (cedendo foram %d)", requisicoesPorJanela, k.maiorJanela.Load(), k.janelas.Load(), j.janelas.Load())
	}
}

// B5 — um backup inteiro roda entre duas janelas, exatamente na virada de
// página do plano: envia e cataloga arquivos cujas chaves caem no pedaço da
// listagem que o agente já tinha na memória. A página seguinte do plano os
// traz; a listagem em memória, não. (Antes: até 9 viravam "desmentidas", e de
// 10 em diante a rodada saía listing_inconsistent.)
func TestBackupNaViradaDePaginaDoPlano(t *testing.T) {
	for _, novos := range []int{3, 12, 300} {
		t.Run(fmt.Sprintf("%d arquivos novos", novos), func(t *testing.T) {
			f, b := novoBucket(t)
			semear(f, 50, 1, func(i int) string { return fmt.Sprintf("dir/%04d", i*10) })
			antes := catalogo(f)
			primeira := &api.InventoryPlanResponse{RunID: "r", Prefix: prefixo, Page: 1, Items: antes[:25], Next: "x"}
			o := Opcoes{Bucket: b, Prefixo: prefixo, Prazo: time.Minute,
				Proxima: func(context.Context, string) (*api.InventoryPlanResponse, error) {
					// O backup: arquivos novos, enviados E catalogados, com chaves
					// depois da última já conferida.
					for i := range novos {
						f.Semear(prefixo+fmt.Sprintf("dir/%04d-novo-%04d", 300+(i%20)*10, i), bancada.VersaoFalsa{Tamanho: 1})
					}
					todos := catalogo(f)
					resto := todos[sort.Search(len(todos), func(i int) bool { return todos[i].Key > antes[24].Key }):]
					return &api.InventoryPlanResponse{RunID: "r", Prefix: prefixo, Page: 2, Items: resto}, nil
				}}
			r, _ := Rodar(context.Background(), o, primeira)
			exigirCompleto(t, r)
			if r.PlanVersions != int64(50+novos) || r.Found != r.PlanVersions {
				t.Fatalf("todas as versões do plano estão no bucket: found=%d de %d", r.Found, r.PlanVersions)
			}
			if r.Unconfirmed != 0 || r.MissingTotal != 0 || r.HeadRequests != 0 {
				t.Fatalf("nenhuma versão devia ter passado por suspeita: desmentidas=%d faltando=%d HEAD=%d", r.Unconfirmed, r.MissingTotal, r.HeadRequests)
			}
			if r.BucketVersions != int64(50+novos) || r.ExtraVersions != 0 {
				t.Fatalf("a retomada não conta entrada duas vezes: versões no bucket=%d a mais=%d", r.BucketVersions, r.ExtraVersions)
			}
		})
	}
}

// B5, a garantia que não pode mudar: a versão que SOME na virada de página
// (apagada por fora) continua sendo faltante, confirmada por HeadObject — e a
// que some ANTES da última entrada consumida também.
func TestVersaoApagadaNaViradaDePaginaSegueFaltante(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 50, 2, func(i int) string { return fmt.Sprintf("dir/%04d", i*10) })
	itens := catalogo(f) // 100 versões, duas por chave
	primeira := &api.InventoryPlanResponse{RunID: "r", Prefix: prefixo, Page: 1, Items: itens[:50], Next: "x"}
	o := Opcoes{Bucket: b, Prefixo: prefixo, Prazo: time.Minute,
		Proxima: func(context.Context, string) (*api.InventoryPlanResponse, error) {
			f.ApagarVersao(itens[70].Key, itens[70].VersionID)
			return &api.InventoryPlanResponse{RunID: "r", Prefix: prefixo, Page: 2, Items: itens[50:]}, nil
		}}
	r, _ := Rodar(context.Background(), o, primeira)
	exigirCompleto(t, r)
	if r.MissingTotal != 1 || len(r.Missing) != 1 || r.Missing[0].Key != itens[70].Key || r.Missing[0].VersionID != itens[70].VersionID {
		t.Fatalf("a versão apagada entre as páginas é faltante: %+v (total %d)", r.Missing, r.MissingTotal)
	}
	if r.Found != 99 || r.BucketVersions != 99 || r.ExtraVersions != 0 || r.Unconfirmed != 0 {
		t.Fatalf("o resto está no lugar, contado uma vez: found=%d no bucket=%d a mais=%d desmentidas=%d", r.Found, r.BucketVersions, r.ExtraVersions, r.Unconfirmed)
	}
}

// B6 — "tamanho diferente" só com o HeadObject da versão confirmando: a
// listagem de um provedor compatível que erra o tamanho não marca versão.
func TestTamanhoDiferenteSoComHeadObject(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 400, 1, arq)
	itens := catalogo(f)
	mentir := func(quantas int) Bucket {
		return bucketTorto{Bucket: b, listar: func(pg Pagina) Pagina {
			for i := range pg.Entradas {
				if i >= 10 && i < 10+quantas {
					pg.Entradas[i].Size += 3
				}
			}
			return pg
		}}
	}
	// A listagem erra o tamanho de três versões; o HEAD diz o do catálogo.
	f.Zerar()
	r := rodar(t, mentir(3), itens, 1000)
	exigirCompleto(t, r)
	if r.SizeMismatchTotal != 0 || len(r.SizeMismatch) != 0 {
		t.Fatalf("tamanho que só a listagem diz diferente não é relatado: %+v", r.SizeMismatch)
	}
	if r.Unconfirmed != 3 || r.Found != 400 || f.N("HeadObjectVersion") != 3 || r.HeadRequests != 3 {
		t.Fatalf("as três são conferidas por HEAD e contam como encontradas: desmentidas=%d found=%d HEAD=%d", r.Unconfirmed, r.Found, f.N("HeadObjectVersion"))
	}
	// Muitas: a listagem do provedor não é confiável, e a rodada não vale.
	r = rodar(t, mentir(40), itens, 1000)
	if r.Complete || r.Interrupted != Inconsistente || len(r.SizeMismatch) != 0 {
		t.Fatalf("tamanhos desmentidos em massa: esperava listing_inconsistent, veio complete=%v %q", r.Complete, r.Interrupted)
	}

	// O catálogo diz outro tamanho, e a listagem e o HEAD concordam entre si:
	// é divergência de verdade, relatada com o tamanho do HEAD.
	torto := append([]api.InventoryItem(nil), itens...)
	torto[20].Size += 5
	r = rodar(t, b, torto, 1000)
	exigirCompleto(t, r)
	if r.SizeMismatchTotal != 1 || len(r.SizeMismatch) != 1 || r.SizeMismatch[0].Key != itens[20].Key || r.SizeMismatch[0].Size != itens[20].Size {
		t.Fatalf("divergência confirmada: %+v", r.SizeMismatch)
	}
	// A versão some entre a listagem e o HEAD: nem tamanho diferente, nem faltante.
	f.Antes(func(op string, req *http.Request) {
		if op == "HeadObjectVersion" {
			f.ApagarVersao(itens[20].Key, itens[20].VersionID)
		}
	})
	r = rodar(t, b, torto, 1000)
	f.Antes(nil)
	exigirCompleto(t, r)
	if len(r.SizeMismatch) != 0 || r.SizeMismatchTotal != 0 || r.MissingTotal != 0 || r.Unconfirmed != 1 {
		t.Fatalf("versão que a listagem viu e o HEAD não achou não é afirmada: tamanho=%+v faltando=%d desmentidas=%d", r.SizeMismatch, r.MissingTotal, r.Unconfirmed)
	}
}

// B6 — erro do momento na confirmação do tamanho: incompleta, nada afirmado; e
// o teto de confirmações (200 HEAD) com a lista cortada.
func TestConfirmacaoDeTamanhoErroETeto(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 300, 1, arq)
	itens := catalogo(f)
	torto := append([]api.InventoryItem(nil), itens...)
	for i := range 250 {
		torto[i].Size += 9
	}
	f.Zerar()
	r := rodar(t, b, torto, 1000)
	exigirCompleto(t, r)
	if len(r.SizeMismatch) != MaxDivergencias || r.SizeMismatchTotal != 250 || !r.Truncated || f.N("HeadObjectVersion") != MaxDivergencias {
		t.Fatalf("250 divergências: 200 conferidas e relatadas, total 250, lista cortada — veio lista=%d total=%d cortada=%v HEAD=%d", len(r.SizeMismatch), r.SizeMismatchTotal, r.Truncated, f.N("HeadObjectVersion"))
	}
	f.Recusar(func(op string, _ *http.Request) (int, string) {
		if op == "HeadObjectVersion" {
			return 503, "SlowDown"
		}
		return 0, ""
	})
	r = rodar(t, b, torto, 1000)
	f.Recusar(nil)
	if r.Complete || r.Interrupted != Transitorio || len(r.SizeMismatch) != 0 || r.SizeMismatchTotal != 0 {
		t.Fatalf("503 na confirmação do tamanho: esperava transient sem lista, veio complete=%v %q %d", r.Complete, r.Interrupted, len(r.SizeMismatch))
	}
}
