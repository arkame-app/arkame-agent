//go:build linux

package service

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Até a v0.4.3 o install aceitava qualquer nome (backup-oci). O uninstall de
// outro agente só via as units arkame-agent* e apagava o programa de que
// backup-oci depende. A unit que chama o mesmo programa conta como agente.
func TestUnitsDoAgentePeloPrograma(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "arkame-agent")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "atalho")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(dir, "units")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	escrever := func(nome, exec string) {
		t.Helper()
		u := "[Service]\nExecStart=" + exec + " run --config /etc/arkame/x.env\n"
		if err := os.WriteFile(filepath.Join(units, nome+".service"), []byte(u), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escrever("arkame-agent", quoteArg(bin))
	escrever("backup-oci", quoteArg(bin))
	escrever("pelo-atalho", "-"+link)
	escrever("nginx", "/usr/sbin/nginx")

	got := unitsDoAgente([]string{units}, bin)
	slices.Sort(got)
	quer := []string{"arkame-agent", "backup-oci", "pelo-atalho"}
	if !slices.Equal(got, quer) {
		t.Fatalf("units do agente = %v, queria %v", got, quer)
	}
	if o := outrosEntre("linux", got, "arkame-agent"); !slices.Contains(o, "backup-oci") {
		t.Fatalf("backup-oci não conta como outro agente: %v", o)
	}
	// Sem saber o próprio programa, só o prefixo.
	if got := unitsDoAgente([]string{units}, ""); !slices.Equal(got, []string{"arkame-agent"}) {
		t.Fatalf("sem programa: %v", got)
	}
}

// A unit com o nome legado conta como existente só se chama este programa.
func TestUnitEmChamaPrograma(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "arkame-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(dir, "units")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	for nome, exec := range map[string]string{"backup-oci": bin, "nginx": "/usr/sbin/nginx"} {
		u := "[Service]\nExecStart=" + exec + " run --config /etc/arkame/x.env\n"
		if err := os.WriteFile(filepath.Join(units, nome+".service"), []byte(u), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{filepath.Join(dir, "vazia"), units}
	if !unitEmChamaPrograma(dirs, "backup-oci", bin) {
		t.Fatal("backup-oci chama o programa")
	}
	if unitEmChamaPrograma(dirs, "nginx", bin) || unitEmChamaPrograma(dirs, "nao-existe", bin) || unitEmChamaPrograma(dirs, "backup-oci", "") {
		t.Fatal("só a unit que chama o programa conta")
	}
}
