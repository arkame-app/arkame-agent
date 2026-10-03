//go:build windows

package restore

import "os"

// copiarDono: no Windows o dono vem da ACL herdada da pasta.
func copiarDono(string, os.FileInfo) error { return nil }
