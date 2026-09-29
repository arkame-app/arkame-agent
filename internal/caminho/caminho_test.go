package caminho

import "testing"

func TestNoDiscoWindows(t *testing.T) {
	for entrada, esperado := range map[string]string{
		`C:\Users\Greyce\Documents`: `C:\Users\Greyce\Documents`,
		`c:/Users/Greyce/`:          `C:\Users\Greyce`,
		`D:\`:                       `D:\`,
		`D:`:                        `D:\`,
		`/Users/Greyce`:             `C:\Users\Greyce`, // plano antigo
	} {
		if got := noDisco(true, "/", entrada); got != esperado {
			t.Errorf("noDisco(%q) = %q, esperado %q", entrada, got, esperado)
		}
	}
	if got := noDisco(false, "/host", "/var/dados"); got != "/host/var/dados" {
		t.Errorf("Linux no Docker: %q", got)
	}
}

func TestNaChaveWindows(t *testing.T) {
	// A unidade entra na chave: C:\dados e D:\dados não podem cair no mesmo lugar.
	if got := naChave(true, "/", `C:\Users\Greyce\a.txt`); got != "C:/Users/Greyce/a.txt" {
		t.Errorf("C: → %q", got)
	}
	if got := naChave(true, "/", `d:\dados\a.txt`); got != "D:/dados/a.txt" {
		t.Errorf("D: → %q", got)
	}
	if got := naChave(false, "/host", "/host/var/a.txt"); got != "var/a.txt" {
		t.Errorf("Linux → %q", got)
	}
}

func TestDestino(t *testing.T) {
	if got, err := destino(true, "/", `C:\Restaurados`); err != nil || got != `C:\Restaurados` {
		t.Errorf("Windows C:\\ → %q, %v", got, err)
	}
	if got, err := destino(true, "/", "D:/x"); err != nil || got != `D:\x` {
		t.Errorf("Windows D:/ → %q, %v", got, err)
	}
	if _, err := destino(true, "/", `Restaurados`); err == nil {
		t.Error("Windows relativo deveria ser recusado")
	}
	if _, err := destino(false, "/", `C:\x`); err == nil {
		t.Error("Linux não aceita C:\\")
	}
	if got, _ := destino(false, "/host", "/restore"); got != "/host/restore" {
		t.Errorf("Linux → %q", got)
	}
}
