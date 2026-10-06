//go:build !windows

package atualizacao

import "syscall"

// Executar troca este processo pelo programa dado, com os mesmos argumentos
// (o primeiro passa a ser o programa). Só volta se falhar. É como o lançador
// do Docker passa a vez ao programa do volume, e como o programa do volume
// volta ao da imagem: o processo 1 do container continua o mesmo, e nada
// depende da política de reinício do `docker run`.
func Executar(programa string, args, ambiente []string) error {
	argv := append([]string{programa}, args...)
	return syscall.Exec(programa, argv, ambiente)
}
