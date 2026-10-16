//go:build windows

package localstore

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// interromper: no Windows não há sinal para um processo de console sem
// janela; o RustFS é encerrado direto. Os objetos já gravados ficam; a escrita
// em curso é a que o agente vai repetir.
func interromper(p *os.Process) error { return p.Kill() }

// semSeguirLink: no Windows o log fica na pasta da configuração, fechada para
// Administradores e SYSTEM; não há O_NOFOLLOW.
const semSeguirLink = 0

// prenderAoAgente põe o RustFS num Job Object que encerra o que está dentro
// quando o último handle fecha — o do agente, que morre com ele. Um serviço
// morto à força (taskkill, falha) não deixa o rustfs.exe órfão segurando a
// porta e a pasta.
func prenderAoAgente(p *os.Process) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	// Fechar o handle encerra o RustFS: só depois de ele já ter saído.
	return func() { windows.CloseHandle(job) }, nil
}
