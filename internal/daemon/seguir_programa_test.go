package daemon

import (
	"context"
	"os"
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
