package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Mudar a pasta da cópia local (Hugo, 17/10): para o serviço, copia os dados
// com as permissões, reinstala apontando para a pasta nova e deixa a antiga.
// Falhou no meio: a pasta nova sai, o serviço volta na antiga, nada reinstala.
func TestMoverCopiaLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissões Unix")
	}
	// O teste roda em /tmp, que a validação recusa para dados de verdade.
	antesTemp := pastasTemporarias
	pastasTemporarias = nil
	t.Cleanup(func() { pastasTemporarias = antesTemp })
	base := t.TempDir()
	antiga := filepath.Join(base, "srv", "arkame-local")
	if err := os.MkdirAll(filepath.Join(antiga, "bucket", ".rustfs.sys"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(antiga, "bucket", "obj"), []byte("versão 1"), 0o600)
	cfgDir := filepath.Join(base, "etc")
	os.MkdirAll(cfgDir, 0o700)
	principal := filepath.Join(cfgDir, "agent.env")
	os.WriteFile(principal, []byte("AGENT_ID=a1\n"), 0o600)
	os.WriteFile(filepath.Join(cfgDir, "agent-local.env"), []byte("AGENT_ID=a1\nSTORAGE_ID=24e7a7b5-57dc-400b-8001-dc2f6dceb040\nSTORAGE_BUCKET=arkame-local-7d6bd828\nARKAME_LOCAL_STORE_DATA="+antiga+"\nARKAME_LOCAL_STORE_ADDRESS=127.0.0.1:9911\n"), 0o600)

	var parados, religados int
	var instalado *opcoesDaCopiaLocal
	falhaNaInstalacao := false
	antes := [3]any{pararServicoDaCopia, religarServicoDaCopia, instalarParaMover}
	t.Cleanup(func() {
		pararServicoDaCopia = antes[0].(func(string) error)
		religarServicoDaCopia = antes[1].(func(string) error)
		instalarParaMover = antes[2].(func(context.Context, opcoesDaCopiaLocal, escritor) error)
	})
	pararServicoDaCopia = func(string) error { parados++; return nil }
	religarServicoDaCopia = func(string) error { religados++; return nil }
	instalarParaMover = func(_ context.Context, o opcoesDaCopiaLocal, _ escritor) error {
		instalado = &o
		if falhaNaInstalacao {
			return os.ErrPermission
		}
		return nil
	}
	mover := func(nova string) (string, error) {
		var out bytes.Buffer
		err := moverCopiaLocal(context.Background(), nova, principal, "arkame-agent-local", "arkame-agent", &out)
		return out.String(), err
	}

	// Dentro da antiga, ou com arquivos: recusadas antes de parar o serviço.
	if _, err := mover(filepath.Join(antiga, "sub")); err == nil {
		t.Fatal("aceitou a pasta nova dentro da antiga")
	}
	cheia := filepath.Join(base, "cheia")
	os.MkdirAll(cheia, 0o700)
	os.WriteFile(filepath.Join(cheia, "x"), []byte("x"), 0o600)
	if _, err := mover(cheia); err == nil || !strings.Contains(err.Error(), "já tem arquivos") {
		t.Fatalf("aceitou uma pasta com arquivos: %v", err)
	}
	// Um arquivo no lugar da pasta nova: recusado, e o arquivo continua lá.
	arquivo := filepath.Join(base, "um-arquivo")
	os.WriteFile(arquivo, []byte("não apague"), 0o600)
	if _, err := mover(arquivo); err == nil || !strings.Contains(err.Error(), "não é uma pasta") {
		t.Fatalf("aceitou um arquivo como pasta nova: %v", err)
	}
	if b, _ := os.ReadFile(arquivo); string(b) != "não apague" {
		t.Fatal("o arquivo no lugar da pasta nova foi mexido")
	}
	if parados != 0 {
		t.Fatal("parou o serviço antes de validar")
	}

	nova := filepath.Join(base, "opt", "arkame-local")
	saida, err := mover(nova)
	if err != nil {
		t.Fatalf("mover: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(nova, "bucket", "obj")); string(b) != "versão 1" {
		t.Fatalf("o objeto não foi copiado: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(nova, "bucket", "obj")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("permissão do objeto não ficou igual: %v", fi)
	}
	if fi, _ := os.Stat(filepath.Join(nova, "bucket", ".rustfs.sys")); fi == nil || !fi.IsDir() {
		t.Fatal("a pasta oculta do RustFS não foi copiada")
	}
	if instalado == nil || instalado.pasta != nova || instalado.storageID != "24e7a7b5-57dc-400b-8001-dc2f6dceb040" || instalado.bucket != "arkame-local-7d6bd828" || instalado.porta != 9911 {
		t.Fatalf("reinstalou com as opções erradas: %+v", instalado)
	}
	if _, err := os.Stat(filepath.Join(antiga, "bucket", "obj")); err != nil {
		t.Fatal("a pasta antiga foi mexida")
	}
	if !strings.Contains(saida, "rm -rf "+antiga) || parados != 1 || religados != 0 {
		t.Fatalf("saída ou serviço: parados=%d religados=%d %q", parados, religados, saida)
	}

	// Falha na cópia (um link nos dados): a nova sai, o serviço volta na antiga.
	instalado = nil
	os.Symlink("/etc/passwd", filepath.Join(antiga, "bucket", "link"))
	outra := filepath.Join(base, "opt2", "arkame-local")
	if _, err := mover(outra); err == nil {
		t.Fatal("copiou um link de dentro dos dados")
	}
	if _, err := os.Stat(outra); !os.IsNotExist(err) {
		t.Fatal("a pasta nova pela metade ficou")
	}
	if religados != 1 || instalado != nil {
		t.Fatalf("depois da falha: religados=%d instalado=%v", religados, instalado)
	}
}
