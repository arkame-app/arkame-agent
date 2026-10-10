//go:build !linux

package atualizacao

import (
	"errors"
	"os"
)

// ExecutarAberto: o lançador é do Docker, que é Linux — só lá há
// /proc/self/fd para executar pelo descritor.
func ExecutarAberto(*os.File, string, []string, []string) error {
	return errors.New("sem lançador fora do Linux")
}
