//go:build windows

package sync

import "io/fs"

// ligacaoDeArquivoFechado: no Windows o hardlink exige escrita no arquivo, e
// a conferência fica com o caminho real.
func ligacaoDeArquivoFechado(fs.FileInfo, int64) bool { return false }

func donoDaPasta(string) int64 { return -1 }
