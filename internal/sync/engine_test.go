package sync

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func arquivoDeTeste(t *testing.T, conteudo string) FileInfo {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dados.db")
	if err := os.WriteFile(p, []byte(conteudo), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	return FileInfo{AbsolutePath: p, RelativePath: "dados.db", Size: st.Size(), ModTime: st.ModTime()}
}

// Arquivo que não muda: sobe, e o version_map registra o hash do que subiu.
func TestProcessFileSobeOQueHasheou(t *testing.T) {
	falso, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "conteúdo estável")
	e, dedup, err := processFile(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}, fi)
	if err != nil || dedup {
		t.Fatalf("err=%v dedup=%v", err, dedup)
	}
	o := falso.objetos["data/a/dados.db"]
	if e.SHA256 != sha256Hex(o.dados) || e.VersionID != o.versao {
		t.Fatalf("registro %+v não confere com o objeto (%s, %s)", e, sha256Hex(o.dados), o.versao)
	}
}

// Arquivo alterado no lugar entre o hash e o envio: antes, o version_map
// registrava o hash antigo com o conteúdo novo no bucket, e a restauração
// falhava com "sha256 mismatch". Agora o arquivo falha e a versão errada sai do
// bucket (senão envenenaria o dedup).
func TestProcessFileArquivoMudouNoEnvio(t *testing.T) {
	falso, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "AAAAAAAAAAAAAAAA")
	antesDoEnvio = func(p string) {
		if err := os.WriteFile(p, []byte("BBBBBBBBBBBBBBBB"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { antesDoEnvio = nil })

	e, _, err := processFile(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}, fi)
	if !errors.Is(err, ErrArquivoMudou) || e != nil {
		t.Fatalf("esperava ErrArquivoMudou sem registro, veio entry=%+v err=%v", e, err)
	}
	if _, ficou := falso.objetos["data/a/dados.db"]; ficou || len(falso.apagados) != 1 || falso.apagados[0] != "data/a/dados.db@v1" {
		t.Fatalf("a versão que não confere deveria sair do bucket; apagados=%v", falso.apagados)
	}
}

// Multipart: o mesmo, conferido antes do Complete — aborta, e nada fica.
func TestUploadMultipartArquivoMudou(t *testing.T) {
	falso, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "conteúdo original de um arquivo grande")
	hashAntigo := sha256Hex([]byte("conteúdo original de um arquivo grande"))
	if err := os.WriteFile(fi.AbsolutePath, []byte("conteúdo ALTERADO de um arquivo grande"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(fi.AbsolutePath)
	defer f.Close()
	_, err := uploadMultipart(context.Background(), c, "b", "k", f, nil, 0, hashAntigo, fi.Size)
	if !errors.Is(err, ErrArquivoMudou) || falso.completos != 0 || falso.abortados != 1 {
		t.Fatalf("esperava abort sem complete; err=%v completos=%d abortados=%d", err, falso.completos, falso.abortados)
	}
}

// Acima de ~156 GiB, partes fixas de 16 MiB passavam das 10.000 que o S3 aceita.
func TestTamanhoDaParteCabeEmDezMilPartes(t *testing.T) {
	const mib, gib = int64(1 << 20), int64(1 << 30)
	for _, tam := range []int64{100 * mib, 156 * gib, 157 * gib, 500 * gib, 4 * 1024 * gib} {
		p := tamanhoDaParte(tam)
		if p < 16*mib || p%mib != 0 {
			t.Errorf("%d bytes: parte %d (mín. 16 MiB, múltiplo de MiB)", tam, p)
		}
		if partes := (tam + p - 1) / p; partes > 10000 {
			t.Errorf("%d bytes: %d partes de %d — passa de 10.000", tam, partes, p)
		}
	}
	if tamanhoDaParte(100*mib) != 16*mib {
		t.Error("arquivo comum continua com partes de 16 MiB")
	}
}

// Serviço parando no meio do multipart (ctx cancelado): o abort tem de chegar
// ao bucket, senão as partes ficam órfãs e cobradas.
func TestUploadMultipartAbortaMesmoComCtxCancelado(t *testing.T) {
	falso, c := novoS3Falso(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	falso.falhar = func(r *http.Request) bool {
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "" {
			cancel() // o serviço parou durante o envio da parte
			return true
		}
		return false
	}
	conteudo := "parte única"
	fi := arquivoDeTeste(t, conteudo)
	f, _ := os.Open(fi.AbsolutePath)
	defer f.Close()
	if _, err := uploadMultipart(ctx, c, "b", "k", f, nil, 0, sha256Hex([]byte(conteudo)), fi.Size); err == nil {
		t.Fatal("o envio da parte falhou; esperava erro")
	}
	if falso.abortados != 1 {
		t.Fatalf("o abort não chegou ao bucket (abortados=%d)", falso.abortados)
	}
}

// Arquivo que só cresce (log aberto): sobe o que o walker viu (fi.Size bytes),
// com o hash desses bytes — antes o hash cobria também o que cresceu, e o
// registro não conferia com o que foi para o bucket.
func TestProcessFileArquivoCrescendoSobeOTamanhoDoWalk(t *testing.T) {
	falso, c := novoS3Falso(t)
	original := "linha 1\nlinha 2\n"
	fi := arquivoDeTeste(t, original)
	acrescentar := func(p string) {
		a, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		_, _ = a.WriteString("linha nova\n")
	}
	acrescentar(fi.AbsolutePath) // cresceu entre o walk e o hash
	antesDoEnvio = acrescentar   // e entre o hash e o envio
	t.Cleanup(func() { antesDoEnvio = nil })

	e, _, err := processFile(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}, fi)
	if err != nil {
		t.Fatalf("arquivo que só cresce não pode falhar: %v", err)
	}
	o := falso.objetos["data/a/dados.db"]
	if string(o.dados) != original || e.SHA256 != sha256Hex([]byte(original)) || e.Size != int64(len(original)) {
		t.Fatalf("esperava o conteúdo do walk; bucket=%q entry=%+v", o.dados, e)
	}
}

// Multipart de arquivo que só cresce: antes lia até o fim, mandava bytes a
// mais e abortava com ErrArquivoMudou todo dia; o PUT simples passava.
func TestUploadMultipartArquivoCrescendo(t *testing.T) {
	falso, c := novoS3Falso(t)
	original := "conteúdo de um log grande\n"
	fi := arquivoDeTeste(t, original)
	a, err := os.OpenFile(fi.AbsolutePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = a.WriteString("mais uma linha que chegou depois\n")
	a.Close()

	f, _ := os.Open(fi.AbsolutePath)
	defer f.Close()
	if _, err := uploadMultipart(context.Background(), c, "b", "k", f, nil, 0, sha256Hex([]byte(original)), fi.Size); err != nil {
		t.Fatalf("arquivo que só cresce não pode abortar: %v", err)
	}
	if falso.completos != 1 || string(falso.objetos["k"].dados) != original {
		t.Fatalf("esperava o conteúdo do walk no bucket; completos=%d dados=%q", falso.completos, falso.objetos["k"].dados)
	}
}

// Encolheu depois do walk: não há fi.Size bytes; falha como arquivo mudado.
func TestProcessFileArquivoEncolheu(t *testing.T) {
	_, c := novoS3Falso(t)
	fi := arquivoDeTeste(t, "conteúdo comprido")
	if err := os.WriteFile(fi.AbsolutePath, []byte("curto"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := processFile(context.Background(), EngineOptions{S3: c, Bucket: "b", PrefixRoot: "data/a/"}, fi); !errors.Is(err, ErrArquivoMudou) {
		t.Fatalf("esperava ErrArquivoMudou, veio %v", err)
	}
}
