//go:build windows

package hooks

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// comandoDoShell roda o comando no cmd.exe com a linha montada à mão: com os
// argumentos do Go, as aspas internas chegavam ao cmd como \" e o formato mais
// comum no Windows quebrava — `"C:\Program Files\PostgreSQL\16\bin\pg_dump.exe"
// -f "C:\bk\dump.sql"`. `/s /c "…"`: o cmd tira só as aspas de fora.
func comandoDoShell(comando string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	cmd := exec.Command(comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + comspec + `" /d /s /c "` + comando + `"`}
	return cmd
}

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
