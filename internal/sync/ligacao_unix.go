//go:build !windows

package sync

import (
	"io/fs"
	"syscall"
)

// ligacaoDeArquivoFechado: um hardlink (mais de um nome) para um arquivo do
// root que os outros não leem. No macOS não há protected_hardlinks, e
// `ln /etc/ssh/ssh_host_ed25519_key ~/Documentos/x` punha a chave do host
// num plano do MCP de ~/Documentos: o caminho do arquivo aberto é o nome
// inocente (revisão, 18/10). O arquivo comum com vários nomes (o store do
// pnpm) é do usuário e passa.
func ligacaoDeArquivoFechado(st fs.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Nlink > 1 && s.Uid == 0 && st.Mode().Perm()&0o004 == 0
}
