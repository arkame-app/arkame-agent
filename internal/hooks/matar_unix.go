//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// grupoProprio coloca o comando no próprio grupo de processos.
//
// Sem isto, matar o comando mata só o `/bin/sh`. Um hook de verdade quase nunca
// é um processo só — `pg_dump … | gzip > arquivo` são três —, e os que sobram
// continuam segurando o pipe de saída aberto, o que faz o `Wait` esperar por
// eles mesmo depois de o shell morrer.
func grupoProprio(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// matarArvore derruba o grupo inteiro. O PID negativo é o grupo.
//
// SIGKILL e não SIGTERM: o prazo já estourou, e um comando que ignora o pedido
// educado é exatamente o que precisamos interromper.
func matarArvore(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
