//go:build linux || darwin

package service

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// executar roda um comando e devolve a saída combinada. Variável para os
// testes trocarem o systemctl e o launchctl de verdade.
var executar = func(ctx context.Context, nome string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, nome, args...).CombinedOutput()
}

// RodandoOPrograma lista os serviços do agente (systemd do sistema e do
// usuário; launchd do sistema e do usuário) que estão rodando agora o
// programa exe. Lida antes de trocar o programa: o processo em execução
// segue com o arquivo antigo (o mv troca o inode) até reiniciar.
func RodandoOPrograma(exe string) []string {
	if exe == "" {
		return nil
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return doPrograma(runtime.GOOS, registrados(), exe, rodandoNoSO)
}

// folgaDoReinicio é o que prazoDoReinicio soma a EsperaParada: o systemctl
// restart e o launchctl kickstart -k esperam o serviço parar (até
// TimeoutStopSec/ExitTimeOut, que são EsperaParada) e ainda o sobem de novo.
const folgaDoReinicio = 30 * time.Second

// prazoDoReinicio é quanto Reiniciar espera o systemctl/launchctl voltar.
// Com 2 min fixos, abaixo dos 150s de EsperaParada, um daemon que demorava a
// fechar um backup fazia o comando ser morto enquanto o job de restart seguia
// no gerenciador — o serviço subia no programa novo e o setup dizia que ele
// continuava na versão antiga.
func prazoDoReinicio() time.Duration {
	return EsperaParada + folgaDoReinicio
}

// Reiniciar reinicia o serviço nome no escopo em que ele está registrado
// (systemctl [--user] restart; launchctl kickstart -k), e ele sobe com o
// programa que estiver no caminho registrado agora.
func Reiniciar(nome string) error {
	s, ok := escolherRegistro(runtime.GOOS, registrados(), nome, rodandoNoSO)
	if !ok {
		s = servicoRegistrado{nome: nome, escopo: defaultScope()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), prazoDoReinicio())
	defer cancel()
	args := restartArgs(s.nome, s.escopo)
	if out, err := executar(ctx, args[0], args[1:]...); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
