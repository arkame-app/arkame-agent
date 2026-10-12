package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// Tentativas na finalização (achado B2): o contrato (item 10) diz "mais 5
// tentativas POR LOTE". A conta não zerava a cada lote aceito: com o painel
// instável, o terceiro lote já entrava sem tentativa nenhuma e o backup
// falhava com `complete_failed` depois de os arquivos terem subido.
func TestTentativasDaFinalizacaoSaoPorLote(t *testing.T) {
	recuosCurtos(t)
	antes := recuoMaximoDoLote
	recuoMaximoDoLote = time.Millisecond
	t.Cleanup(func() { recuoMaximoDoLote = antes })

	const falhasPorLote = 4 // menos que as 5 tentativas a que cada lote tem direito
	var mu sync.Mutex
	falhas := map[string]int{}
	aceitos := 0
	var env *enviadorDeLotes
	pronto := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-pronto
		// Só responde com a finalização já em curso: as falhas de antes dela
		// não têm limite e não fariam parte do que o teste mede.
		for i := 0; i < 2000; i++ {
			env.mu.Lock()
			fechando := env.fechando
			env.mu.Unlock()
			if fechando {
				break
			}
			time.Sleep(time.Millisecond)
		}
		var corpo strings.Builder
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		corpo.Write(buf[:n])
		seq := corpo.String()[:strings.Index(corpo.String(), ",")]
		mu.Lock()
		defer mu.Unlock()
		if falhas[seq] < falhasPorLote {
			falhas[seq]++
			http.Error(w, "fora do ar", http.StatusServiceUnavailable)
			return
		}
		aceitos++
		_, _ = w.Write([]byte(`{"ok":true,"seq":` + strconv.Itoa(aceitos) + `}`))
	}))
	t.Cleanup(srv.Close)
	c, err := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	if err != nil {
		t.Fatal(err)
	}
	env, err = novoEnviadorDeLotes(context.Background(), c, "/files", &api.IndexOffer{Mode: "batches", MaxEntries: 100}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.fechar)
	close(pronto)
	for i := 0; i < 300; i++ {
		if err := env.Emitir(api.IndexEntry{Op: api.OpRemoved, Key: "k" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	lotes, entradas, err := env.finalizar(context.Background())
	if err != nil || lotes != 3 || entradas != 300 {
		t.Fatalf("cada lote tinha direito a 5 tentativas e precisou de %d: lotes=%d entradas=%d err=%v", falhasPorLote+1, lotes, entradas, err)
	}
}

// E o limite por lote continua valendo: o lote que falha além das 5 tentativas
// derruba a finalização.
func TestLoteQueNaoChegaEmCincoTentativasFalha(t *testing.T) {
	recuosCurtos(t)
	antes := recuoMaximoDoLote
	recuoMaximoDoLote = time.Millisecond
	t.Cleanup(func() { recuoMaximoDoLote = antes })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fora do ar", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	env, err := novoEnviadorDeLotes(context.Background(), c, "/files", &api.IndexOffer{Mode: "batches"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.fechar)
	_ = env.Emitir(api.IndexEntry{Op: api.OpRemoved, Key: "k"})
	if _, _, err := env.finalizar(context.Background()); err == nil {
		t.Fatal("painel fora do ar até o fim: a finalização tem de falhar")
	}
}
