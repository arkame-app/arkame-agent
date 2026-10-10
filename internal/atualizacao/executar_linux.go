//go:build linux

package atualizacao

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// CaminhoDoAberto é o caminho pelo qual o kernel alcança o arquivo de um
// descritor deste processo: /proc/self/fd/N. Executar por ele é executar
// aquele arquivo, e não o que estiver, naquele instante, no caminho de onde
// ele foi aberto.
func CaminhoDoAberto(f *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", f.Fd())
}

// ExecutarAberto troca este processo pelo programa do descritor, já aberto e
// conferido (AbrirDoVolume). argv0 é o nome com que o programa se vê (o
// caminho do volume). Só volta se falhar.
//
// O descritor é close-on-exec, como todo arquivo do Go, e isso não atrapalha:
// o kernel resolve o caminho e abre o programa antes de fechar os descritores.
func ExecutarAberto(f *os.File, argv0 string, args, ambiente []string) error {
	argv := append([]string{argv0}, args...)
	err := syscall.Exec(CaminhoDoAberto(f), argv, ambiente)
	runtime.KeepAlive(f)
	return err
}
