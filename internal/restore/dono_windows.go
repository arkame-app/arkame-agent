//go:build windows

package restore

import "os"

// copiarDono: no Windows o dono vem da ACL herdada da pasta.
func copiarDono(*os.File, int, int) error { return nil }
