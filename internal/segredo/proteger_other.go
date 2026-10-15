//go:build !windows

package segredo

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func protegerComModo(caminho string, modo os.FileMode) error {
	return os.Chmod(caminho, modo)
}

// protegerPasta: o 0700 do MkdirAll já basta.
func protegerPasta(string) error { return nil }

// donoConfiavel: o root e o usuário deste processo.
func donoConfiavel(caminho string) (confiavel bool, dono string, err error) {
	fi, err := os.Stat(caminho)
	if err != nil {
		return false, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, "", fmt.Errorf("sem informação de dono")
	}
	uid := int(st.Uid)
	return uid == 0 || uid == os.Geteuid(), "uid " + strconv.Itoa(uid), nil
}

// pastaDeVerdade: uma pasta, ou um link para uma (o administrador pode ter
// posto o estado em outro disco por um link; o dono conferido é o do destino).
func pastaDeVerdade(p string, _ os.FileInfo) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s não é uma pasta", p)
	}
	return nil
}

// permitirEscrita só existe no Windows: fora dele o comando de antes roda como
// root e grava o dump ele mesmo.
func permitirEscrita(dir, escritor string) error {
	return fmt.Errorf("--writer só existe no Windows (a conta %q não foi aplicada a %s)", escritor, dir)
}

// criarPastaFechada cria a pasta com 0700.
func criarPastaFechada(p string) error { return os.Mkdir(p, 0o700) }

// semLinkAcima: fora do Windows um link no caminho é escolha do
// administrador (o estado em outro disco), e o dono conferido é o do destino.
func semLinkAcima(string) error { return nil }
