//go:build !windows

package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// mesaParcial monta um plano com uma sessão confirmada e deixa um arquivo que
// mudou e não dá para ler: dali em diante toda sessão fecha parcial, como no
// servidor que sempre tem um arquivo assim. O "painel" oferece
// `removed_partial`.
func mesaParcial(t *testing.T) *mesa {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root lê qualquer arquivo")
	}
	m := novaMesa(t)
	m.removParcial = true
	m.semear(40)
	m.rodar()
	trancado := filepath.Join(m.raiz, "p02/arquivo-0002.txt")
	if err := os.WriteFile(trancado, []byte("mudou e trancou"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancado, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancado, 0o644) })
	return m
}

func chaves(es []api.IndexEntry, op string) []string {
	var out []string
	for _, e := range es {
		if e.Op == op {
			out = append(out, e.Key)
		}
	}
	return out
}

// Sessão parcial, com a oferta: sai `removed` do arquivo que não existe mais
// e cuja pasta foi listada; do ilegível, nada.
func TestParcialEmiteRemovedDoQueTemProva(t *testing.T) {
	m := mesaParcial(t)
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
		t.Fatal(err)
	}
	r, em := m.rodar()
	if r.InventarioCompleto || r.FilesFailed != 1 {
		t.Fatalf("a sessão tinha de ser parcial: completo=%v falhas=%d", r.InventarioCompleto, r.FilesFailed)
	}
	if rem := chaves(em, api.OpRemoved); len(rem) != 1 || rem[0] != m.chave("p01/arquivo-0001.txt") || len(em) != 1 {
		t.Fatalf("queria só o `removed` do arquivo apagado: %+v", em)
	}
	if r.FilesRemoved != 1 || r.FilesUnselected != 0 {
		t.Fatalf("removidos=%d fora da seleção=%d", r.FilesRemoved, r.FilesUnselected)
	}
	// Confirmada, a remoção não sai de novo.
	if _, em = m.rodar(); len(em) != 0 {
		t.Fatalf("a remoção confirmada saiu de novo: %+v", em)
	}
}

// A remoção que o painel não confirmou sai de novo na sessão seguinte, parcial
// ou completa: a lápide só some com a confirmação.
func TestRemovedDeParcialNaoConfirmadaSaiDeNovo(t *testing.T) {
	m := mesaParcial(t)
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
		t.Fatal(err)
	}
	m.confirmar = false
	if _, em := m.rodar(); len(chaves(em, api.OpRemoved)) != 1 {
		t.Fatalf("primeira: %+v", em)
	}
	m.confirmar = true
	if _, em := m.rodar(); len(chaves(em, api.OpRemoved)) != 1 || len(em) != 1 {
		t.Fatalf("a remoção não confirmada tinha de sair de novo: %+v", em)
	}
	if _, em := m.rodar(); len(em) != 0 {
		t.Fatalf("confirmada, não sai mais: %+v", em)
	}
}

// O que NÃO é prova: cada caso deixa um arquivo sem ser visto, e nenhum pode
// virar `removed`.
func TestParcialSemProvaNaoEmiteRemoved(t *testing.T) {
	t.Run("a pasta existe e não pôde ser listada", func(t *testing.T) {
		m := mesaParcial(t)
		pasta := filepath.Join(m.raiz, "p03")
		if err := os.Chmod(pasta, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(pasta, 0o755) })
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("pasta ilegível virou remoção: %+v", em)
		}
	})
	t.Run("a pasta não lista e o lstat do arquivo diz que não existe", func(t *testing.T) {
		// O pior caso: a pasta deixou de abrir E o que está abaixo dela
		// responde "não existe" (montagem que caiu). Sem a pasta listada,
		// não há prova.
		m := mesaParcial(t)
		pasta := filepath.Join(m.raiz, "p03")
		if err := os.Chmod(pasta, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(pasta, 0o755) })
		antes := lstatDaProva
		lstatDaProva = func(p string) (fs.FileInfo, error) {
			if strings.HasPrefix(p, pasta+string(filepath.Separator)) {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.ENOENT}
			}
			return antes(p)
		}
		t.Cleanup(func() { lstatDaProva = antes })
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("arquivo atrás de pasta não listada virou remoção: %+v", em)
		}
	})
	t.Run("o lstat falha por outro motivo", func(t *testing.T) {
		m := mesaParcial(t)
		alvo := filepath.Join(m.raiz, "p01/arquivo-0001.txt")
		if err := os.Remove(alvo); err != nil {
			t.Fatal(err)
		}
		antes := lstatDaProva
		lstatDaProva = func(p string) (fs.FileInfo, error) {
			if p == alvo {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.EIO}
			}
			return antes(p)
		}
		t.Cleanup(func() { lstatDaProva = antes })
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("erro de leitura no lstat virou remoção: %+v", em)
		}
	})
	t.Run("o arquivo virou outra coisa", func(t *testing.T) {
		m := mesaParcial(t)
		alvo := filepath.Join(m.raiz, "p01/arquivo-0001.txt")
		if err := os.Remove(alvo); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(alvo, 0o644); err != nil {
			t.Skipf("sem fifo neste sistema: %v", err)
		}
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("o caminho existe (não é mais arquivo comum) e virou remoção: %+v", em)
		}
	})
	t.Run("saiu da seleção do plano", func(t *testing.T) {
		// `unselected` segue só na varredura completa; e o arquivo excluído
		// que também foi apagado não vira `removed`.
		m := mesaParcial(t)
		if err := os.Remove(filepath.Join(m.raiz, "p04/arquivo-0004.txt")); err != nil {
			t.Fatal(err)
		}
		// Excluído pelo nome: a pasta dele segue listada, e sem a regra da
		// seleção o lstat + a pasta listada dariam "prova".
		m.exclusoes = []string{"arquivo-0004.txt"}
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("fora da seleção em sessão parcial: %+v", em)
		}
	})
	t.Run("sem a oferta do painel", func(t *testing.T) {
		m := mesaParcial(t)
		m.removParcial = false
		if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
			t.Fatal(err)
		}
		if _, em := m.rodar(); len(em) != 0 {
			t.Fatalf("sem `removed_partial` a sessão parcial não emite remoção: %+v", em)
		}
	})
}

// A pasta inteira apagada: a pasta-mãe dos arquivos não existe mais, e a prova
// é a pasta listada acima dela, que não a mostra.
func TestParcialPastaApagadaEProva(t *testing.T) {
	m := mesaParcial(t)
	m.escrever("fundo/a/b/c.txt", "c")
	m.escrever("fundo/a/d.txt", "d")
	m.escrever("fundo/e.txt", "e")
	if _, em := m.rodar(); len(chaves(em, api.OpUpsert)) != 3 {
		t.Fatalf("os três arquivos novos: %+v", em)
	}
	if err := os.RemoveAll(filepath.Join(m.raiz, "fundo/a")); err != nil {
		t.Fatal(err)
	}
	_, em := m.rodar()
	rem := chaves(em, api.OpRemoved)
	if len(rem) != 2 || len(em) != 2 {
		t.Fatalf("queria `removed` dos dois arquivos sob a pasta apagada: %+v", em)
	}
	for _, k := range rem {
		if !strings.Contains(k, "/fundo/a/") {
			t.Fatalf("remoção de arquivo fora da pasta apagada: %s", k)
		}
	}
}

// A prova, sem o disco: o que conta como "saiu" para cada desenho de pastas
// listadas.
func TestPastasListadasSaiu(t *testing.T) {
	existe := map[string]bool{}
	antes := lstatDaProva
	lstatDaProva = func(p string) (fs.FileInfo, error) {
		if existe[filepath.ToSlash(p)] {
			return nil, nil
		}
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: syscall.ENOENT}
	}
	t.Cleanup(func() { lstatDaProva = antes })

	nova := func(listadas ...string) *pastasListadas {
		p := novasPastasListadas()
		p.fonte("dados", "/srv/dados")
		for _, l := range listadas {
			p.entrou(l)
		}
		return p
	}
	for _, c := range []struct {
		nome     string
		listadas []string
		existem  []string
		rel      string
		saiu     bool
	}{
		{"pasta-mãe listada, arquivo não existe", []string{"dados", "dados/x"}, []string{"/srv/dados/x"}, "dados/x/f.txt", true},
		{"arquivo existe", []string{"dados", "dados/x"}, []string{"/srv/dados/x", "/srv/dados/x/f.txt"}, "dados/x/f.txt", false},
		{"pasta-mãe não listada e existe", []string{"dados"}, []string{"/srv/dados/x"}, "dados/x/f.txt", false},
		{"pasta-mãe apagada, a de cima listada", []string{"dados"}, nil, "dados/x/f.txt", true},
		{"duas pastas apagadas, a raiz listada", []string{"dados"}, nil, "dados/x/y/f.txt", true},
		{"pasta do meio existe e não foi listada", []string{"dados"}, []string{"/srv/dados/x"}, "dados/x/y/f.txt", false},
		{"a pasta do plano não foi listada", nil, nil, "dados/f.txt", false},
		{"arquivo na raiz do plano", []string{"dados"}, nil, "dados/f.txt", true},
		{"chave fora das pastas do plano", []string{"dados"}, nil, "outra/f.txt", false},
		{"chave que é a própria pasta do plano", []string{"dados"}, nil, "dados", false},
	} {
		clear(existe)
		for _, e := range c.existem {
			existe[e] = true
		}
		if got := nova(c.listadas...).saiu(c.rel); got != c.saiu {
			t.Errorf("%s: saiu(%q) = %v, queria %v", c.nome, c.rel, got, c.saiu)
		}
	}

	// A listagem que falhou tira a pasta, mesmo já marcada (o WalkDir avisa
	// do erro depois de ter entrado nela).
	p := nova("dados", "dados/x")
	p.falhou("dados/x")
	existe["/srv/dados/x"] = true
	if p.saiu("dados/x/f.txt") {
		t.Error("pasta cuja listagem falhou serviu de prova")
	}

	// Chave sob duas pastas do plano: a prova tem de valer nas duas.
	clear(existe)
	p = novasPastasListadas()
	p.fonte("dados", "/srv/dados")
	p.fonte("dados/x", "/mnt/x")
	p.entrou("dados")
	p.entrou("dados/x")
	if !p.saiu("dados/x/f.txt") {
		t.Error("não existe por nenhum dos dois caminhos: saiu")
	}
	existe["/mnt/x/f.txt"] = true
	if p.saiu("dados/x/f.txt") {
		t.Error("existe por um dos caminhos e foi dado por removido")
	}

	// A pasta do plano que é a raiz do servidor (chave vazia).
	clear(existe)
	p = novasPastasListadas()
	p.fonte(".", "/")
	p.entrou(".")
	if !p.saiu("f.txt") {
		t.Error("arquivo na raiz do servidor, pasta listada: saiu")
	}
	if nilo := (*pastasListadas)(nil); nilo.saiu("dados/f.txt") {
		t.Error("sem as pastas anotadas não há prova")
	}
}
