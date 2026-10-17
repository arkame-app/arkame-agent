package zimbra

import "errors"

// O Zimbra não roda no Windows: sem medida, a guarda do disco fica desligada.
func espacoNoDiscoDoSistema(string) (livre, total uint64, err error) {
	return 0, 0, errors.New("sem medida de disco no Windows")
}
