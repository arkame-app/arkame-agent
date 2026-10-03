//go:build !windows

package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ano9999 é uma data que o UnixNano (1677–2262) não representa.
var ano9999 = time.Date(9999, 6, 1, 12, 0, 0, 0, time.UTC)

// datarArquivo põe a data no arquivo sem passar por UnixNano; pula o teste
// se o sistema de arquivos não guarda a data (ext4 para em 2446).
func datarArquivo(t *testing.T, p string, quando time.Time) {
	t.Helper()
	ts, err := unix.TimeToTimespec(quando)
	if err != nil {
		t.Skipf("plataforma sem timespec de 64 bits: %v", err)
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ts, ts}, 0); err != nil {
		t.Skipf("o sistema de arquivos não aceitou a data: %v", err)
	}
	if st, err := os.Stat(p); err != nil || !st.ModTime().Equal(quando) {
		t.Skipf("o sistema de arquivos não guarda o ano %d", quando.Year())
	}
}

// Arquivo com data fora de 1677–2262: o walker a guardava em UnixNano, que
// dava a volta, e o índice recebia outra data.
func TestWalkGuardaDataForaDoUnixNano(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "futuro.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	datarArquivo(t, p, ano9999)

	arquivos, erros := Walk(context.Background(), "/", []string{dir}, nil)
	var achou []FileInfo
	for fi := range arquivos {
		achou = append(achou, fi)
	}
	if err := <-erros; err != nil {
		t.Fatal(err)
	}
	if len(achou) != 1 || !achou[0].ModTime.Equal(ano9999) {
		t.Fatalf("data do arquivo = %v, queria %v", achou, ano9999)
	}
}

// A data vai ao painel inteira até 9999 e, além disso, presa a 9999-12-31: o
// JSON de time.Time recusa ano acima de 9999 e a sessão não fecharia.
func TestEntradaDoIndiceComDataExtrema(t *testing.T) {
	for _, c := range []struct{ arquivo, indice time.Time }{
		{ano9999, ano9999},
		{time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC), maiorDataDoPainel},
		{time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		falso, cli := novoS3Falso(t)
		fi := arquivoDeTeste(t, "igual")
		fi.ModTime = c.arquivo
		falso.objetos["data/a/dados.db"] = objetoFalso{dados: []byte("igual"), sha256: sha256Hex([]byte("igual")), versao: "v9"}
		e, _, err := processFile(context.Background(), EngineOptions{S3: cli, Bucket: "b", PrefixRoot: "data/a/"}, fi)
		if err != nil {
			t.Fatal(err)
		}
		if !e.ModifiedAt.Equal(c.indice) {
			t.Fatalf("arquivo de %v: modified_at = %v, queria %v", c.arquivo, e.ModifiedAt, c.indice)
		}
		if _, err := json.Marshal(e); err != nil {
			t.Fatalf("entrada não vira JSON: %v", err)
		}
	}
}
