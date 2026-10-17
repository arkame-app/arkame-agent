package daemon

import "golang.org/x/sys/windows"

// espacoDoDisco: os bytes livres e o tamanho do disco da pasta.
func espacoDoDisco(pasta string) (livre, total uint64, ok bool) {
	if pasta == "" {
		return 0, 0, false
	}
	p, err := windows.UTF16PtrFromString(pasta)
	if err != nil {
		return 0, 0, false
	}
	var disponivel, tamanho, livreTotal uint64
	if windows.GetDiskFreeSpaceEx(p, &disponivel, &tamanho, &livreTotal) != nil {
		return 0, 0, false
	}
	return disponivel, tamanho, true
}
