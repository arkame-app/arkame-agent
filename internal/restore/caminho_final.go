package restore

import "strings"

// Conferência do caminho real (GetFinalPathNameByHandle) no Windows. Vive sem
// build tag, em funções puras, para ter teste em qualquer sistema: as regras
// de comparação são o que decide se uma gravação desviada passa.

// semPrefixoLongo tira o prefixo que o GetFinalPathNameByHandle põe:
// `\\?\C:\x` vira `C:\x` e `\\?\UNC\srv\share\x` vira `\\srv\share\x`.
func semPrefixoLongo(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`):
		return p[len(`\\?\`):]
	}
	return p
}

// normalizarWindows deixa o caminho comparável: sem o prefixo longo, com `\`
// e sem `\` no fim.
func normalizarWindows(p string) string {
	return strings.TrimRight(semPrefixoLongo(strings.ReplaceAll(p, "/", `\`)), `\`)
}

// mesmoCaminhoWindows: a e b são o mesmo caminho do Windows (que não
// diferencia maiúsculas), com ou sem o prefixo longo e a barra final.
func mesmoCaminhoWindows(a, b string) bool {
	return strings.EqualFold(normalizarWindows(a), normalizarWindows(b))
}

// arquivoNaPastaWindows: o caminho real de um arquivo (o do handle) é o nome
// dado direto dentro da pasta esperada (o caminho real dela, conferido sem
// link). Um link ou uma junção trocados no caminho depois da conferência
// levam o arquivo para outro lugar, e o caminho real o denuncia.
func arquivoNaPastaWindows(real, pasta, nome string) bool {
	if nome == "" || strings.ContainsAny(nome, `\/`) {
		return false
	}
	return mesmoCaminhoWindows(real, normalizarWindows(pasta)+`\`+nome)
}
