package caminho

import (
	"os"
	"path/filepath"
	"testing"
)

// O caminho real decide: um link inocente para /etc é recusado, e as pastas
// comuns passam.
func TestPastaSensivelPeloCaminhoReal(t *testing.T) {
	if Windows {
		t.Skip("links simbólicos do Unix")
	}
	raiz := t.TempDir()
	for _, d := range []string{"etc/ssh", "srv/dados", "home/ana/.ssh", "var/log"} {
		if err := os.MkdirAll(filepath.Join(raiz, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(raiz, "srv", "inocente")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../home/ana/.ssh", filepath.Join(raiz, "srv", "chaves")); err != nil {
		t.Fatal(err)
	}
	casos := map[string]bool{
		"/srv/inocente":     true, // link para /etc, que contém /etc/ssh
		"/srv/inocente/ssh": true,
		"/srv/chaves":       true, // link relativo para .ssh
		"/etc":              true,
		"/":                 true,
		"/root":             true,
		"/srv/dados":        false,
		"/var/log":          false,
		"/home":             false,
		"/var":              false,
	}
	for origem, quer := range casos {
		if got := PastaSensivel(raiz, origem) != ""; got != quer {
			t.Errorf("%s: sensível=%v, queria %v", origem, got, quer)
		}
	}
}

func TestPastaSensivelNoWindows(t *testing.T) {
	casos := map[string]bool{
		`C:\Windows\System32\config`: true,
		`C:\Windows`:                 true,
		`C:\`:                        true,
		`D:\`:                        false,
		`D:\etc\arkame`:              true,
		`C:\Users\ana\.ssh`:          true,
		`C:\Users\ana\Documents`:     false,
	}
	for p, quer := range casos {
		if got := sensivelNoWindows(p) != ""; got != quer {
			t.Errorf("%s: sensível=%v, queria %v", p, got, quer)
		}
	}
}

// No macOS o caminho real de /etc é /private/etc: a lista vale com e sem o
// prefixo (revisão de segurança, 18/10).
func TestSensivelComPrefixoPrivate(t *testing.T) {
	casos := map[string]bool{
		"/private/etc/sudoers.d":       true,
		"/private/etc/master.passwd":   true,
		"/private/etc/letsencrypt/x":   true,
		"/private/etc/ssl/private":     true,
		"/private/var/root":            true,
		"/private/var/db/dslocal/node": true,
		"/private":                     true,
		"/private/tmp":                 false,
		"/private/var/log":             false,
	}
	for p, quer := range casos {
		if got := sensivelNoUnix(p); got != quer {
			t.Errorf("%s: sensível=%v, queria %v", p, got, quer)
		}
	}
}

func TestResolvidoNoWindows(t *testing.T) {
	casos := map[string]bool{
		`C:\dados\x`: true,
		`D:/x`:       true,
		`\\srv\x`:    true,
		`\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy3\dados\x`: false,
		`\\.\PhysicalDrive0`: false,
		`C:`:                 false,
		`dados`:              false,
	}
	for p, quer := range casos {
		if got := ResolvidoNoWindows(p); got != quer {
			t.Errorf("%s: %v, queria %v", p, got, quer)
		}
	}
}
