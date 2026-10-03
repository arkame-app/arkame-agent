//go:build !windows && !linux && !darwin

package service

import "errors"

// RodandoOPrograma: sem gerenciador de serviços conhecido nesta plataforma, a
// lista é vazia.
func RodandoOPrograma(string) []string { return nil }

// Reiniciar: ver RodandoOPrograma.
func Reiniciar(string) error {
	return errors.New("sem gerenciador de serviços conhecido nesta plataforma")
}
