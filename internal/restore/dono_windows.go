//go:build windows

package restore

import "os"

// copiarDono: no Windows o dono vem da ACL herdada da pasta.
func copiarDono(*os.File, os.FileInfo) error { return nil }
