//go:build !windows

package service

import "context"

// RunAsService só faz sentido no Windows. Nos demais sistemas o supervisor
// (systemd, launchd) fala com o processo por sinais, então nunca há o que
// interceptar e o chamador segue direto para a execução comum.
func RunAsService(_ string, _ func(context.Context) error) (handled bool, err error) {
	return false, nil
}
