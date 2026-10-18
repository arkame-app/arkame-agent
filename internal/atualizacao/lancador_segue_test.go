package atualizacao

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

// O irmão que segue o principal não se atualiza sozinho, mas é reconhecido.
func TestSegueEDesligada(t *testing.T) {
	if !Desligada("follow") || !Segue("follow") || Segue("off") || Segue("") {
		t.Fatal("follow tem de desligar a atualização própria e marcar que segue o principal")
	}
}

// O irmão não executa a versão em prova nem conta a partida dela: quem a
// confirma é o principal. Confirmada, o irmão a executa.
func TestIrmaoSoExecutaAVersaoConfirmada(t *testing.T) {
	b := noVolume(t)
	aberto, porque := AbrirDoVolumeParaIrmao(b.lugar.Atual, "0.4.31", []ed25519.PublicKey{b.pub}, NomeNoRelease())
	if aberto != nil {
		_ = aberto.Close()
		t.Fatal("o irmão executou a versão ainda em prova")
	}
	if !strings.Contains(porque, "em prova") {
		t.Fatalf("motivo = %q", porque)
	}
	if e := b.estado(); e.Arranques[chaveDoLancador] != 0 {
		t.Fatalf("o irmão contou uma partida: %+v", e)
	}
	e := b.estado()
	e.Situacao = SituacaoConfirmada
	if err := b.lugar.gravarEstado(e); err != nil {
		t.Fatal(err)
	}
	aberto, porque = AbrirDoVolumeParaIrmao(b.lugar.Atual, "0.4.31", []ed25519.PublicKey{b.pub}, NomeNoRelease())
	if aberto == nil {
		t.Fatalf("confirmada, o irmão não a executou: %s", porque)
	}
	_ = aberto.Close()
}
