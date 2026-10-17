//go:build !windows

package sync

import (
	"errors"
	"io/fs"
)

// Fora do Windows não há reparse point: irregularLegivel nem chega aqui.
func lerReparseDoSistema(string) (uint32, uint32, error) {
	return 0, 0, errors.New("reparse point só existe no Windows")
}

// Fora do Windows, ELOOP/ENOTDIR/ENAMETOOLONG já cobrem o link sem alvo.
var errosLinkSemAlvoDoSistema []error

// Fora do Windows não há arquivo de nuvem do Cloud Files API.
func atributosDoArquivo(fs.FileInfo) (uint32, bool) { return 0, false }

func erroDeArquivoDeNuvem(error) bool { return false }
