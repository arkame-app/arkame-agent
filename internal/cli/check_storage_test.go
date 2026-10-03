package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// O check-storage lia só --config (padrão /etc/arkame/agent.env): no agente
// sem root e num segundo agente testava o bucket de outro arquivo. Sem
// --config, o arquivo é o do serviço, como no status e no set-storage-keys.
func TestCheckStorageLeOArquivoDoServico(t *testing.T) {
	for _, k := range []string{"STORAGE_BUCKET", "STORAGE_ID", "STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	escrever := func(nome, bucket string) string {
		p := filepath.Join(dir, nome)
		if err := os.WriteFile(p, []byte("STORAGE_BUCKET="+bucket+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	doUsuario := escrever("usuario.env", "bucket-usuario")
	doOutro := escrever("oci.env", "bucket-oci")
	explicito := escrever("explicito.env", "bucket-explicito")

	antesServico := configDoServico
	configDoServico = func(nome string, escopo service.Scope) (string, bool) {
		switch {
		case nome == service.DefaultName && escopo == "":
			return doUsuario, true
		case nome == "arkame-agent-oci":
			return doOutro, true
		}
		return "", false
	}
	var testado string
	antesCheck := checarStorage
	checarStorage = func(_ context.Context, cfg *config.Config) error {
		testado = cfg.StorageBucket
		return nil
	}
	t.Cleanup(func() { configDoServico, checarStorage = antesServico, antesCheck })

	rodar := func(args ...string) (string, error) {
		testado = ""
		c := newCheckStorageCmd()
		var b bytes.Buffer
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		err := c.Execute()
		return b.String(), err
	}

	if out, err := rodar(); err != nil || testado != "bucket-usuario" || !strings.Contains(out, doUsuario) {
		t.Fatalf("sem flags deveria testar o bucket de %s; testou %q, err=%v\n%s", doUsuario, testado, err, out)
	}
	if _, err := rodar("--service-name", "arkame-agent-oci"); err != nil || testado != "bucket-oci" {
		t.Fatalf("--service-name deveria testar o bucket de %s; testou %q, err=%v", doOutro, testado, err)
	}
	if _, err := rodar("--config", explicito, "--service-name", "arkame-agent-oci"); err != nil || testado != "bucket-explicito" {
		t.Fatalf("--config manda; testou %q, err=%v", testado, err)
	}
	if _, err := rodar("--service-name", "arkame-agent-sumido"); err == nil || !strings.Contains(err.Error(), "arkame-agent-sumido") || testado != "" {
		t.Fatalf("serviço sem registro deveria dar erro sem testar nada; err=%v testou %q", err, testado)
	}
}
