//go:build !windows

package sync

import "errors"

// Fora do Windows não há reparse point: irregularLegivel nem chega aqui.
func lerReparseDoSistema(string) (uint32, uint32, error) {
	return 0, 0, errors.New("reparse point só existe no Windows")
}
