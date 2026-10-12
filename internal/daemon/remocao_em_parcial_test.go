package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// trancar deixa um arquivo que mudou e não dá para ler: a sessão fecha
// parcial, como no servidor que sempre tem um arquivo assim.
func trancar(t *testing.T, p string) {
	t.Helper()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("precisa de um arquivo ilegível")
	}
	if err := os.WriteFile(p, []byte("mudou e trancou"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
}

// O caso do contrato, item 27: num servidor em que toda sessão fecha parcial,
// o arquivo apagado na origem sai do catálogo — o agente tem a prova positiva
// (o arquivo não existe e a pasta dele foi listada) e o painel ofereceu
// `removed_partial`. O arquivo ilegível segue presente.
func TestSessaoParcialEmiteRemovedComProvaPositiva(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(30)
	s := m.rodarOK()
	if !slices.Contains(s.Capacidades, api.CapacidadeRemovidosEmParcial) {
		t.Fatalf("o agente declara a capacidade no /start: %v", s.Capacidades)
	}
	if s.OfertaRemovidosEmParcial {
		t.Fatal("este teste supõe que a primeira sessão (sem delta) não leva a oferta")
	}

	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-00001.txt")); err != nil {
		t.Fatal(err)
	}
	trancar(t, filepath.Join(m.raiz, "p02/arquivo-00002.txt"))

	s = m.rodarOK()
	if s.Status != "partial" || s.Modo != "delta" || !s.OfertaRemovidosEmParcial {
		t.Fatalf("%+v", s)
	}
	if s.Entradas != 1 || s.Presentes != 29 || s.RemovidosIgnorados != 0 || s.RemocoesSemX != 0 {
		t.Fatalf("queria 1 entrada (o `removed`), 29 presentes e nada ignorado: entradas=%d presentes=%d ignorados=%d sem base=%d",
			s.Entradas, s.Presentes, s.RemovidosIgnorados, s.RemocoesSemX)
	}
	cat := m.painel.Catalogo("ag1", "p1")
	if _, tem := cat[m.chave("p01/arquivo-00001.txt")]; tem {
		t.Fatal("o arquivo apagado segue no catálogo")
	}
	if _, tem := cat[m.chave("p02/arquivo-00002.txt")]; !tem {
		t.Fatal("o arquivo ilegível saiu do catálogo: não ler não é prova de que saiu")
	}
	if st := s.Complete["stats"].(map[string]any); st["files_removed"] != float64(1) {
		t.Fatalf("files_removed = %v", st["files_removed"])
	}

	// A sessão parcial seguinte não repete a remoção (o painel a confirmou).
	s = m.rodarOK()
	if s.Status != "partial" || s.Modo != "delta" || s.Entradas != 0 || s.Presentes != 29 {
		t.Fatalf("seguinte: %+v", s)
	}

	// Uma pasta inteira apagada também sai: a pasta acima dela foi listada, e
	// ela não está mais lá.
	if err := os.RemoveAll(filepath.Join(m.raiz, "p03")); err != nil {
		t.Fatal(err)
	}
	s = m.rodarOK()
	if s.Status != "partial" || s.Modo != "delta" || s.Entradas != 3 || s.Presentes != 26 {
		t.Fatalf("pasta apagada: queria 3 `removed` e 26 presentes: %+v", s)
	}

	// Destrancado, a sessão completa fecha o que faltava e o catálogo é o disco.
	if err := os.Chmod(filepath.Join(m.raiz, "p02/arquivo-00002.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = m.rodarOK()
	if s.Status != "complete" || s.Modo != "delta" || s.Entradas != 1 || s.Presentes != 26 {
		t.Fatalf("completa: %+v", s)
	}
	m.conferir()
}

// Pasta que existe e não pôde ser listada não prova nada do que está abaixo
// dela: os arquivos seguem no catálogo, mesmo com a oferta.
func TestPastaIlegivelNaoProvaRemocao(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("precisa de uma pasta ilegível")
	}
	m := novaMesaDoDaemon(t)
	m.semear(30)
	m.rodarOK()
	// p04 fica sem permissão de leitura: o que há dentro não é visto.
	pasta := filepath.Join(m.raiz, "p04")
	if err := os.Chmod(pasta, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pasta, 0o755) })
	// E um arquivo de outra pasta sai de verdade.
	if err := os.Remove(filepath.Join(m.raiz, "p05/arquivo-00005.txt")); err != nil {
		t.Fatal(err)
	}
	s := m.rodarOK()
	if s.Status != "partial" || s.Modo != "delta" || !s.OfertaRemovidosEmParcial {
		t.Fatalf("%+v", s)
	}
	if s.Entradas != 1 || s.Presentes != 29 {
		t.Fatalf("só o arquivo apagado sai (1 entrada, 29 presentes): entradas=%d presentes=%d", s.Entradas, s.Presentes)
	}
	cat := m.painel.Catalogo("ag1", "p1")
	for _, rel := range []string{"p04/arquivo-00004.txt", "p04/arquivo-00013.txt", "p04/arquivo-00022.txt"} {
		if _, tem := cat[m.chave(rel)]; !tem {
			t.Fatalf("%s está atrás de uma pasta que não abriu e saiu do catálogo", rel)
		}
	}
}

// Contra o painel que não oferece `removed_partial` (o de antes), o agente
// novo não muda: sessão parcial sem `removed`, e a remoção sai na primeira
// sessão completa.
func TestSemAOfertaASessaoParcialNaoEmiteRemoved(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.painel.SemRemovidosEmParcial = true
	m.semear(30)
	m.rodarOK()
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-00001.txt")); err != nil {
		t.Fatal(err)
	}
	trancado := filepath.Join(m.raiz, "p02/arquivo-00002.txt")
	trancar(t, trancado)
	s := m.rodarOK()
	if s.OfertaRemovidosEmParcial || s.Status != "partial" || s.Modo != "delta" || s.Entradas != 0 || s.Presentes != 30 {
		t.Fatalf("%+v", s)
	}
	if err := os.Chmod(trancado, 0o644); err != nil {
		t.Fatal(err)
	}
	s = m.rodarOK()
	if s.Status != "complete" || s.Modo != "delta" || s.Entradas != 2 || s.Presentes != 29 {
		t.Fatalf("completa (1 upsert + 1 removed): %+v", s)
	}
	m.conferir()
}
