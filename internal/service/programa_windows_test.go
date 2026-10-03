//go:build windows

package service

import (
	"strings"
	"testing"
)

// O caminho inteiro é conferido, com o papel certo: o programa, a pasta dele
// e as de cima.
func TestConferirProgramaPercorreOCaminhoNoWindows(t *testing.T) {
	antesACL, antesLinks, antesAdm := lerACL, resolverLinks, adminDoInstall
	t.Cleanup(func() { lerACL, resolverLinks, adminDoInstall = antesACL, antesLinks, antesAdm })
	adminDoInstall = func() []string { return nil }
	resolverLinks = func(p string) (string, error) { return p, nil }
	adm := aclDeArquivo{dono: sidAdministradores, aces: []aceDeArquivo{{mascara: 0x1200a9, sid: "S-1-5-32-545"}}}
	arvore := map[string]aclDeArquivo{
		`C:\`:                     adm,
		`C:\Program Files`:        adm,
		`C:\Program Files\Arkame`: adm,
		`C:\Program Files\Arkame\arkame-agent.exe`: adm,
		`C:\Users`:               adm,
		`C:\Users\ana`:           {dono: "S-1-5-21-1-2-3-1001"},
		`C:\Users\ana\Downloads`: {dono: "S-1-5-21-1-2-3-1001"},
		`C:\Users\ana\Downloads\arkame-agent.exe`: {dono: "S-1-5-21-1-2-3-1001"},
	}
	lerACL = func(p string) (aclDeArquivo, error) { return arvore[p], nil }
	if err := conferirPrograma(`C:\Program Files\Arkame\arkame-agent.exe`); err != nil {
		t.Fatalf("Program Files recusado: %v", err)
	}
	err := conferirPrograma(`C:\Users\ana\Downloads\arkame-agent.exe`)
	if err == nil || !strings.Contains(err.Error(), "Program Files") {
		t.Fatalf("Downloads da ana aceito, ou sem orientar: %v", err)
	}
}
