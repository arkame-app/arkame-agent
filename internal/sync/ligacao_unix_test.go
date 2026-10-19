//go:build !windows

package sync

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

type infoFalso struct {
	modo fs.FileMode
	sys  *syscall.Stat_t
}

func (i infoFalso) Name() string       { return "x" }
func (i infoFalso) Size() int64        { return 1 }
func (i infoFalso) Mode() fs.FileMode  { return i.modo }
func (i infoFalso) ModTime() time.Time { return time.Time{} }
func (i infoFalso) IsDir() bool        { return false }
func (i infoFalso) Sys() any           { return i.sys }

// Hardlink para arquivo do root fechado (a chave do host) fica de fora do
// plano do MCP; o do usuário com vários nomes (pnpm) e o legível passam.
func TestLigacaoDeArquivoFechado(t *testing.T) {
	casos := []struct {
		nome  string
		modo  fs.FileMode
		links int
		uid   uint32
		quer  bool
	}{
		{"chave do host com dois nomes", 0o600, 2, 0, true},
		{"chave do host com um nome", 0o600, 1, 0, false},
		{"arquivo do usuário com dois nomes", 0o600, 2, 1000, false},
		{"arquivo do root legível com dois nomes", 0o644, 2, 0, false},
	}
	for _, c := range casos {
		st := &syscall.Stat_t{Uid: c.uid}
		st.Nlink = 1
		if c.links > 1 {
			st.Nlink = 2
		}
		if got := ligacaoDeArquivoFechado(infoFalso{modo: c.modo, sys: st}); got != c.quer {
			t.Errorf("%s: %v, queria %v", c.nome, got, c.quer)
		}
	}
}
