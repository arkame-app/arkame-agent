package inventario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
)

const prefixo = "arkame/data/agente-1/"

// novoBucket sobe o S3 falso e devolve o Bucket de verdade (pelo SDK) sobre ele.
func novoBucket(t *testing.T) (*bancada.S3Falso, BucketS3) {
	t.Helper()
	f := bancada.NovoS3Falso()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, BucketS3{Cliente: bancada.ClienteS3(srv.URL), Nome: "b"}
}

// catalogo são as versões que o "painel" diz existir: todas as versões (sem os
// marcadores) das chaves sob o prefixo, em ordem de bytes da chave.
func catalogo(f *bancada.S3Falso) []api.InventoryItem {
	chaves := f.Chaves()
	sort.Strings(chaves)
	var itens []api.InventoryItem
	for _, k := range chaves {
		if !strings.HasPrefix(k, prefixo) {
			continue
		}
		vs := f.Versoes(k)
		sort.Slice(vs, func(i, j int) bool { return vs[i].ID < vs[j].ID })
		for _, v := range vs {
			if !v.Marcador {
				itens = append(itens, api.InventoryItem{Key: k, VersionID: v.ID, Size: v.Tamanho})
			}
		}
	}
	return itens
}

// paginas corta o plano em páginas de tam itens; o cursor é o índice.
func paginas(itens []api.InventoryItem, tam int) (*api.InventoryPlanResponse, func(context.Context, string) (*api.InventoryPlanResponse, error)) {
	pagina := func(ini int) *api.InventoryPlanResponse {
		fim := min(ini+tam, len(itens))
		p := &api.InventoryPlanResponse{RunID: "r", Prefix: prefixo, Page: ini/tam + 1, Items: itens[ini:fim]}
		if fim < len(itens) {
			p.Next = strconv.Itoa(fim)
		}
		return p
	}
	return pagina(0), func(_ context.Context, cursor string) (*api.InventoryPlanResponse, error) {
		ini, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, err
		}
		return pagina(ini), nil
	}
}

func rodar(t *testing.T, b Bucket, itens []api.InventoryItem, tam int, ajustes ...func(*Opcoes)) api.InventoryResult {
	t.Helper()
	primeira, proxima := paginas(itens, tam)
	o := Opcoes{Bucket: b, Prefixo: prefixo, Prazo: time.Minute, Amostra: 0, Proxima: proxima, Sorteio: rand.New(rand.NewPCG(1, 2))}
	for _, a := range ajustes {
		a(&o)
	}
	r, interrompida := Rodar(context.Background(), o, primeira)
	if interrompida {
		t.Fatal("a rodada foi dada por interrompida sem o contexto ter sido cancelado")
	}
	return r
}

// semear põe n chaves com `versoes` versões cada sob o prefixo.
func semear(f *bancada.S3Falso, n, versoes int, nome func(i int) string) {
	for i := range n {
		for v := range versoes {
			f.Semear(prefixo+nome(i), bancada.VersaoFalsa{Tamanho: int64(100 + i + v), SHA256: fmt.Sprintf("%064x", i*10+v)})
		}
	}
}

func arq(i int) string { return fmt.Sprintf("dados/arq-%05d.txt", i) }

func exigirCompleto(t *testing.T, r api.InventoryResult) {
	t.Helper()
	if !r.Complete || r.Interrupted != "" {
		t.Fatalf("a rodada devia ter chegado ao fim: complete=%v interrupted=%q detail=%q", r.Complete, r.Interrupted, r.Detail)
	}
}

func chavesDe(vs []api.InventoryVersion) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.TrimPrefix(v.Key, prefixo) + "@" + v.VersionID
	}
	sort.Strings(out)
	return out
}

func TestBucketIgualAoCatalogoNaoTemFaltante(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 30, 2, arq)
	itens := catalogo(f)
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.PlanVersions != 60 || r.Found != 60 || r.MissingTotal != 0 || len(r.Missing) != 0 || r.ExtraVersions != 0 || r.BucketVersions != 60 {
		t.Fatalf("esperava 60 do plano, 60 encontradas, 0 faltando, 0 a mais: %+v", r)
	}
	if r.Versioning != "Enabled" || r.ListRequests != 1 || r.HeadRequests != 0 {
		t.Fatalf("versionamento %q, %d listagens, %d HEAD", r.Versioning, r.ListRequests, r.HeadRequests)
	}
}

func TestVersaoApagadaNoBucketViraFaltante(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 30, 2, arq)
	itens := catalogo(f)
	sumiu := itens[17]
	if !f.ApagarVersao(sumiu.Key, sumiu.VersionID) {
		t.Fatal("não apagou a versão do teste")
	}
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.MissingTotal != 1 || len(r.Missing) != 1 || r.Missing[0].Key != sumiu.Key || r.Missing[0].VersionID != sumiu.VersionID {
		t.Fatalf("esperava só a versão apagada como faltante: %+v (total %d)", r.Missing, r.MissingTotal)
	}
	if r.Found != 59 || r.HeadRequests != 1 || r.Unconfirmed != 0 || r.Truncated {
		t.Fatalf("59 encontradas e um HEAD de confirmação: found=%d head=%d desmentidas=%d cortado=%v", r.Found, r.HeadRequests, r.Unconfirmed, r.Truncated)
	}
	// A chave inteira apagada (as duas versões), no fim do prefixo: também.
	ultima := itens[len(itens)-1]
	f.ApagarVersao(ultima.Key, ultima.VersionID)
	f.ApagarVersao(itens[len(itens)-2].Key, itens[len(itens)-2].VersionID)
	r = rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.MissingTotal != 3 {
		t.Fatalf("a chave que saiu inteira do fim do prefixo tem de dar 2 faltantes a mais: %v", chavesDe(r.Missing))
	}
}

func TestMarcadorDeExclusaoNaoEFaltante(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 10, 2, arq)
	itens := catalogo(f)
	// Marcador por cima de uma chave cujas versões seguem no bucket.
	f.PorMarcador(prefixo + arq(3))
	// Marcador por cima de uma chave cuja versão catalogada saiu: aí falta.
	f.PorMarcador(prefixo + arq(6))
	apagadas := 0
	for _, it := range itens {
		if it.Key == prefixo+arq(6) {
			f.ApagarVersao(it.Key, it.VersionID)
			apagadas++
		}
	}
	// Marcador numa chave que o catálogo não conhece: não interessa.
	f.PorMarcador(prefixo + "dados/nunca-catalogada.txt")

	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.DeleteMarkersTotal != 1 || len(r.DeleteMarkers) != 1 || r.DeleteMarkers[0].Key != prefixo+arq(3) {
		t.Fatalf("só a chave com versão presente e marcador por cima é relatada: %+v", r.DeleteMarkers)
	}
	if r.MissingTotal != int64(apagadas) || r.Found != int64(len(itens)-apagadas) {
		t.Fatalf("as versões sob o marcador que seguem no bucket contam como encontradas: found=%d faltando=%d", r.Found, r.MissingTotal)
	}
	for _, m := range r.Missing {
		if m.Key != prefixo+arq(6) {
			t.Fatalf("versão que ainda restaura foi dada por faltante: %+v", m)
		}
	}
	if r.BucketVersions != int64(len(itens)-apagadas) {
		t.Fatalf("marcador não conta como versão do bucket: %d", r.BucketVersions)
	}
}

func TestObjetoEstranhoNoPrefixoEIgnorado(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 10, 1, arq)
	itens := catalogo(f)
	// A mais: antes, no meio e depois das chaves do plano, e uma versão a mais
	// de uma chave catalogada. E objetos de outro servidor, fora do prefixo.
	f.Semear(prefixo+"aaa-sessao-que-falhou.tmp", bancada.VersaoFalsa{Tamanho: 5})
	f.Semear(prefixo+"dados/arq-00004.txt.parcial", bancada.VersaoFalsa{Tamanho: 5})
	f.Semear(prefixo+"zzz/depois-de-tudo", bancada.VersaoFalsa{Tamanho: 5})
	f.Semear(prefixo+arq(2), bancada.VersaoFalsa{Tamanho: 5})
	f.Semear("arkame/data/agente-2/dados/de-outro-servidor.txt", bancada.VersaoFalsa{Tamanho: 5})
	f.Semear("arkame/data/agente-1-vizinho/x", bancada.VersaoFalsa{Tamanho: 5})

	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.MissingTotal != 0 || r.Found != 10 || r.SizeMismatchTotal != 0 {
		t.Fatalf("objeto a mais não é erro: found=%d faltando=%d tamanho=%d", r.Found, r.MissingTotal, r.SizeMismatchTotal)
	}
	if r.ExtraVersions != 4 || r.BucketVersions != 14 {
		t.Fatalf("esperava 4 versões a mais, só do prefixo deste servidor (14 no total): a mais=%d total=%d", r.ExtraVersions, r.BucketVersions)
	}
}

func TestVersionamentoSuspensoERelatado(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 2500, 1, arq)
	itens := catalogo(f)
	f.Versionamento("Suspended")
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.Versioning != "Suspended" || r.MissingTotal != 0 {
		t.Fatalf("versionamento suspenso tem de ir no relato, sem inventar faltante: %q %d", r.Versioning, r.MissingTotal)
	}
	// Suspenso NO MEIO da rodada: a conferência do fim pega.
	f.Versionamento("Enabled")
	var listas atomic.Int32
	f.Antes(func(op string, _ *http.Request) {
		if op == "ListObjectVersions" && listas.Add(1) == 2 {
			f.Versionamento("Suspended")
		}
	})
	r = rodar(t, b, itens, 1000)
	f.Antes(nil)
	exigirCompleto(t, r)
	if r.Versioning != "Suspended" {
		t.Fatalf("suspenso no meio da rodada: esperava Suspended, veio %q", r.Versioning)
	}
	f.Versionamento("")
	if r = rodar(t, b, itens, 1000); r.Versioning != "Off" {
		t.Fatalf("nunca ligado: esperava Off, veio %q", r.Versioning)
	}
}

// 25 mil versões, com chaves em que a ordem de bytes não é a alfabética, em
// páginas de 10 mil do plano e de mil da listagem — com uma chave de três
// versões atravessando a divisa de página do plano.
func TestVinteECincoMilVersoesComAcentoEmOrdemDeBytes(t *testing.T) {
	f, b := novoBucket(t)
	grupos := []string{"ação/", "Zebra/", "zebra/", "árvore/", "a/", "B/", "ç/", "é combinado/", "日本語/", "😀/", "a-b/", "a b/", "a.b/", "a_b/", "É/", "ñandú/", "com+mais/", "com%25/"}
	for i := range 12_500 {
		k := prefixo + grupos[i%len(grupos)] + fmt.Sprintf("arq-%06d.dat", i)
		for v := range 2 {
			f.Semear(k, bancada.VersaoFalsa{Tamanho: int64(10 + i%7 + v)})
		}
	}
	itens := catalogo(f)
	if len(itens) != 25_000 {
		t.Fatalf("o bucket de teste tem %d versões", len(itens))
	}
	// A divisa da página do plano cai no meio das versões de uma chave.
	const tam = 9_999
	if itens[tam-1].Key != itens[tam].Key {
		t.Fatalf("o teste precisa de uma chave atravessando a divisa da página")
	}
	r := rodar(t, b, itens, tam)
	exigirCompleto(t, r)
	if r.Found != 25_000 || r.MissingTotal != 0 || r.ExtraVersions != 0 || r.SizeMismatchTotal != 0 {
		t.Fatalf("tudo no lugar: found=%d faltando=%d a mais=%d tamanho=%d (%s)", r.Found, r.MissingTotal, r.ExtraVersions, r.SizeMismatchTotal, r.Detail)
	}
	if r.ListRequests != 25 {
		t.Fatalf("25 mil versões são 25 listagens de mil, vieram %d", r.ListRequests)
	}
	// Faltando: a primeira, as duas da divisa, uma no meio e a última.
	apagar := []api.InventoryItem{itens[0], itens[tam-1], itens[tam], itens[17_321], itens[len(itens)-1]}
	var esperadas []api.InventoryVersion
	for _, it := range apagar {
		f.ApagarVersao(it.Key, it.VersionID)
		esperadas = append(esperadas, api.InventoryVersion{Key: it.Key, VersionID: it.VersionID})
	}
	r = rodar(t, b, itens, tam)
	exigirCompleto(t, r)
	if got, want := chavesDe(r.Missing), chavesDe(esperadas); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("faltantes:\n  esperava %v\n  vieram   %v", want, got)
	}
	if r.Found != 25_000-5 || r.MissingTotal != 5 {
		t.Fatalf("found=%d faltando=%d", r.Found, r.MissingTotal)
	}
}

func TestChaveQueOXmlNaoCarregaEEncontrada(t *testing.T) {
	f, b := novoBucket(t)
	for _, nome := range []string{"dados/com espaço.txt", "dados/com+mais.txt", "dados/com%porcento.txt", "dados/controle\x01aqui.txt", "dados/e&comercial<>.txt"} {
		f.Semear(prefixo+nome, bancada.VersaoFalsa{Tamanho: 9})
	}
	itens := catalogo(f)
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.Found != 5 || r.MissingTotal != 0 {
		t.Fatalf("as cinco chaves existem e tinham de ser encontradas: found=%d faltando=%d %v", r.Found, r.MissingTotal, chavesDe(r.Missing))
	}
}

func TestErroPassageiroNoMeioDaListagemNaoViraFaltante(t *testing.T) {
	casos := map[string]func(f *bancada.S3Falso, n *atomic.Int32){
		"500": func(f *bancada.S3Falso, n *atomic.Int32) {
			f.Falhar(func(op string, _ *http.Request) bool { return op == "ListObjectVersions" && n.Add(1) == 3 })
		},
		"503 SlowDown": func(f *bancada.S3Falso, n *atomic.Int32) {
			f.Recusar(func(op string, _ *http.Request) (int, string) {
				if op == "ListObjectVersions" && n.Add(1) == 3 {
					return http.StatusServiceUnavailable, "SlowDown"
				}
				return 0, ""
			})
		},
		"429": func(f *bancada.S3Falso, n *atomic.Int32) {
			f.Recusar(func(op string, _ *http.Request) (int, string) {
				if op == "ListObjectVersions" && n.Add(1) == 3 {
					return http.StatusTooManyRequests, "TooManyRequests"
				}
				return 0, ""
			})
		},
	}
	for nome, armar := range casos {
		t.Run(nome, func(t *testing.T) {
			f, b := novoBucket(t)
			semear(f, 5000, 1, arq)
			itens := catalogo(f)
			// Uma versão falta de verdade, ANTES do erro: nem ela é afirmada.
			f.ApagarVersao(itens[10].Key, itens[10].VersionID)
			var n atomic.Int32
			armar(f, &n)
			r := rodar(t, b, itens, 1000)
			if r.Complete || r.Interrupted != Transitorio {
				t.Fatalf("erro passageiro: esperava incompleta/transient, veio complete=%v interrupted=%q", r.Complete, r.Interrupted)
			}
			if len(r.Missing) != 0 || r.MissingTotal != 0 || len(r.SizeMismatch) != 0 || len(r.FoundAgain) != 0 || r.Truncated {
				t.Fatalf("rodada incompleta não leva faltante nenhum: %+v total=%d", r.Missing, r.MissingTotal)
			}
			if r.Detail == "" || r.ListRequests != 3 {
				t.Fatalf("o relato diz o que parou e quanto andou: %q, %d listagens", r.Detail, r.ListRequests)
			}
		})
	}
}

func TestErroPassageiroNaConfirmacaoNaoViraFaltante(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 50, 1, arq)
	itens := catalogo(f)
	f.ApagarVersao(itens[10].Key, itens[10].VersionID)
	f.Recusar(func(op string, _ *http.Request) (int, string) {
		if op == "HeadObjectVersion" {
			return http.StatusServiceUnavailable, "SlowDown"
		}
		return 0, ""
	})
	r := rodar(t, b, itens, 1000)
	if r.Complete || r.Interrupted != Transitorio || r.MissingTotal != 0 || len(r.Missing) != 0 {
		t.Fatalf("o HEAD de confirmação que não respondeu não confirma nada: complete=%v %q %v", r.Complete, r.Interrupted, r.Missing)
	}
}

func TestPermissaoNegadaNoMeioDaListagem(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 3000, 1, arq)
	itens := catalogo(f)
	f.ApagarVersao(itens[10].Key, itens[10].VersionID)
	var n atomic.Int32
	f.Recusar(func(op string, _ *http.Request) (int, string) {
		if op == "ListObjectVersions" && n.Add(1) >= 2 {
			return http.StatusForbidden, "AccessDenied"
		}
		return 0, ""
	})
	r := rodar(t, b, itens, 1000)
	if r.Complete || r.Interrupted != AcessoNegado {
		t.Fatalf("permissão negada: esperava access_denied, veio complete=%v %q", r.Complete, r.Interrupted)
	}
	if len(r.Missing) != 0 || r.MissingTotal != 0 {
		t.Fatalf("listagem negada no meio não gera faltante: %v", r.Missing)
	}
}

func TestCancelamentoInterrompeAListagem(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 5000, 1, arq)
	itens := catalogo(f)
	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()
	var n atomic.Int32
	f.Antes(func(op string, _ *http.Request) {
		if op == "ListObjectVersions" && n.Add(1) == 2 {
			cancelar()
		}
	})
	primeira, proxima := paginas(itens, 1000)
	r, interrompida := Rodar(ctx, Opcoes{Bucket: b, Prefixo: prefixo, Prazo: time.Minute, Proxima: proxima}, primeira)
	if !interrompida {
		t.Fatalf("o serviço parando tem de interromper a rodada sem relato: %+v", r)
	}
	if got := n.Load(); got > 3 {
		t.Fatalf("a listagem seguiu depois do cancelamento: %d requisições", got)
	}
	if r.Complete || len(r.Missing) != 0 {
		t.Fatalf("rodada interrompida não leva relato: %+v", r)
	}
}

// A trava do bucket: pedida por janela, nunca segurada durante a chamada ao
// painel, e com teto de requisições por janela.
func TestTravaPorJanelaESemSegurarDuranteOPainel(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 45_000, 1, arq)
	itens := catalogo(f)
	var dentro atomic.Bool
	var janelas, maiorJanela, naJanela atomic.Int32
	var foraDaTrava, painelComTrava atomic.Int32
	f.Antes(func(op string, _ *http.Request) {
		if op == "GetBucketVersioning" {
			return
		}
		if !dentro.Load() {
			foraDaTrava.Add(1)
		}
		if n := naJanela.Add(1); n > maiorJanela.Load() {
			maiorJanela.Store(n)
		}
	})
	primeira, proximaDoPlano := paginas(itens, 10_000)
	o := Opcoes{
		Bucket: b, Prefixo: prefixo, Prazo: time.Minute, Amostra: 30,
		Janela: func(_ context.Context, fn func()) bool {
			janelas.Add(1)
			naJanela.Store(0)
			dentro.Store(true)
			fn()
			dentro.Store(false)
			return true
		},
		Proxima: func(ctx context.Context, cursor string) (*api.InventoryPlanResponse, error) {
			if dentro.Load() {
				painelComTrava.Add(1)
			}
			return proximaDoPlano(ctx, cursor)
		},
	}
	r, _ := Rodar(context.Background(), o, primeira)
	exigirCompleto(t, r)
	if r.Found != 45_000 {
		t.Fatalf("found=%d", r.Found)
	}
	if painelComTrava.Load() != 0 {
		t.Fatalf("o painel foi chamado %d vez(es) com a trava do bucket segurada", painelComTrava.Load())
	}
	if foraDaTrava.Load() != 0 {
		t.Fatalf("%d requisição(ões) ao bucket fora da trava", foraDaTrava.Load())
	}
	if maiorJanela.Load() > requisicoesPorJanela {
		t.Fatalf("uma janela fez %d requisições (teto %d): o backup esperaria isso tudo", maiorJanela.Load(), requisicoesPorJanela)
	}
	if janelas.Load() < 5 {
		t.Fatalf("45 listagens têm de se dividir em várias janelas, vieram %d", janelas.Load())
	}

	// O plano só cita o fim do prefixo: até chegar lá são 45 listagens de
	// objetos a mais, que não cabem numa janela só.
	janelas.Store(0)
	maiorJanela.Store(0)
	doFim, semMais := paginas(itens[len(itens)-100:], 10_000)
	o.Proxima, o.Amostra = semMais, 0
	r, _ = Rodar(context.Background(), o, doFim)
	exigirCompleto(t, r)
	if r.Found != 100 || r.ExtraVersions != 44_900 {
		t.Fatalf("found=%d a mais=%d", r.Found, r.ExtraVersions)
	}
	if maiorJanela.Load() > requisicoesPorJanela || janelas.Load() < 3 {
		t.Fatalf("atravessar 45 mil objetos a mais tem de soltar a trava no caminho: maior janela=%d requisições, %d janelas", maiorJanela.Load(), janelas.Load())
	}
}

func TestTravaOcupadaAteOPrazo(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 100, 1, arq)
	itens := catalogo(f)
	f.ApagarVersao(itens[3].Key, itens[3].VersionID)
	primeira, proxima := paginas(itens, 1000)
	o := Opcoes{
		Bucket: b, Prefixo: prefixo, Prazo: 150 * time.Millisecond, Proxima: proxima,
		// O backup segura o bucket: a vez não vem até o prazo da rodada.
		Janela: func(ctx context.Context, _ func()) bool {
			<-ctx.Done()
			return false
		},
	}
	r, interrompida := Rodar(context.Background(), o, primeira)
	if interrompida || r.Complete || r.Interrupted != Ocupado {
		t.Fatalf("bucket ocupado até o prazo: esperava busy, veio interrompida=%v complete=%v %q", interrompida, r.Complete, r.Interrupted)
	}
	if f.N("ListObjectVersions") != 0 || len(r.Missing) != 0 {
		t.Fatalf("sem a vez, nada é listado nem afirmado: %d listagens, %v", f.N("ListObjectVersions"), r.Missing)
	}
}

func TestPrazoDaRodada(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 3000, 1, arq)
	itens := catalogo(f)
	f.Antes(func(op string, _ *http.Request) {
		if op == "ListObjectVersions" {
			time.Sleep(80 * time.Millisecond)
		}
	})
	r := rodar(t, b, itens, 1000, func(o *Opcoes) { o.Prazo = 100 * time.Millisecond })
	if r.Complete || r.Interrupted != Prazo || len(r.Missing) != 0 {
		t.Fatalf("passou do prazo: esperava deadline, veio complete=%v %q", r.Complete, r.Interrupted)
	}
}

// O prazo que vence depois de os faltantes já estarem confirmados (durante a
// amostra) ainda deixa a rodada incompleta — e incompleta não leva faltante.
func TestPrazoDuranteAAmostraNaoLevaFaltantes(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 200, 1, arq)
	itens := catalogo(f)
	f.ApagarVersao(itens[10].Key, itens[10].VersionID)
	var heads atomic.Int32
	f.Antes(func(op string, _ *http.Request) {
		// O primeiro HEAD é a confirmação do faltante; os da amostra demoram.
		if op == "HeadObjectVersion" && heads.Add(1) > 1 {
			time.Sleep(60 * time.Millisecond)
		}
	})
	r := rodar(t, b, itens, 1000, func(o *Opcoes) { o.Prazo, o.Amostra = 250*time.Millisecond, 50 })
	if r.Complete || r.Interrupted != Prazo {
		t.Fatalf("esperava a rodada cortada pelo prazo durante a amostra: complete=%v %q (%d HEAD)", r.Complete, r.Interrupted, heads.Load())
	}
	if len(r.Missing) != 0 || r.MissingTotal != 0 || len(r.Sample.Items) != 0 {
		t.Fatalf("rodada incompleta não leva faltante, nem o já confirmado: %+v total=%d amostra=%d", r.Missing, r.MissingTotal, len(r.Sample.Items))
	}
}

// A memória fica na janela: com 300 mil versões, o que a rodada segura não
// cresce com o bucket. Falha se a listagem (ou o plano) for acumulada inteira.
func TestMemoriaLimitadaAJanela(t *testing.T) {
	if testing.Short() {
		t.Skip("monta um bucket de 300 mil versões")
	}
	f, b := novoBucket(t)
	const total = 300_000
	for i := range total {
		f.Semear(prefixo+fmt.Sprintf("dados/pasta-%04d/arquivo-com-nome-comprido-%08d.dat", i%1000, i), bancada.VersaoFalsa{Tamanho: int64(i % 4096)})
	}
	itens := catalogo(f)
	vivo := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := vivo()
	var pico atomic.Uint64
	var n atomic.Int32
	f.Antes(func(op string, _ *http.Request) {
		if op == "ListObjectVersions" && n.Add(1)%25 == 0 {
			if v := vivo(); v > pico.Load() {
				pico.Store(v)
			}
		}
	})
	r := rodar(t, b, itens, 10_000, func(o *Opcoes) { o.Amostra = 200 })
	exigirCompleto(t, r)
	if r.Found != total || r.ListRequests != total/1000 {
		t.Fatalf("found=%d listagens=%d", r.Found, r.ListRequests)
	}
	var cresceu uint64
	if pico.Load() > base {
		cresceu = pico.Load() - base
	}
	// A listagem inteira em memória seriam dezenas de MB (300 mil entradas de
	// ~150 bytes); a janela — uma página do plano, uma da listagem — não chega
	// a 8 MiB.
	const teto = 8 << 20
	t.Logf("memória viva durante a rodada: +%.1f MiB sobre a base (teto %d MiB)", float64(cresceu)/(1<<20), teto>>20)
	if cresceu > teto {
		t.Fatalf("a rodada segurou %.1f MiB a mais durante a listagem (teto %d MiB): a listagem não pode ser acumulada", float64(cresceu)/(1<<20), teto>>20)
	}
}

func TestTamanhoDiferenteEReencontrada(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 20, 1, arq)
	itens := catalogo(f)
	// O catálogo diz outro tamanho para a 4.
	itens[4].Size += 7
	// A 6 e a 8 o painel já tinha por indisponíveis: a 6 está no bucket (volta);
	// a 8 não está (nada de novo); a 9 está, com outro tamanho (segue indisponível).
	itens[6].Unavailable = true
	itens[8].Unavailable = true
	f.ApagarVersao(itens[8].Key, itens[8].VersionID)
	itens[9].Unavailable = true
	itens[9].Size++
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if r.SizeMismatchTotal != 1 || len(r.SizeMismatch) != 1 || r.SizeMismatch[0].Key != itens[4].Key || r.SizeMismatch[0].Size != itens[4].Size-7 {
		t.Fatalf("tamanho diferente: %+v", r.SizeMismatch)
	}
	if len(r.FoundAgain) != 1 || r.FoundAgain[0].Key != itens[6].Key {
		t.Fatalf("a versão indisponível que está no bucket com o tamanho certo vai em found_again: %+v", r.FoundAgain)
	}
	if r.MissingTotal != 0 || len(r.Missing) != 0 {
		t.Fatalf("a versão já indisponível que segue ausente não é faltante novo: %+v", r.Missing)
	}
	if r.Found != 17 {
		t.Fatalf("17 presentes com o tamanho do catálogo, vieram %d", r.Found)
	}
}

func TestAmostraLevaOQueOBucketRespondeu(t *testing.T) {
	f, b := novoBucket(t)
	ate := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	gravado := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 1000 {
		f.Semear(prefixo+arq(i), bancada.VersaoFalsa{Tamanho: 5, SHA256: fmt.Sprintf("%064x", i), TravaModo: "GOVERNANCE", TravaAte: ate, Modificado: gravado})
	}
	f.Semear(prefixo+"sem-metadado.bin", bancada.VersaoFalsa{Tamanho: 5})
	itens := catalogo(f)
	r := rodar(t, b, itens, 400, func(o *Opcoes) { o.Amostra = 5000 })
	exigirCompleto(t, r)
	if r.Sample.Checked != MaxAmostra || len(r.Sample.Items) != MaxAmostra || r.HeadRequests != MaxAmostra {
		t.Fatalf("a amostra tem teto de %d: conferidas=%d itens=%d head=%d", MaxAmostra, r.Sample.Checked, len(r.Sample.Items), r.HeadRequests)
	}
	vistos := map[string]bool{}
	for _, it := range r.Sample.Items {
		if vistos[it.Key] {
			t.Fatalf("item repetido na amostra: %s", it.Key)
		}
		vistos[it.Key] = true
		if it.Key == prefixo+"sem-metadado.bin" {
			if it.SHA256 != "" || it.LockMode != "" {
				t.Fatalf("objeto sem metadado nem retenção: %+v", it)
			}
			continue
		}
		if len(it.SHA256) != 64 || it.LockMode != "GOVERNANCE" || it.RetainUntil == nil || !it.RetainUntil.Equal(ate) || it.LastModified == nil || !it.LastModified.Equal(gravado) {
			t.Fatalf("o item da amostra leva o metadado, a retenção e a data: %+v", it)
		}
	}
	// A amostra se espalha pelo prefixo (não são os 200 primeiros).
	ultimos := 0
	for k := range vistos {
		if k >= prefixo+arq(500) {
			ultimos++
		}
	}
	if ultimos < 50 {
		t.Fatalf("a amostra tem de cobrir o prefixo inteiro: só %d dos 200 vêm da segunda metade", ultimos)
	}
	// HEAD da amostra que não responde é erro DA AMOSTRA: a rodada segue completa.
	f.Recusar(func(op string, _ *http.Request) (int, string) {
		if op == "HeadObjectVersion" {
			return http.StatusServiceUnavailable, "SlowDown"
		}
		return 0, ""
	})
	r = rodar(t, b, itens, 400, func(o *Opcoes) { o.Amostra = 20 })
	exigirCompleto(t, r)
	if r.Sample.Errors != 20 || r.Sample.Checked != 0 || r.MissingTotal != 0 {
		t.Fatalf("amostra sem resposta: erros=%d conferidas=%d faltando=%d", r.Sample.Errors, r.Sample.Checked, r.MissingTotal)
	}
}

// bucketTorto embrulha o Bucket de verdade para simular provedor compatível
// com defeito na listagem.
type bucketTorto struct {
	Bucket
	listar func(pg Pagina) Pagina
}

func (b bucketTorto) Listar(ctx context.Context, p, k, v string) (Pagina, error) {
	pg, err := b.Bucket.Listar(ctx, p, k, v)
	if err != nil {
		return pg, err
	}
	return b.listar(pg), nil
}

func TestListagemQueEscondeVersoesEDesmentidaPeloHead(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 100, 1, arq)
	itens := catalogo(f)
	esconder := func(quantas int) Bucket {
		return bucketTorto{Bucket: b, listar: func(pg Pagina) Pagina {
			var fica []Entrada
			for i, e := range pg.Entradas {
				if i >= 10 && i < 10+quantas {
					continue
				}
				fica = append(fica, e)
			}
			pg.Entradas = fica
			return pg
		}}
	}
	// Três versões que a listagem não trouxe e o HEAD achou: não faltam.
	r := rodar(t, esconder(3), itens, 1000)
	exigirCompleto(t, r)
	if r.MissingTotal != 0 || len(r.Missing) != 0 || r.Unconfirmed != 3 {
		t.Fatalf("versão que o HEAD achou não é faltante: faltando=%d desmentidas=%d", r.MissingTotal, r.Unconfirmed)
	}
	// Muitas: a listagem do provedor não é confiável, e a rodada não vale.
	r = rodar(t, esconder(40), itens, 1000)
	if r.Complete || r.Interrupted != Inconsistente || len(r.Missing) != 0 {
		t.Fatalf("listagem desmentida em massa: esperava listing_inconsistent, veio complete=%v %q", r.Complete, r.Interrupted)
	}
}

func TestListagemForaDeOrdemOuTruncadaSemMarcador(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 100, 1, arq)
	itens := catalogo(f)
	invertida := bucketTorto{Bucket: b, listar: func(pg Pagina) Pagina {
		pg.Entradas[20], pg.Entradas[60] = pg.Entradas[60], pg.Entradas[20]
		return pg
	}}
	r := rodar(t, invertida, itens, 1000)
	if r.Complete || r.Interrupted != ForaDeOrdem || len(r.Missing) != 0 {
		t.Fatalf("listagem fora de ordem de bytes: esperava unordered, veio complete=%v %q %v", r.Complete, r.Interrupted, r.Missing)
	}
	cortada := bucketTorto{Bucket: b, listar: func(pg Pagina) Pagina {
		pg.Entradas = pg.Entradas[:50]
		pg.Truncada, pg.ProximaChave = true, ""
		return pg
	}}
	r = rodar(t, cortada, itens, 1000)
	if r.Complete || r.Interrupted != Inconsistente || len(r.Missing) != 0 || !strings.Contains(r.Detail, "truncada") || r.HeadRequests != 0 {
		t.Fatalf("truncada sem marcador não é fim de listagem (e não se sai perguntando versão por versão): complete=%v %q %q head=%d", r.Complete, r.Interrupted, r.Detail, r.HeadRequests)
	}
}

func TestPlanoQueNaoServe(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 20, 1, arq)
	itens := catalogo(f)

	fora := append([]api.InventoryItem{}, itens...)
	fora[5].Key = "arkame/data/agente-2/dados/de-outro.txt"
	if r := rodar(t, b, fora, 1000); r.Complete || r.Interrupted != ErroDoPlano {
		t.Fatalf("chave fora do prefixo do servidor: esperava plan_error, veio %q", r.Interrupted)
	}
	desordenado := append([]api.InventoryItem{}, itens...)
	desordenado[3], desordenado[12] = desordenado[12], desordenado[3]
	if r := rodar(t, b, desordenado, 1000); r.Complete || r.Interrupted != ErroDoPlano || len(r.Missing) != 0 {
		t.Fatalf("plano fora de ordem: esperava plan_error sem faltantes, veio %q %v", r.Interrupted, r.Missing)
	}
	// O painel não entrega a página seguinte: a rodada não chega ao fim.
	primeira, _ := paginas(itens, 5)
	f.ApagarVersao(itens[0].Key, itens[0].VersionID)
	r, _ := Rodar(context.Background(), Opcoes{Bucket: b, Prefixo: prefixo, Prazo: time.Minute, Proxima: func(context.Context, string) (*api.InventoryPlanResponse, error) {
		return nil, errors.New("503 do painel")
	}}, primeira)
	if r.Complete || r.Interrupted != ErroDoPlano || len(r.Missing) != 0 || r.MissingTotal != 0 {
		t.Fatalf("página do plano que não veio: esperava plan_error sem faltantes, veio %q %v", r.Interrupted, r.Missing)
	}
	// Sem prefixo, o inventário listaria o bucket inteiro: recusa.
	sem, _ := Rodar(context.Background(), Opcoes{Bucket: b, Prefixo: "", Prazo: time.Minute}, &api.InventoryPlanResponse{RunID: "r", Items: itens})
	if sem.Complete || sem.Interrupted != ErroDoPlano || f.N("ListObjectVersions") > 3 {
		t.Fatalf("plano sem prefixo: esperava plan_error sem listar, veio %q", sem.Interrupted)
	}
}

func TestMaisFaltantesQueOTetoVaoCortados(t *testing.T) {
	f, b := novoBucket(t)
	semear(f, 1500, 1, arq)
	itens := catalogo(f)
	for _, it := range itens[:1200] {
		f.ApagarVersao(it.Key, it.VersionID)
	}
	r := rodar(t, b, itens, 1000)
	exigirCompleto(t, r)
	if len(r.Missing) != MaxFaltantes || r.MissingTotal != 1200 || !r.Truncated {
		t.Fatalf("1.200 faltam e a lista leva %d: lista=%d total=%d cortado=%v", MaxFaltantes, len(r.Missing), r.MissingTotal, r.Truncated)
	}
	if r.HeadRequests != MaxFaltantes {
		t.Fatalf("só os da lista são confirmados um a um: %d HEAD", r.HeadRequests)
	}
}

func TestRelatoCabeNoCorpo(t *testing.T) {
	longa := func(i int) string { return prefixo + strings.Repeat("x", 1024-len(prefixo)-6) + fmt.Sprintf("%06d", i) }
	r := api.InventoryResult{RunID: "r", Complete: true, MissingTotal: 1000, DeleteMarkersTotal: 200}
	for i := range 1000 {
		r.Missing = append(r.Missing, api.InventoryVersion{Key: longa(i), VersionID: "v1"})
	}
	for i := range 200 {
		r.DeleteMarkers = append(r.DeleteMarkers, api.InventoryKey{Key: longa(i)})
		r.Sample.Items = append(r.Sample.Items, api.InventorySampleItem{Key: longa(i), VersionID: "v1", SHA256: strings.Repeat("a", 64)})
	}
	Caber(&r, TetoDoCorpo)
	corpo, _ := json.Marshal(r)
	if len(corpo) > TetoDoCorpo {
		t.Fatalf("o relato ficou com %d bytes (teto %d)", len(corpo), TetoDoCorpo)
	}
	if !r.Truncated || len(r.Missing) >= 1000 || len(r.Missing) < 500 || r.MissingTotal != 1000 {
		t.Fatalf("mil faltantes de chave de 1.024 bytes não cabem: a lista é cortada e o relato diz: lista=%d total=%d cortado=%v", len(r.Missing), r.MissingTotal, r.Truncated)
	}
	// O que cabe não é tocado.
	pequeno := api.InventoryResult{RunID: "r", Complete: true, Missing: []api.InventoryVersion{{Key: "k", VersionID: "v"}}, MissingTotal: 1}
	Caber(&pequeno, TetoDoCorpo)
	if pequeno.Truncated || len(pequeno.Missing) != 1 {
		t.Fatalf("relato pequeno foi cortado: %+v", pequeno)
	}
}

func TestLimites(t *testing.T) {
	if p, a := Limites(0, -1); p != PrazoMinimo || a != 0 {
		t.Fatalf("mínimos: %v %d", p, a)
	}
	if p, a := Limites(999_999, 5000); p != PrazoMaximo || a != MaxAmostra {
		t.Fatalf("tetos: %v %d", p, a)
	}
}
