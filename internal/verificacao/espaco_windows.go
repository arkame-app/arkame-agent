//go:build windows

package verificacao

import "golang.org/x/sys/windows"

// espacoLivre devolve os bytes livres para o usuário do agente no volume da
// pasta.
func espacoLivre(pasta string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(pasta)
	if err != nil {
		return 0, err
	}
	var livre, total, totalLivre uint64
	if err := windows.GetDiskFreeSpaceEx(p, &livre, &total, &totalLivre); err != nil {
		return 0, err
	}
	return livre, nil
}
