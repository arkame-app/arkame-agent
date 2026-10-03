//go:build windows

package service

// conferirPrograma: no Windows o setup copia o programa para Program Files,
// que só o administrador grava. Nada a conferir aqui.
func conferirPrograma(string) error { return nil }
