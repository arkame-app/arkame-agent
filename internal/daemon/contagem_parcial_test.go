package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/arkame-app/agent/internal/estado"
	syncengine "github.com/arkame-app/agent/internal/sync"
)

// arquivosNoEstado é a contagem de arquivos que o estado local guarda do
// plano — a base do teto da reconferência rotativa.
func (m *mesaDoDaemon) arquivosNoEstado() int64 {
	m.t.Helper()
	b, err := estado.Abrir(m.cfg.StateDir, identidadeDoEstado(m.cfg, m.plano.StorageRef.Bucket))
	if err != nil {
		m.t.Fatal(err)
	}
	defer b.Fechar()
	p, err := b.Plano(m.plano.ID, syncengine.PrefixoDasChaves("data/"+m.cfg.AgentID+"/"))
	if err != nil {
		m.t.Fatal(err)
	}
	return p.Arquivos()
}

// Servidor em que toda sessão é parcial (achado A2 da revisão: um arquivo
// ilegível que nunca sai): a contagem de arquivos do plano ficava em zero para
// sempre, e o teto da reconferência rotativa, no piso de 1.000 por execução —
// com 1 milhão de arquivos, um ciclo de ~1.000 dias.
func TestSessaoParcialAtualizaAContagemDeArquivos(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de um arquivo ilegível")
	}
	m := novaMesaDoDaemon(t)
	m.semear(45)
	trancado := filepath.Join(m.raiz, "p02/arquivo-00002.txt")
	if err := os.Chmod(trancado, 0o000); err != nil {
		t.Fatal(err)
	}
	if s := m.rodarOK(); s.Status != "partial" {
		t.Fatalf("%+v", s)
	}
	if n := m.arquivosNoEstado(); n != 44 {
		t.Fatalf("depois de uma sessão parcial a contagem é a dos arquivos vistos (44); está em %d", n)
	}
	// Sessão parcial que viu menos (uma pasta inteira ilegível) não derruba a
	// contagem; a completa, sim, a acerta.
	pasta := filepath.Join(m.raiz, "p03")
	if err := os.Chmod(pasta, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pasta, 0o755) })
	if s := m.rodarOK(); s.Status != "partial" {
		t.Fatalf("%+v", s)
	}
	if n := m.arquivosNoEstado(); n != 44 {
		t.Fatalf("parcial que viu menos não derruba a contagem: %d", n)
	}
	_ = os.Chmod(pasta, 0o755)
	_ = os.Chmod(trancado, 0o644)
	_ = os.Remove(filepath.Join(m.raiz, "p04/arquivo-00004.txt"))
	_ = os.Remove(filepath.Join(m.raiz, "p05/arquivo-00005.txt"))
	if s := m.rodarOK(); s.Status != "complete" {
		t.Fatalf("%+v", s)
	}
	if n := m.arquivosNoEstado(); n != 43 {
		t.Fatalf("a sessão completa acerta a contagem (45 − 2 removidos): %d", n)
	}
}
