//go:build !windows

package service

import "errors"

// RodandoOPrograma: só o Windows troca o programa com o serviço rodando o
// antigo (.old); fora dele o setup não roda, e a lista é vazia.
func RodandoOPrograma(string) []string { return nil }

// Reiniciar: ver RodandoOPrograma.
func Reiniciar(string) error { return errors.New("só no Windows") }
