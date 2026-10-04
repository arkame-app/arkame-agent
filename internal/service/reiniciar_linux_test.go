//go:build linux

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// No Linux, RodandoOPrograma devolvia nil e Reiniciar só falhava: depois de
// trocar o programa, os outros agentes seguiam no antigo. Agora lê as units
// (aqui, as do usuário) e reinicia no escopo de cada uma.
func TestRodandoOProgramaEReiniciarNoSystemd(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	units := filepath.Join(xdg, "systemd", "user")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	sistema := t.TempDir()
	antesDir := dirUnitsDoSistema
	t.Cleanup(func() { dirUnitsDoSistema = antesDir })
	dirUnitsDoSistema = sistema
	bin := filepath.Join(t.TempDir(), "arkame-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for nome, programa := range map[string]string{
		"arkame-agent-a":      bin,
		"arkame-agent-b":      bin,
		"arkame-agent-parado": bin,
		"arkame-agent-outro":  "/usr/local/bin/arkame-agent-outro",
	} {
		u := "[Service]\nExecStart=" + quoteArg(programa) + " run --config /x.env\n"
		if err := os.WriteFile(filepath.Join(units, nome+".service"), []byte(u), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Uma unit do sistema, ativa, com o mesmo programa.
	u := "[Service]\nExecStart=" + quoteArg(bin) + " run --config /etc/arkame/s.env\n"
	if err := os.WriteFile(filepath.Join(sistema, "arkame-agent-s.service"), []byte(u), 0o644); err != nil {
		t.Fatal(err)
	}

	var chamadas []string
	antes := executar
	t.Cleanup(func() { executar = antes })
	executar = func(_ context.Context, nome string, args ...string) ([]byte, error) {
		c := nome + " " + strings.Join(args, " ")
		chamadas = append(chamadas, c)
		if slices.Contains(args, "is-active") && !strings.HasSuffix(c, "arkame-agent-parado") &&
			(strings.Contains(c, "--user") || strings.HasSuffix(c, "arkame-agent-s")) {
			return nil, nil
		}
		if slices.Contains(args, "restart") {
			return nil, nil
		}
		return nil, errors.New("inactive")
	}

	got := RodandoOPrograma(bin)
	slices.Sort(got)
	if !slices.Equal(got, []string{"arkame-agent-a", "arkame-agent-b", "arkame-agent-s"}) {
		t.Fatalf("RodandoOPrograma = %v", got)
	}

	chamadas = nil
	if err := Reiniciar("arkame-agent-b"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(chamadas, "systemctl --user restart arkame-agent-b") {
		t.Fatalf("não reiniciou no escopo do usuário: %v", chamadas)
	}
	chamadas = nil
	if err := Reiniciar("arkame-agent-s"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(chamadas, "systemctl restart arkame-agent-s") {
		t.Fatalf("não reiniciou no escopo do sistema: %v", chamadas)
	}
}

// Reiniciar cortava a espera em 2 min fixos, abaixo de EsperaParada (150s):
// o systemctl restart morria enquanto o daemon ainda fechava um backup, e o
// setup dizia que o serviço continuava na versão antiga. O prazo agora vem de
// EsperaParada, com folga para o serviço subir de novo.
func TestReiniciarEsperaMaisQueEsperaParada(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	antesDir := dirUnitsDoSistema
	t.Cleanup(func() { dirUnitsDoSistema = antesDir })
	dirUnitsDoSistema = t.TempDir()

	if prazoDoReinicio() != EsperaParada+folgaDoReinicio {
		t.Fatalf("prazoDoReinicio() = %s, querido EsperaParada (%s) + folga (%s)", prazoDoReinicio(), EsperaParada, folgaDoReinicio)
	}
	if folgaDoReinicio <= 0 {
		t.Fatalf("folgaDoReinicio = %s, precisa ser positiva", folgaDoReinicio)
	}

	var prazo time.Duration
	antes := executar
	t.Cleanup(func() { executar = antes })
	executar = func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if slices.Contains(args, "restart") {
			d, ok := ctx.Deadline()
			if !ok {
				t.Fatal("restart sem prazo")
			}
			prazo = time.Until(d)
		}
		return nil, nil
	}
	inicio := time.Now()
	if err := Reiniciar("arkame-agent-x"); err != nil {
		t.Fatal(err)
	}
	gasto := time.Since(inicio)
	if prazo <= EsperaParada || prazo > prazoDoReinicio() || prazo < prazoDoReinicio()-gasto-time.Second {
		t.Fatalf("prazo do restart = %s, querido ~%s (EsperaParada %s + folga)", prazo, prazoDoReinicio(), EsperaParada)
	}
}
