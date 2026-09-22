//go:build windows

package hooks

import (
	"os/exec"
	"strconv"
)

// grupoProprio não tem equivalente direto no Windows; a árvore é derrubada pelo
// `taskkill /T` em `matarArvore`.
func grupoProprio(_ *exec.Cmd) {}

// matarArvore usa o `taskkill` porque `Process.Kill` derruba só o `cmd.exe`, e
// os netos continuariam segurando o pipe de saída.
func matarArvore(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
