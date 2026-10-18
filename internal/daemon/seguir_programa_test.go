package daemon

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Quando o executável muda (o principal trocou o programa), o processo da
// cópia local pede para reiniciar; com algo em curso, espera.
func TestSeguirOProgramaReiniciaQuandoOArquivoMuda(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	antes := intervaloDeSeguir
	intervaloDeSeguir = 50 * time.Millisecond
	t.Cleanup(func() { intervaloDeSeguir = antes })
	fi, _ := os.Stat(exe)
	t.Cleanup(func() { os.Chtimes(exe, fi.ModTime(), fi.ModTime()) })

	oc := &ocupacao{trava: &exclusaoDoBucket{}}
	oc.comecarRestauracao() // uma restauração em curso
	reiniciou := make(chan struct{})
	ctx, cancelar := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelar()
	go seguirOPrograma(ctx, oc, func() { close(reiniciou) }, nil)

	time.Sleep(150 * time.Millisecond)
	select {
	case <-reiniciou:
		t.Fatal("reiniciou sem o programa mudar")
	default:
	}
	novo := fi.ModTime().Add(time.Minute)
	os.Chtimes(exe, novo, novo)
	time.Sleep(200 * time.Millisecond)
	select {
	case <-reiniciou:
		t.Fatal("reiniciou no meio de uma restauração")
	default:
	}
	oc.terminarRestauracao()
	select {
	case <-reiniciou:
	case <-time.After(2 * time.Second):
		t.Fatal("o programa mudou e o processo da cópia local não reiniciou")
	}
}

// No Docker, a marca é a versão confirmada no volume: a prova de uma versão
// nova ("" enquanto não confirma) não reinicia o irmão, nem na 2ª
// atualização; a confirmação de outra versão reinicia (revisão, 18/10).
func TestSeguirNoDockerIgnoraAProva(t *testing.T) {
	antes := intervaloDeSeguir
	intervaloDeSeguir = 20 * time.Millisecond
	t.Cleanup(func() { intervaloDeSeguir = antes })
	var versao atomic.Value
	versao.Store("0.5.5")
	oc := &ocupacao{trava: &exclusaoDoBucket{}}
	reiniciou := make(chan struct{})
	ctx, cancelar := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelar()
	go seguirOPrograma(ctx, oc, func() { close(reiniciou) }, func() string { return versao.Load().(string) })
	quieto := func(o string) {
		t.Helper()
		time.Sleep(120 * time.Millisecond)
		select {
		case <-reiniciou:
			t.Fatalf("reiniciou %s", o)
		default:
		}
	}
	quieto("sem nada mudar")
	versao.Store("") // a 0.5.6 em prova
	quieto("durante a prova")
	versao.Store("0.5.5") // prova abandonada, volta a confirmada de antes
	quieto("com a mesma versão confirmada")
	versao.Store("")
	quieto("na segunda prova")
	versao.Store("0.5.6")
	select {
	case <-reiniciou:
	case <-time.After(2 * time.Second):
		t.Fatal("a versão nova foi confirmada e o irmão não reiniciou")
	}
}
