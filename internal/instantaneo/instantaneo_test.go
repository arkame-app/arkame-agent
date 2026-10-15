package instantaneo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const dispC = `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy5`

// A leitura troca a unidade pelo dispositivo do instantâneo dela, e só a dela.
// A raiz do volume leva a barra: sem ela, abre-se o dispositivo, e não a pasta.
func TestLerEm(t *testing.T) {
	volumes := map[string]string{"C:": dispC}
	casos := map[string]string{
		`C:\Users\Greyce\a.pst`: dispC + `\Users\Greyce\a.pst`,
		`C:\`:                   dispC + `\`,
		`C:`:                    dispC + `\`,
		`D:\dados`:              `D:\dados`,
		`/var/dados`:            `/var/dados`,
	}
	for p, queria := range casos {
		if got := lerEm(volumes, p); got != queria {
			t.Errorf("lerEm(%q) = %q; queria %q", p, got, queria)
		}
	}
}

// As unidades das pastas do plano, sem repetir; o formato antigo (`/Users`) é
// do disco do sistema, e caminho de rede não tem instantâneo.
func TestUnidadesDoPlano(t *testing.T) {
	got := unidadesDoPlano([]string{`D:\b`, `c:\a`, `C:/x`, `/Users/x`, `\\servidor\pasta`, `D:\c`})
	if queria := []string{"C:", "D:"}; !reflect.DeepEqual(got, queria) {
		t.Fatalf("unidades = %v; queria %v", got, queria)
	}
}

func trocar(t *testing.T, win bool, f func(context.Context, []string) (map[string]string, func(), error)) {
	t.Helper()
	velhoW, velhoC := noWindows, criar
	noWindows, criar = win, f
	t.Cleanup(func() { noWindows, criar = velhoW, velhoC })
}

func TestAbrirComInstantaneo(t *testing.T) {
	soltou := 0
	var pedidas []string
	trocar(t, true, func(_ context.Context, u []string) (map[string]string, func(), error) {
		pedidas = u
		return map[string]string{"C:": dispC}, func() { soltou++ }, nil
	})
	i := Abrir(context.Background(), "auto", []string{`C:\Users\x`})
	if !i.Ativo() || i.Metodo != MetodoVSS || i.Aviso != "" {
		t.Fatalf("instantâneo = %+v; queria vss sem aviso", i)
	}
	if !reflect.DeepEqual(pedidas, []string{"C:"}) {
		t.Fatalf("unidades pedidas = %v", pedidas)
	}
	if got := i.Ler(`C:\Users\x\a.txt`); got != dispC+`\Users\x\a.txt` {
		t.Fatalf("Ler = %q", got)
	}
	i.Fechar()
	i.Fechar()
	if soltou != 1 {
		t.Fatalf("soltou %d vezes; queria 1", soltou)
	}
}

// Falhar ao criar não falha o backup: lê do disco, e o motivo vai ao painel.
func TestAbrirSemInstantaneoLeDoDisco(t *testing.T) {
	trocar(t, true, func(context.Context, []string) (map[string]string, func(), error) {
		return nil, nil, errors.New("acesso negado")
	})
	i := Abrir(context.Background(), "", []string{`C:\a`, `D:\b`})
	if i.Ativo() || i.Metodo != MetodoNenhum || !strings.Contains(i.Aviso, "C:, D:") || !strings.Contains(i.Aviso, "acesso negado") {
		t.Fatalf("instantâneo = %+v; queria leitura do disco com o motivo", i)
	}
	if got := i.Ler(`C:\a\x`); got != `C:\a\x` {
		t.Fatalf("Ler sem instantâneo mudou o caminho: %q", got)
	}
	i.Fechar()
}

// Plano que dispensa, e outro sistema: nem tenta.
func TestAbrirNaoTenta(t *testing.T) {
	chamou := false
	f := func(context.Context, []string) (map[string]string, func(), error) {
		chamou = true
		return nil, nil, errors.New("x")
	}
	trocar(t, true, f)
	if i := Abrir(context.Background(), ModoDesligado, []string{`C:\a`}); i.Metodo != "" || chamou {
		t.Fatalf("plano com snapshot=off tentou o instantâneo: %+v", i)
	}
	trocar(t, false, f)
	if i := Abrir(context.Background(), "auto", []string{"/var/dados"}); i.Metodo != "" || chamou {
		t.Fatalf("fora do Windows tentou o instantâneo: %+v", i)
	}
}

// O valor zero (e o nil) lê do disco.
func TestNilLeDoDisco(t *testing.T) {
	var i *Instantaneo
	if i.Ler(`C:\a`) != `C:\a` || i.Ativo() {
		t.Fatal("nil não lê do disco")
	}
	i.Fechar()
}

// A unidade que não aceita instantâneo é lida do disco, com aviso, e as
// outras seguem do instantâneo.
func TestUnidadeSemSuporteLeDoDiscoComAviso(t *testing.T) {
	trocar(t, true, func(context.Context, []string) (map[string]string, func(), error) {
		return map[string]string{"C:": dispC}, func() {}, nil
	})
	i := Abrir(context.Background(), "auto", []string{`C:\a`, `E:\pendrive`})
	if !i.Ativo() || i.Metodo != MetodoVSS || !strings.Contains(i.Aviso, "E:") || strings.Contains(i.Aviso, "C:") {
		t.Fatalf("instantâneo = %+v; queria vss com aviso só de E:", i)
	}
	if i.Ler(`E:\pendrive\x`) != `E:\pendrive\x` || i.Ler(`C:\a\x`) != dispC+`\a\x` {
		t.Fatal("E: devia ler do disco e C: do instantâneo")
	}
}
