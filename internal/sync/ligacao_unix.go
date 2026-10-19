//go:build !windows

package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ligacaoDeArquivoFechado: um hardlink (mais de um nome) para um arquivo que
// os outros não leem, de um dono diferente do da pasta onde o nome está. No
// macOS não há protected_hardlinks, e `ln /etc/ssh/ssh_host_ed25519_key
// ~/Documentos/x` (ou a chave de uma conta de serviço, como _postgres) punha o
// arquivo num plano do MCP de ~/Documentos: o caminho do arquivo aberto é o
// nome inocente (revisão, 18/10). O store do pnpm (arquivos do próprio
// usuário, legíveis) passa. donoDaPasta < 0: não se sabe, e só o root conta.
func ligacaoDeArquivoFechado(st fs.FileInfo, donoDaPasta int64) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Nlink <= 1 || st.Mode().Perm()&0o004 != 0 {
		return false
	}
	// Do root, sempre: a pasta do nome também pode ser do root e aberta a
	// todos (/Users/Shared, /private/tmp).
	return s.Uid == 0 || donoDaPasta >= 0 && int64(s.Uid) != donoDaPasta
}

// donoDaPasta: o dono da pasta onde está o nome do arquivo, ou -1.
func donoDaPasta(arquivo string) int64 {
	st, err := os.Stat(filepath.Dir(arquivo))
	if err != nil {
		return -1
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(s.Uid)
	}
	return -1
}
