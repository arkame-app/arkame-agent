package cli

import "golang.org/x/sys/windows"

// espacoLivreEm: os bytes livres do disco da pasta.
func espacoLivreEm(p string) (uint64, bool) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, false
	}
	var livre, total, livreTotal uint64
	if windows.GetDiskFreeSpaceEx(u, &livre, &total, &livreTotal) != nil {
		return 0, false
	}
	return livre, true
}
