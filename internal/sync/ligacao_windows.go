//go:build windows

package sync

import "io/fs"

type pastaDoNome struct{}

// ligacaoDeArquivoFechado: no Windows o hardlink exige escrita no arquivo, e
// a conferência fica com o caminho real.
func ligacaoDeArquivoFechado(fs.FileInfo, pastaDoNome) bool { return false }

func donoDaPasta(string) pastaDoNome { return pastaDoNome{} }
