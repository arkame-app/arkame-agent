//go:build windows

package service

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// arkameService adapta o loop do daemon ao protocolo do Service Control
// Manager. Sem isso o Windows encerra o processo com o erro 1053 ("o serviço
// não respondeu a tempo"), porque um binário comum nunca reporta estado ao SCM.
type arkameService struct {
	run func(context.Context) error
}

func (s *arkameService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case err := <-done:
			changes <- svc.Status{State: svc.Stopped}
			if err != nil {
				return false, 1
			}
			return false, 0
		}
	}
}

// RunAsService roda `run` sob o SCM quando o processo foi iniciado como serviço
// do Windows. Retorna handled=false quando é execução normal de terminal, e aí
// o chamador segue o caminho comum.
func RunAsService(name string, run func(context.Context) error) (handled bool, err error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, nil
	}
	if name == "" {
		name = DefaultName
	}
	return true, svc.Run(name, &arkameService{run: run})
}
