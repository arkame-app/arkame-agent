package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseFalso serve o checksums.txt da tag v1.2.3 com a linha dada.
func releaseFalso(t *testing.T, linhas string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.2.3/checksums.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(linhas))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func programaBaixado(t *testing.T, conteudo string) (caminho, sha string) {
	t.Helper()
	caminho = filepath.Join(t.TempDir(), "arkame-agent.exe")
	if err := os.WriteFile(caminho, []byte(conteudo), 0o755); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256([]byte(conteudo))
	return caminho, hex.EncodeToString(s[:])
}

const nomeNoRelease = "arkame-agent_windows_amd64.exe"

func TestConferirProgramaConfere(t *testing.T) {
	exe, sha := programaBaixado(t, "programa bom")
	base := releaseFalso(t, strings.Repeat("0", 64)+"  arkame-agent_windows_amd64.zip\n"+sha+"  "+nomeNoRelease+"\n")
	for _, v := range []string{"1.2.3", "v1.2.3"} {
		if err := conferirPrograma(context.Background(), http.DefaultClient, base, v, nomeNoRelease, exe); err != nil {
			t.Fatalf("versão %s: %v", v, err)
		}
	}
}

// Programa adulterado (ou baixado pela metade): o checksum não bate.
func TestConferirProgramaDiferente(t *testing.T) {
	exe, _ := programaBaixado(t, "programa adulterado")
	_, outro := programaBaixado(t, "programa bom")
	base := releaseFalso(t, outro+"  "+nomeNoRelease+"\n")
	err := conferirPrograma(context.Background(), http.DefaultClient, base, "1.2.3", nomeNoRelease, exe)
	if !errors.Is(err, errChecksumDiferente) {
		t.Fatalf("esperava errChecksumDiferente, veio %v", err)
	}
}

// Sem como conferir — release inexistente, rede fora, programa fora da
// lista, versão de desenvolvimento — é errSemConferencia, não "confere".
func TestConferirProgramaSemConferencia(t *testing.T) {
	exe, sha := programaBaixado(t, "programa bom")
	base := releaseFalso(t, sha+"  outro-programa.exe\n")
	fora := httptest.NewServer(http.NotFoundHandler())
	fora.Close()
	casos := map[string]struct{ base, versao string }{
		"fora da lista":       {base, "1.2.3"},
		"release inexistente": {base, "9.9.9"},
		"rede fora":           {fora.URL, "1.2.3"},
		"desenvolvimento":     {base, "dev"},
	}
	for nome, c := range casos {
		err := conferirPrograma(context.Background(), http.DefaultClient, c.base, c.versao, nomeNoRelease, exe)
		if !errors.Is(err, errSemConferencia) {
			t.Fatalf("%s: esperava errSemConferencia, veio %v", nome, err)
		}
	}
}

// A decisão do setup: diferente aborta sempre; sem conferência aborta, a
// menos que venha --skip-checksum.
func TestVerificarBaixado(t *testing.T) {
	exe, sha := programaBaixado(t, "programa bom")
	_, outro := programaBaixado(t, "outro")
	bom := releaseFalso(t, sha+"  "+nomeNoRelease+"\n")
	ruim := releaseFalso(t, outro+"  "+nomeNoRelease+"\n")
	fora := httptest.NewServer(http.NotFoundHandler())
	fora.Close()

	var saida bytes.Buffer
	verificar := func(base string, pular bool) error {
		return verificarBaixado(context.Background(), http.DefaultClient, base, "1.2.3", nomeNoRelease, exe, pular, &saida)
	}
	if err := verificar(bom, false); err != nil {
		t.Fatalf("checksum certo: %v", err)
	}
	for _, pular := range []bool{false, true} {
		if err := verificar(ruim, pular); !errors.Is(err, errChecksumDiferente) {
			t.Fatalf("checksum diferente com pular=%v seguiu: %v", pular, err)
		}
	}
	err := verificar(fora.URL, false)
	if !errors.Is(err, errSemConferencia) || !strings.Contains(err.Error(), "--skip-checksum") {
		t.Fatalf("sem checksums, sem a opção: queria abortar citando --skip-checksum, veio %v", err)
	}
	saida.Reset()
	if err := verificar(fora.URL, true); err != nil {
		t.Fatalf("com --skip-checksum devia seguir: %v", err)
	}
	if !strings.Contains(saida.String(), "--skip-checksum") {
		t.Fatalf("seguir sem conferir tem de avisar; saída: %q", saida.String())
	}
}
