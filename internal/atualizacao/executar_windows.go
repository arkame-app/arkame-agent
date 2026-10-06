//go:build windows

package atualizacao

import "errors"

// Executar: o lançador é do Docker, que é Linux. No Windows quem religa o
// serviço é o SCM.
func Executar(string, []string, []string) error {
	return errors.New("sem lançador no Windows")
}
