package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// Com instantâneo, os arquivos são lidos da cópia congelada, e a chave no
// bucket continua a do disco: quem restaura vê a pasta que marcou no plano.
// A pasta do estado local é ignorada também dentro da cópia.
func TestWalkLeDoInstantaneoComAChaveDoDisco(t *testing.T) {
	base := t.TempDir()
	vivo := filepath.Join(base, "vivo")
	congelado := filepath.Join(base, "congelado")
	for _, d := range []string{vivo, filepath.Join(congelado, "estado")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	escrever := func(p, conteudo string) {
		if err := os.WriteFile(p, []byte(conteudo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escrever(filepath.Join(vivo, "a.txt"), "gravando agora")
	escrever(filepath.Join(congelado, "a.txt"), "como estava")
	escrever(filepath.Join(congelado, "so-na-copia.txt"), "x")
	escrever(filepath.Join(congelado, "estado", "estado.db"), "x")

	ler := func(p string) string {
		if resto, ok := strings.CutPrefix(p, vivo); ok {
			return congelado + resto
		}
		return p
	}
	arquivos, erros := walkSem(context.Background(), "/", []string{vivo}, nil, nil,
		[]string{filepath.Join(vivo, "estado")}, nil, ler)
	lidos := map[string]string{}
	for f := range arquivos {
		lidos[f.RelativePath] = f.AbsolutePath
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	chave := strings.TrimPrefix(filepath.ToSlash(vivo), "/")
	queria := map[string]string{
		chave + "/a.txt":           filepath.Join(congelado, "a.txt"),
		chave + "/so-na-copia.txt": filepath.Join(congelado, "so-na-copia.txt"),
	}
	if len(lidos) != len(queria) {
		t.Fatalf("lidos = %v; queria %v", lidos, queria)
	}
	for k, abs := range queria {
		if lidos[k] != abs {
			t.Fatalf("chave %q lida de %q; queria %q (lidos: %v)", k, lidos[k], abs, lidos)
		}
	}
}

// O instantâneo que some no meio da sessão (a área de cópias de sombra lotou)
// faz tudo "não existir": nada disso pode virar remoção no catálogo. A sessão
// que seria inventário completo deixa de ser, e não sai `removed`.
func TestInstantaneoQueSumiuNaoViraRemocao(t *testing.T) {
	m := novaMesa(t)
	m.semear(20)
	m.rodar()
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
		t.Fatal(err)
	}
	m.viva = func() error { return os.ErrNotExist }
	r, em := m.rodar()
	if r.InventarioCompleto {
		t.Fatal("com o instantâneo perdido a sessão não pode ser inventário completo")
	}
	if rem := chavesDoOp(em, api.OpRemoved); len(rem) != 0 {
		t.Fatalf("remoção emitida com o instantâneo perdido: %v", rem)
	}
	// Com ele vivo, a mesma falta é remoção.
	m.viva = func() error { return nil }
	r, em = m.rodar()
	if !r.InventarioCompleto || len(chavesDoOp(em, api.OpRemoved)) != 1 {
		t.Fatalf("com o instantâneo vivo a remoção tinha de sair: completo=%v %+v", r.InventarioCompleto, em)
	}
}

func chavesDoOp(es []api.IndexEntry, op string) []string {
	var out []string
	for _, e := range es {
		if e.Op == op {
			out = append(out, e.Key)
		}
	}
	return out
}
