//go:build windows

package restore

import (
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func daclProtegida(t *testing.T, p string) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	return ctl&windows.SE_DACL_PROTECTED != 0
}

// A pasta de destino que o agente cria (C:\Restaurados) nasce só para
// Administradores e SYSTEM; antes herdava a ACL da pasta de cima, com leitura
// para Users.
func TestPastaDeDestinoCriadaNasceProtegida(t *testing.T) {
	conteudo := []byte("SENHA=x")
	destino := filepath.Join(t.TempDir(), "Restaurados")
	item := itemDe(destino, ".env", conteudo, "suffix-version")
	item.DestFilename = "C/app/.env"
	if err := Run(context.Background(), Options{S3: bucketComObjeto(t, conteudo)}, item); err != nil {
		t.Fatal(err)
	}
	if !daclProtegida(t, destino) {
		t.Fatalf("%s sem a DACL protegida", destino)
	}
	// Pasta que já existia não é tocada.
	if daclProtegida(t, filepath.Dir(destino)) {
		t.Fatalf("%s (que já existia) ficou com a DACL protegida", filepath.Dir(destino))
	}
}
