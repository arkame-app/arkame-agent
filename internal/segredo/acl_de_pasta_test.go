package segredo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// As ACEs que se acham nas pastas de verdade do Windows, e o que cada uma
// significa para a pasta do dump.
func TestAceQueEntrega(t *testing.T) {
	const (
		usuariosAutenticados = "S-1-5-11"
		modificar            = 0x1301bf // inclui DELETE; não inclui FILE_DELETE_CHILD
		controleTotal        = 0x1f01ff
		leituraEExecucao     = 0x1200a9
		criarPastas          = 0x4
	)
	confiavel := func(sid string) bool { return sidsConfiaveisFixos[sid] }
	casos := []struct {
		nome    string
		ace     aceLida
		raiz    bool
		entrega bool
	}{
		{"C:\\Dumps criada pelo Explorer: Usuários Autenticados com modificar (herdado da raiz)", aceLida{Tipo: aceDePermitir, Marcas: 0x10, Mascara: modificar, SID: usuariosAutenticados}, false, true},
		{"a mesma ACE na raiz, só para herdar: não vale para a raiz", aceLida{Tipo: aceDePermitir, Marcas: 0x0b, Mascara: modificar, SID: usuariosAutenticados}, true, false},
		{"modificar na raiz: a raiz não se renomeia, e modificar não apaga o que está dentro", aceLida{Tipo: aceDePermitir, Mascara: modificar, SID: usuariosAutenticados}, true, false},
		{"controle total na raiz: apaga (renomeia) o que está dentro", aceLida{Tipo: aceDePermitir, Mascara: controleTotal, SID: usuariosAutenticados}, true, true},
		{"criar pastas na raiz (o padrão do C:)", aceLida{Tipo: aceDePermitir, Mascara: criarPastas, SID: usuariosAutenticados}, true, false},
		{"leitura e execução para Usuários (o padrão do ProgramData)", aceLida{Tipo: aceDePermitir, Mascara: leituraEExecucao, SID: "S-1-5-32-545"}, false, false},
		{"Usuários podem gravar e criar pastas no ProgramData, sem apagar", aceLida{Tipo: aceDePermitir, Mascara: 0x100116, SID: "S-1-5-32-545"}, false, false},
		{"mudar a permissão, só isso", aceLida{Tipo: aceDePermitir, Mascara: direitoMudarDACL, SID: "S-1-1-0"}, false, true},
		{"mudar o dono, só isso, na raiz", aceLida{Tipo: aceDePermitir, Mascara: direitoMudarDono, SID: "S-1-1-0"}, true, true},
		{"GENERIC_ALL explícito", aceLida{Tipo: aceDePermitir, Mascara: direitoTudo, SID: "S-1-1-0"}, false, true},
		{"controle total para SYSTEM, Administradores e TrustedInstaller", aceLida{Tipo: aceDePermitir, Mascara: controleTotal, SID: sidDoTrustedInstaller}, false, false},
		{"ACE condicional dá o mesmo direito", aceLida{Tipo: aceDePermitirCondicao, Mascara: modificar, SID: usuariosAutenticados}, false, true},
		{"ACE de negar não entrega nada", aceLida{Tipo: 1, Mascara: controleTotal, SID: usuariosAutenticados}, false, false},
	}
	for _, c := range casos {
		entrega, err := aceQueEntrega(c.ace, c.raiz, confiavel)
		if err != nil || entrega != c.entrega {
			t.Errorf("%s: entrega=%v err=%v; esperado %v", c.nome, entrega, err, c.entrega)
		}
	}
	if _, err := aceQueEntrega(aceLida{Tipo: aceDePermitirDeObjeto, Mascara: leituraEExecucao}, false, confiavel); err == nil {
		t.Error("ACE de objeto (SID noutra posição) tem de ser recusada, não lida como se fosse a comum")
	}
}

// Fora do Windows: a pasta acima em que outro usuário escreve (sem o sticky
// bit) é recusada; a com sticky bit, como o /tmp, não.
func TestPastaDeDumpRecusaPastaAcimaQueOutrosAlteram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Windows a regra é a da DACL (TestAceQueEntrega)")
	}
	base := t.TempDir()
	aberta := filepath.Join(base, "aberta")
	if err := os.Mkdir(aberta, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(aberta, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := PastaDeDump(filepath.Join(aberta, "sql"), ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta acima gravável por todos aceita: %v", err)
	}
	if _, err := os.Stat(filepath.Join(aberta, "sql")); !os.IsNotExist(err) {
		t.Fatalf("a pasta do dump não podia ter sido criada: %v", err)
	}
	if err := os.Chmod(aberta, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := PastaDeDump(filepath.Join(aberta, "sql"), ""); err != nil {
		t.Fatalf("pasta acima com sticky bit (como o /tmp) recusada: %v", err)
	}
	// E quando a aberta é a de cima da de cima: a pasta já criada não salva.
	if err := os.Chmod(aberta, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := PastaDeDump(filepath.Join(aberta, "sql"), ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta do dump já existente sob uma pasta aberta aceita: %v", err)
	}
}

// A conferência antes de criar alcança a pasta alheia que fica acima da
// primeira que existe: a PastaPrivada só olha essa primeira, e sem a
// conferência de antes a pasta do dump seria criada e só depois recusada
// (revisão do S14, 7ª passada: sem teste, mover a linha passava).
func TestPastaDeDumpConfereAcimaAntesDeCriar(t *testing.T) {
	base := t.TempDir()
	alheia := filepath.Join(base, "alheia")
	existe := filepath.Join(alheia, "existe")
	if err := os.MkdirAll(existe, 0o755); err != nil {
		t.Fatal(err)
	}
	donoFalso(t, alheia)
	if err := PastaDeDump(filepath.Join(existe, "novo", "sql"), ""); !errors.Is(err, ErrDonoNaoConfiavel) {
		t.Fatalf("pasta acima de outro dono aceita: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existe, "novo")); !os.IsNotExist(err) {
		t.Fatalf("nada podia ter sido criado sob a pasta alheia: %v", err)
	}
}

func TestComPrefixoLongo(t *testing.T) {
	casos := map[string]string{
		`C:\ProgramData\Arkame\dumps\sqlserver`: `\\?\C:\ProgramData\Arkame\dumps\sqlserver`,
		`\\srv\share\dumps`:                     `\\?\UNC\srv\share\dumps`,
		`\\?\C:\x`:                              `\\?\C:\x`,
		`relativo\x`:                            `relativo\x`,
	}
	for in, quer := range casos {
		if got := comPrefixoLongo(in); got != quer {
			t.Errorf("comPrefixoLongo(%q) = %q; quer %q", in, got, quer)
		}
	}
}
