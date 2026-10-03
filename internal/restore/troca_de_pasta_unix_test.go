//go:build !windows

package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// trocarPastaPorLink faz, no ponto do Run escolhido, o que faria quem manda na
// pasta de destino: renomeia a pasta (para pasta.old) e põe no lugar um link
// para alheio. A pasta já está aberta pelo descritor; tudo o que o Run ler ou
// gravar pelo caminho em texto passa a cair em alheio.
func trocarPastaPorLink(t *testing.T, gancho *func(), pasta, alheio string) {
	t.Helper()
	antes := *gancho
	t.Cleanup(func() { *gancho = antes })
	*gancho = func() {
		if err := os.Rename(pasta, pasta+".old"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(alheio, pasta); err != nil {
			t.Error(err)
		}
	}
}

// alheioCom cria, numa pasta de fora, o arquivo nome com o modo e o conteúdo
// dados (o /usr/bin/passwd 04755 root:root do cenário).
func alheioCom(t *testing.T, nome string, conteudo []byte, modo os.FileMode) string {
	t.Helper()
	alheio := t.TempDir()
	p := filepath.Join(alheio, nome)
	if err := os.WriteFile(p, conteudo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, modo); err != nil {
		t.Fatal(err)
	}
	return alheio
}

func conferirAlheioIntacto(t *testing.T, alheio, nome string, conteudo []byte, modo os.FileMode) {
	t.Helper()
	es, _ := os.ReadDir(alheio)
	if len(es) != 1 {
		t.Fatalf("gravou fora do destino, em %s: %v", alheio, es)
	}
	st, err := os.Stat(filepath.Join(alheio, nome))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(alheio, nome)); string(b) != string(conteudo) || st.Mode() != modo {
		t.Fatalf("arquivo de fora mudou: %q %v", b, st.Mode())
	}
}

// Durante o download, a pasta de destino vira um link para /usr/bin. O
// ajustarPermissoes lia o dono e o modo pelo caminho em texto e dava ao
// arquivo da pessoa o 04755 root:root do /usr/bin/passwd, gravado (pelo
// descritor) na pasta dela: um binário setuid root com o conteúdo dela.
func TestTrocaDaPastaDuranteODownloadNaoCopiaDonoNemModoDeFora(t *testing.T) {
	conteudo := []byte("programa da ana")
	setuid := os.FileMode(0o751) | os.ModeSetuid
	alheio := alheioCom(t, "passwd", []byte("binário do sistema"), setuid)
	pasta := filepath.Join(t.TempDir(), "tools")
	if err := os.Mkdir(pasta, 0o755); err != nil {
		t.Fatal(err)
	}
	// O arquivo de fora com outro grupo: um chown com o dono dele aparece.
	gAlheio := grupoExtra(t, filepath.Join(alheio, "passwd"))
	if err := os.Chmod(filepath.Join(alheio, "passwd"), setuid); err != nil { // o chown limpou o setuid
		t.Fatal(err)
	}
	donos, _ := registrarDonos(t)
	trocarPastaPorLink(t, &aposBaixar, pasta, alheio)

	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
		itemDe(pasta, "passwd", conteudo, "suffix-version")); err != nil {
		t.Fatal(err)
	}
	conferirAlheioIntacto(t, alheio, "passwd", []byte("binário do sistema"), setuid)
	st, err := os.Lstat(filepath.Join(pasta+".old", "passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != modoDeArquivoNovo {
		t.Fatalf("modo do restaurado = %v, queria %v (arquivo novo); veio do arquivo de fora", st.Mode(), modoDeArquivoNovo)
	}
	// Arquivo novo fica do dono da pasta aberta, nunca do arquivo de fora.
	uid, gid := donoDe(t, pasta+".old")
	if len(*donos) != 1 || (*donos)[0] != [2]int{uid, gid} || gid == gAlheio {
		t.Fatalf("donos dados ao restaurado: %v, queria só o da pasta aberta (%d:%d), nunca o grupo %d do arquivo de fora",
			*donos, uid, gid, gAlheio)
	}
}

// A troca logo depois de abrir a pasta: o jaRestaurado e o resolveConflict
// olhavam o caminho em texto, viam o arquivo de fora e decidiam por ele —
// "já restaurado" (nada gravado) ou "existe" (skip, sufixo).
func TestTrocaDaPastaAntesDoConflitoDecidePelaPastaAberta(t *testing.T) {
	conteudo := []byte("conteúdo do backup")
	for nome, caso := range map[string]struct {
		alheio     []byte
		estrategia string
	}{
		"mesmo conteúdo (jaRestaurado)":  {conteudo, "suffix-version"},
		"outro conteúdo, skip":           {[]byte("outro"), "skip"},
		"outro conteúdo, suffix-version": {[]byte("outro"), "suffix-version"},
	} {
		t.Run(nome, func(t *testing.T) {
			alheio := alheioCom(t, "app.conf", caso.alheio, 0o644)
			pasta := filepath.Join(t.TempDir(), "conf")
			if err := os.Mkdir(pasta, 0o755); err != nil {
				t.Fatal(err)
			}
			trocarPastaPorLink(t, &aposAbrirPasta, pasta, alheio)

			err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo), HostRoot: "/"},
				itemDe(pasta, "app.conf", conteudo, caso.estrategia))
			if errors.Is(err, ErrPulado) {
				t.Fatal("pulou pelo arquivo de fora")
			}
			if err != nil {
				t.Fatal(err)
			}
			conferirAlheioIntacto(t, alheio, "app.conf", caso.alheio, 0o644)
			es, _ := os.ReadDir(pasta + ".old")
			if len(es) != 1 || es[0].Name() != "app.conf" {
				t.Fatalf("na pasta aberta queria só app.conf, veio %v", es)
			}
			if b, _ := os.ReadFile(filepath.Join(pasta+".old", "app.conf")); string(b) != string(conteudo) {
				t.Fatalf("conteúdo restaurado: %q", b)
			}
		})
	}
}
