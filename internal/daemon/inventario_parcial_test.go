package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Sessão em modo `full` que fecha parcial (uma pasta ilegível): no painel os
// arquivos presentes passam a ser exatamente o inventário que veio — os da
// pasta que não foi lida ficam FORA desta sessão. O estado local não pode
// seguir afirmando que o painel os tem: na sessão seguinte, em delta, eles
// têm de voltar ao índice, mesmo sem ter mudado.
func TestFullParcialNaoDeixaArquivoForaDoCatalogoNoDeltaSeguinte(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("precisa de uma pasta sem permissão de leitura")
	}
	m := novaMesaDoDaemon(t)
	m.semear(45)
	m.painel.SemDelta = true // o painel só aceita inventário completo (organização não convertida, prefixo trocado)
	m.rodarOK()
	trancada := filepath.Join(m.raiz, "p03")
	if err := os.Chmod(trancada, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trancada, 0o755) })
	s := m.rodarOK()
	if s.Status != "partial" || s.Modo != "full" || s.Presentes != 40 {
		t.Fatalf("full parcial: %+v", s)
	}
	_ = os.Chmod(trancada, 0o755)
	m.painel.SemDelta = false
	m.s3.Zerar()
	s = m.rodarOK()
	if s.Status != "complete" || s.Modo != "delta" || s.Presentes != 45 {
		t.Fatalf("delta depois do full parcial: %+v", s)
	}
	if s.Entradas != 5 || m.s3.N("PutObject") != 0 {
		t.Fatalf("só os 5 arquivos da pasta voltam ao índice, sem reenvio: entradas=%d s3=%v", s.Entradas, m.s3.Requisicoes())
	}
	m.conferir()
}
