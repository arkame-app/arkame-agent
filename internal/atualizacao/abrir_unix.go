//go:build !windows

package atualizacao

import "syscall"

// semSeguirLink: a abertura falha se o último componente do caminho é um link
// simbólico.
const semSeguirLink = syscall.O_NOFOLLOW
