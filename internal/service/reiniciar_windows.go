//go:build windows

package service

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// RodandoOPrograma lista os serviços do SCM que estão rodando agora o programa
// exe. Lida antes de o setup trocar o exe: depois da troca, esses serviços
// seguem no .old até reiniciar.
func RodandoOPrograma(exe string) []string {
	if exe == "" {
		return nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return nil
	}
	defer m.Disconnect()
	todos, err := m.ListServices()
	if err != nil {
		return nil
	}
	var nomes []string
	for _, n := range todos {
		if servicoNoSCMChamaPrograma(m, n, exe) && servicoRodando(m, n) {
			nomes = append(nomes, n)
		}
	}
	return nomes
}

// servicoRodando diz se o serviço nome está Running. Abre só com
// SERVICE_QUERY_STATUS, como o servicoNoSCMChamaPrograma.
func servicoRodando(m *mgr.Mgr, nome string) bool {
	p, err := windows.UTF16PtrFromString(nome)
	if err != nil {
		return false
	}
	h, err := windows.OpenService(m.Handle, p, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	s := &mgr.Service{Name: nome, Handle: h}
	defer s.Close()
	st, err := s.Query()
	return err == nil && st.State == svc.Running
}

// Reiniciar para o serviço nome, espera parar e o inicia de novo — com o
// programa que estiver no caminho registrado agora.
func Reiniciar(nome string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(nome)
	if err != nil {
		return err
	}
	defer s.Close()
	pararEEsperar(s)
	if st, err := s.Query(); err != nil {
		return err
	} else if st.State != svc.Stopped {
		return fmt.Errorf("o serviço não parou em 60s")
	}
	return s.Start()
}
