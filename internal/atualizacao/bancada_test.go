package atualizacao

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// A bancada dos testes da troca: um par de chaves de teste, um "release"
// servido por um servidor local (o programa, o checksums.txt e a assinatura) e
// uma pasta com o programa atual. O programa é um script de shell que responde
// ao self-test — executado de verdade, como a troca faz com o programa novo.

// chavesDeTesteGeradas é um par gerado para o teste; nunca a de produção.
func chavesDeTesteGeradas(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// programaFalso é um "arkame-agent" que só sabe o self-test: imprime a versão
// e sai com o código dado.
func programaFalso(versao string, codigo int) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\n# arkame-agent falso %s\necho %s%s\nexit %d\n", versao, LinhaDaVersao, versao, codigo))
}

func resumoDe(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// release é o que o servidor local serve em /v<versão>/.
type release struct {
	mu         sync.Mutex
	versao     string
	programa   []byte
	checksums  []byte
	assinatura []byte
	// servir troca a resposta de um arquivo (nil: o conteúdo acima).
	servir  map[string]http.HandlerFunc
	pedidos []string
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.pedidos = append(r.pedidos, req.URL.Path)
	nome := strings.TrimPrefix(req.URL.Path, "/v"+r.versao+"/")
	h := r.servir[nome]
	var corpo []byte
	switch nome {
	case ArquivoDeChecksums:
		corpo = r.checksums
	case ArquivoDaAssinatura:
		corpo = r.assinatura
	case NomeNoRelease():
		corpo = r.programa
	}
	r.mu.Unlock()
	if h != nil {
		h(w, req)
		return
	}
	if corpo == nil {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(corpo)
}

// bancada é uma instalação de teste: o programa atual numa pasta, o release
// novo no servidor e o pedido que o painel mandaria.
type bancada struct {
	t       *testing.T
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	rel     *release
	srv     *httptest.Server
	lugar   Lugar
	atual   []byte
	pedido  Pedido
	opcoes  Opcoes
	versaoV Versao
}

// novaBancada monta a troca da versão 0.4.31 para a 0.5.0, toda certa. Cada
// teste estraga uma coisa.
func novaBancada(t *testing.T) *bancada {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("o programa falso é um script de shell")
	}
	pub, priv := chavesDeTesteGeradas(t)
	v, _ := LerVersao("0.5.0")
	programa := programaFalso("0.5.0", 0)
	checksums := []byte(resumoDe(programa) + "  " + NomeNoRelease() + "\n" +
		"0000000000000000000000000000000000000000000000000000000000000000  outro-arquivo.tar.gz\n")
	rel := &release{
		versao:     "0.5.0",
		programa:   programa,
		checksums:  checksums,
		assinatura: Assinar(priv, v, checksums),
		servir:     map[string]http.HandlerFunc{},
	}
	srv := httptest.NewServer(rel)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	atual := programaFalso("0.4.31", 0)
	caminho := filepath.Join(dir, "arkame-agent")
	if err := os.WriteFile(caminho, atual, 0o755); err != nil {
		t.Fatal(err)
	}
	return &bancada{
		t: t, pub: pub, priv: priv, rel: rel, srv: srv,
		lugar: LugarDe(caminho), atual: atual, versaoV: v,
		pedido: Pedido{Versao: "0.5.0", Sha256: resumoDe(programa), BaseURL: srv.URL},
		opcoes: Opcoes{
			Programa:        caminho,
			VersaoAtual:     "0.4.31",
			Chaves:          []ed25519.PublicKey{pub},
			ArgsDoAutoTeste: []string{"self-test"},
		},
	}
}

func (b *bancada) aplicar() (*Resultado, error) {
	return Aplicar(context.Background(), b.pedido, b.opcoes)
}

// trocarPrograma muda o programa do release e refaz checksums e assinatura,
// como um release de verdade.
func (b *bancada) trocarPrograma(programa []byte) {
	b.rel.mu.Lock()
	defer b.rel.mu.Unlock()
	b.rel.programa = programa
	b.rel.checksums = []byte(resumoDe(programa) + "  " + NomeNoRelease() + "\n")
	b.rel.assinatura = Assinar(b.priv, b.versaoV, b.rel.checksums)
	b.pedido.Sha256 = resumoDe(programa)
}

// exigirFalha confere que a troca falhou com o código dado e que nada mudou:
// o programa atual é o mesmo, não sobrou programa novo nem registro de troca.
func (b *bancada) exigirFalha(err error, codigo string) {
	b.t.Helper()
	var f *Falha
	if !errors.As(err, &f) {
		b.t.Fatalf("queria a falha %q, veio %v", codigo, err)
	}
	if f.Codigo != codigo {
		b.t.Fatalf("falha %q (%v), queria %q", f.Codigo, f.Err, codigo)
	}
	b.exigirIntacto()
}

func (b *bancada) exigirIntacto() {
	b.t.Helper()
	noLugar, err := os.ReadFile(b.lugar.Atual)
	if err != nil {
		b.t.Fatalf("o programa atual sumiu: %v", err)
	}
	if string(noLugar) != string(b.atual) {
		b.t.Fatalf("o programa atual foi trocado apesar da falha:\n%s", noLugar)
	}
	for _, resto := range []string{b.lugar.Novo, b.lugar.Anterior, b.lugar.Estado, b.lugar.Trava} {
		if _, err := os.Lstat(resto); err == nil {
			b.t.Fatalf("sobrou %s depois da falha", resto)
		}
	}
}

func (b *bancada) noLugar() string {
	b.t.Helper()
	c, err := os.ReadFile(b.lugar.Atual)
	if err != nil {
		b.t.Fatal(err)
	}
	return string(c)
}

func (b *bancada) estado() Estado {
	b.t.Helper()
	e, ok := b.lugar.lerEstado()
	if !ok {
		b.t.Fatal("sem registro da troca")
	}
	return e
}

// executarQueMarca é um auto-teste que só anota que foi chamado.
func executarQueMarca(chamado *bool) func(context.Context, string, ...string) ([]byte, error) {
	return func(context.Context, string, ...string) ([]byte, error) {
		*chamado = true
		return []byte(LinhaDaVersao + "0.5.0\n"), nil
	}
}
