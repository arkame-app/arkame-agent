package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pacoteFalso monta o tar.gz do release com um arkame-agent que só responde
// "version".
func pacoteFalso(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := []byte("#!/bin/sh\necho arkame-agent falso\n")
	if err := tw.WriteHeader(&tar.Header{Name: "arkame-agent", Mode: 0o755, Size: int64(len(bin))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(bin)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// rodarInstallSh roda o install.sh contra um espelho local que serve o pacote
// e, se checksums não for nil, o checksums.txt com esse conteúdo (a função
// recebe o nome do pacote e o sha256 certo). Devolve a saída, se terminou bem
// e se o binário foi instalado.
func rodarInstallSh(t *testing.T, checksums func(pacote, sha string) string, args ...string) (string, bool, bool) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh é do Linux e do macOS")
	}
	for _, f := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(f); err != nil {
			t.Skipf("sem %s", f)
		}
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("arquitetura sem pacote")
	}
	pacote := fmt.Sprintf("arkame-agent_%s_%s.tar.gz", runtime.GOOS, arch)
	conteudo := pacoteFalso(t)
	h := sha256.Sum256(conteudo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+pacote:
			_, _ = w.Write(conteudo)
		case r.URL.Path == "/checksums.txt" && checksums != nil:
			_, _ = w.Write([]byte(checksums(pacote, hex.EncodeToString(h[:]))))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	binDir := t.TempDir()
	cmd := exec.Command("sh", append([]string{filepath.Join("..", "..", "install.sh"),
		"--version=v9.9.9", "--download-base=" + srv.URL}, args...)...)
	cmd.Env = append(os.Environ(), "ARKAME_BIN_DIR="+binDir, "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	_, errBin := os.Stat(filepath.Join(binDir, "arkame-agent"))
	return string(out), err == nil, errBin == nil
}

// Sem conferir o checksum, o install.sh parava só com --skip-checksum: um
// proxy que bloqueasse o checksums.txt fazia o binário ser instalado sem
// conferência, com um aviso.
func TestInstallShParaSemConferirOChecksum(t *testing.T) {
	casos := map[string]func(pacote, sha string) string{
		"checksums.txt não baixa": nil,
		"sem a linha do pacote": func(_, sha string) string {
			return sha + "  outro_pacote.tar.gz\n"
		},
		"checksum diferente": func(pacote, _ string) string {
			return strings.Repeat("0", 64) + "  " + pacote + "\n"
		},
	}
	for nome, checksums := range casos {
		t.Run(nome, func(t *testing.T) {
			out, terminou, instalou := rodarInstallSh(t, checksums)
			if terminou || instalou {
				t.Fatalf("instalou sem conferir o checksum (terminou=%v, binário=%v):\n%s", terminou, instalou, out)
			}
			if nome != "checksum diferente" && !strings.Contains(out, "--skip-checksum") {
				t.Fatalf("a mensagem não diz como seguir sem conferir:\n%s", out)
			}
		})
	}
}

func TestInstallShComChecksumOuSkipChecksum(t *testing.T) {
	out, terminou, instalou := rodarInstallSh(t, func(pacote, sha string) string {
		return sha + "  " + pacote + "\n"
	})
	if !terminou || !instalou || !strings.Contains(out, "Checksum conferido") {
		t.Fatalf("checksum certo: terminou=%v binário=%v\n%s", terminou, instalou, out)
	}
	out, terminou, instalou = rodarInstallSh(t, nil, "--skip-checksum")
	if !terminou || !instalou || !strings.Contains(out, "sem conferir") {
		t.Fatalf("--skip-checksum: terminou=%v binário=%v\n%s", terminou, instalou, out)
	}
}

// No Git Bash do Windows, o install.sh mandava baixar o .zip e rodar
// "arkame-agent.exe install": sem Program Files, sem a entrada para
// desinstalar e dependendo de o terminal já ser administrador. Agora mostra
// o comando oficial (agente.exe + setup), com o código quando veio.
func TestInstallShNoWindowsMostraOComandoOficial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simula o Git Bash com um uname falso")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sem sh")
	}
	falso := t.TempDir()
	if err := os.WriteFile(filepath.Join(falso, "uname"), []byte("#!/bin/sh\necho MINGW64_NT-10.0-19045\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for token, quer := range map[string]string{
		"atk_abcdefghijklmnopqr": "setup --token=atk_abcdefghijklmnopqr || pause",
		"":                       "setup --token=<código> || pause",
	} {
		args := []string{filepath.Join("..", "..", "install.sh"), "--version=v9.9.9"}
		if token != "" {
			args = append(args, "--token="+token)
		}
		cmd := exec.Command("sh", args...)
		cmd.Env = append(os.Environ(), "PATH="+falso+string(os.PathListSeparator)+os.Getenv("PATH"), "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("no Windows o install.sh tem de parar:\n%s", out)
		}
		oficial := `cmd /c "curl -fsSLo "%TEMP%\arkame-agent.exe" https://get.arkame.app/agente.exe && "%TEMP%\arkame-agent.exe" `
		if !strings.Contains(string(out), oficial+quer) {
			t.Fatalf("sem o comando oficial com %q:\n%s", quer, out)
		}
		if strings.Contains(string(out), ".zip") || strings.Contains(string(out), " install --token") {
			t.Fatalf("ainda manda o .zip e o install:\n%s", out)
		}
	}
}
