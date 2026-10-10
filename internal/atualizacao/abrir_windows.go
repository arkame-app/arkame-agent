//go:build windows

package atualizacao

// semSeguirLink: o Windows não tem O_NOFOLLOW. A abertura exclusiva
// (O_CREATE|O_EXCL vira CREATE_NEW) já falha com qualquer coisa no caminho,
// link inclusive — e o criarNovo apaga o que estiver lá antes.
const semSeguirLink = 0
