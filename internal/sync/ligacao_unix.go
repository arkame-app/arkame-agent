//go:build !windows

package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// pastaDoNome: o dono da pasta onde está o nome do arquivo, e se ela é
// aberta à escrita de todos. dono < 0: não se sabe.
type pastaDoNome struct {
	dono   int64
	aberta bool
}

// ligacaoDeArquivoFechado: um hardlink (mais de um nome) para um arquivo que
// os outros não leem, posto numa pasta que não é do dono dele. No macOS não
// há protected_hardlinks, e `ln /etc/ssh/ssh_host_ed25519_key
// /Users/Shared/x` (ou a chave de uma conta de serviço, como _postgres) punha
// o arquivo num plano do MCP: o caminho do arquivo aberto é o nome inocente
// (revisão, 18/10). Uma pasta aberta à escrita de todos (/Users/Shared,
// /tmp) não serve de prova de dono. O backup por hardlink (rsnapshot,
// --link-dest), em que pasta e arquivo têm o mesmo dono, e o store do pnpm
// (arquivos legíveis) passam.
func ligacaoDeArquivoFechado(st fs.FileInfo, pasta pastaDoNome) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Nlink <= 1 || st.Mode().Perm()&0o004 != 0 {
		return false
	}
	if pasta.dono < 0 {
		return s.Uid == 0
	}
	return int64(s.Uid) != pasta.dono || pasta.aberta
}

func donoDaPasta(arquivo string) pastaDoNome {
	st, err := os.Stat(filepath.Dir(arquivo))
	if err != nil {
		return pastaDoNome{dono: -1}
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return pastaDoNome{dono: -1}
	}
	return pastaDoNome{dono: int64(s.Uid), aberta: st.Mode().Perm()&0o002 != 0}
}
