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
	"slices"
	"strings"
	"testing"
)

// pacoteFalso monta o tar.gz do release com um arkame-agent que só responde
// "version" (e sai com $ARKAME_FALSO_STATUS, para simular o install que falha).
func pacoteFalso(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := []byte("#!/bin/sh\necho arkame-agent falso\nexit ${ARKAME_FALSO_STATUS:-0}\n")
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
	return rodarInstallShNoHome(t, false, checksums, args...)
}

// rodarInstallShNoHome é o rodarInstallSh com, se noHome, o HOME apontando
// para a pasta que contém o destino do binário (a instalação sem root, que
// vai para ~/.local/bin).
func rodarInstallShNoHome(t *testing.T, noHome bool, checksums func(pacote, sha string) string, args ...string) (string, bool, bool) {
	t.Helper()
	binDir := t.TempDir()
	var env []string
	if noHome {
		env = append(env, "HOME="+filepath.Dir(binDir))
	}
	return rodarInstallShEm(t, binDir, env, checksums, args...)
}

// rodarInstallShEm roda o install.sh com o destino binDir (que pode já ter um
// programa) e as variáveis extras env.
func rodarInstallShEm(t *testing.T, binDir string, env []string, checksums func(pacote, sha string) string, args ...string) (string, bool, bool) {
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

	cmd := exec.Command("sh", append([]string{filepath.Join("..", "..", "install.sh"),
		"--version=v9.9.9", "--download-base=" + srv.URL}, args...)...)
	cmd.Env = append(append(os.Environ(), "ARKAME_BIN_DIR="+binDir, "NO_COLOR=1"), env...)
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

// Sem token e com o programa no home (instalação sem root), o install.sh
// mandava rodar `sudo ~/.local/bin/arkame-agent install`: um serviço root
// chamando um arquivo que o usuário troca. Agora sugere o install sem sudo
// (escopo user) ou o instalador com sudo; fora do home, segue o sudo.
func TestInstallShSemTokenNoHomeSugereSemSudo(t *testing.T) {
	certo := func(pacote, sha string) string { return sha + "  " + pacote + "\n" }

	out, terminou, _ := rodarInstallShNoHome(t, true, certo)
	if !terminou {
		t.Fatalf("não terminou:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "arkame-agent install --token") && strings.Contains(l, "sudo") {
			t.Fatalf("sugere sudo no programa do home: %q\n%s", l, out)
		}
	}
	if !strings.Contains(out, "/arkame-agent install --token=SEU_CODIGO") ||
		!strings.Contains(out, "| sudo sh -s -- --token=SEU_CODIGO") {
		t.Fatalf("sem o install sem sudo e o instalador com sudo:\n%s", out)
	}

	out, terminou, _ = rodarInstallSh(t, certo)
	if !terminou || !strings.Contains(out, "sudo /") || !strings.Contains(out, "/arkame-agent install --token=SEU_CODIGO") {
		t.Fatalf("fora do home, o próximo passo é com sudo:\n%s", out)
	}
}

// Com root, o install.sh punha o programa em /usr/local/bin sem conferir: no
// Mac Intel com Homebrew a pasta é do usuário, o agente recusava o programa
// para o serviço root, e o comando do painel falhava sempre. Agora, se a
// pasta (ou uma acima) não é só do root, vai para /opt/arkame/bin.
func TestInstallShComRootEscolheUmaPastaSoDoRoot(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh é do Linux e do macOS")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sem sh")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	escolha := func(padrao string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", `s=$1 p=$2; set --; . "$s"; BIN_DIR_ROOT=$p; bin_dir_de_root`, "sh", script, padrao)
		cmd.Env = append(os.Environ(), "ARKAME_INSTALL_SEM_MAIN=1", "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bin_dir_de_root %s: %v\n%s", padrao, err, out)
		}
		return string(out)
	}

	// Uma pasta do sistema (do root, 0755 até a raiz) fica.
	if got := escolha("/bin"); got != "/bin" {
		t.Errorf("/bin, do root, trocado por %q", got)
	}
	// Do usuário (como o /usr/local/bin do Homebrew): /opt/arkame/bin.
	if os.Geteuid() != 0 {
		if got := escolha(t.TempDir()); got != "/opt/arkame/bin" {
			t.Errorf("pasta do usuário mantida: %q", got)
		}
	}
	// Ainda não existe, debaixo de uma pasta que todos gravam (/tmp).
	if got := escolha("/tmp/arkame-nao-existe/bin"); got != "/opt/arkame/bin" {
		t.Errorf("pasta debaixo do /tmp mantida: %q", got)
	}
}

// O install.sh apagava o programa antes de copiar o novo: com a cópia
// falhando no meio (disco cheio), o antigo sumia, o novo ficava pela metade
// e o serviço não subia mais no próximo reinício. Agora o novo é copiado ao
// lado e entra por mv; se a cópia falha, o antigo fica e o temporário sai.
func TestInstallShCopiaQueFalhaMantemOProgramaAntigo(t *testing.T) {
	certo := func(pacote, sha string) string { return sha + "  " + pacote + "\n" }
	antigo := []byte("#!/bin/sh\necho arkame-agent antigo\n")
	semTemporario := func(t *testing.T, binDir string) {
		t.Helper()
		sobras, _ := filepath.Glob(filepath.Join(binDir, ".arkame-agent.novo.*"))
		if len(sobras) > 0 {
			t.Errorf("temporário deixado para trás: %v", sobras)
		}
	}

	t.Run("cópia falha", func(t *testing.T) {
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "arkame-agent"), antigo, 0o755); err != nil {
			t.Fatal(err)
		}
		// Um cp que escreve pela metade e falha, como no disco cheio.
		falso := t.TempDir()
		cp := "#!/bin/sh\nprintf parcial > \"$2\"\nexit 1\n"
		if err := os.WriteFile(filepath.Join(falso, "cp"), []byte(cp), 0o755); err != nil {
			t.Fatal(err)
		}
		path := "PATH=" + falso + string(os.PathListSeparator) + os.Getenv("PATH")
		out, terminou, _ := rodarInstallShEm(t, binDir, []string{path}, certo)
		if terminou {
			t.Fatalf("a cópia falhou e o install.sh terminou bem:\n%s", out)
		}
		got, err := os.ReadFile(filepath.Join(binDir, "arkame-agent"))
		if err != nil || !bytes.Equal(got, antigo) {
			t.Fatalf("o programa antigo não ficou (err=%v, conteúdo=%q):\n%s", err, got, out)
		}
		if !strings.Contains(out, "continua") {
			t.Errorf("a mensagem não diz que o programa antigo continua:\n%s", out)
		}
		semTemporario(t, binDir)
	})

	t.Run("cópia certa troca o antigo", func(t *testing.T) {
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "arkame-agent"), antigo, 0o755); err != nil {
			t.Fatal(err)
		}
		out, terminou, instalou := rodarInstallShEm(t, binDir, nil, certo)
		if !terminou || !instalou {
			t.Fatalf("terminou=%v binário=%v\n%s", terminou, instalou, out)
		}
		got, _ := os.ReadFile(filepath.Join(binDir, "arkame-agent"))
		if !bytes.Contains(got, []byte("falso")) {
			t.Fatalf("o novo não entrou no lugar: %q", got)
		}
		semTemporario(t, binDir)
	})
}

// systemctlFalso põe no PATH um systemctl que lista as units ativas dadas
// (nome → programa do ExecStart; "u:" na frente é do --user), respeitando o
// padrão de nome do list-units quando há um, responde ao
// show -p ExecStart e anota cada restart em restarts.log — com "novo" quando
// o programa já é o novo no momento do restart. As units com "falha" no nome
// não reiniciam.
func systemctlFalso(t *testing.T, binDir string, units map[string]string) (path string, restarts func() []string) {
	t.Helper()
	dir := t.TempDir()
	for nome, programa := range units {
		escopo, unit := "system", nome
		if u, ok := strings.CutPrefix(nome, "u:"); ok {
			escopo, unit = "user", u
		}
		f, err := os.OpenFile(filepath.Join(dir, escopo+"-units"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(f, "%s.service loaded active running Arkame Backup Agent (%s)\n", unit, unit)
		f.Close()
		exec := fmt.Sprintf("{ path=%s ; argv[]=%s run --config /etc/arkame/%s.env ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n", programa, programa, unit)
		if err := os.WriteFile(filepath.Join(dir, escopo+"-exec-"+unit+".service"), []byte(exec), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "restarts.log")
	script := `#!/bin/sh
d=` + dir + `
escopo=system; prefixo=""
if [ "$1" = "--user" ]; then escopo=user; prefixo="--user "; shift; fi
case "$1" in
  list-units)
    # Com padrão de nome (o argumento sem "-"), só as units que casam, como o
    # systemctl de verdade.
    padrao='*'; shift
    for a in "$@"; do case "$a" in -*) ;; *) padrao=$a ;; esac; done
    while read -r u resto; do
      case "$u" in $padrao) printf '%s %s\n' "$u" "$resto" ;; esac
    done < "$d/$escopo-units" 2>/dev/null ;;
  show) for a in "$@"; do u=$a; done; cat "$d/$escopo-exec-$u" 2>/dev/null ;;
  restart)
    estado=antigo; grep -q falso "` + filepath.Join(binDir, "arkame-agent") + `" 2>/dev/null && estado=novo
    echo "$prefixo$2 $estado" >> "$d/restarts.log"
    case "$2" in *falha*) echo "Job for $2 failed" >&2; exit 1 ;; esac ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), func() []string {
		b, _ := os.ReadFile(log)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "--user ", "--user:"))
	}
}

// Trocar o programa no Linux não reiniciava ninguém: o mv troca o inode e
// cada agente seguia rodando o programa antigo, sem aviso, até o próximo
// boot. Agora os agentes ativos que rodam o programa instalado reiniciam
// depois da troca; sem --token, todos (é atualização, e não há o que
// registrar); com --token, todos menos o --service-name, que o install
// reinicia.
func TestInstallShReiniciaOsAgentesDoPrograma(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd")
	}
	certo := func(pacote, sha string) string { return sha + "  " + pacote + "\n" }
	antigo := []byte("#!/bin/sh\necho arkame-agent antigo\n")
	// O backup-oci é um nome legado (até a v0.4.3 o install aceitava qualquer
	// nome): é agente porque chama o programa, não pelo nome. O nginx é ativo
	// e não é agente.
	prepararCom := func(t *testing.T, units func(bin string) map[string]string) (string, string, func() []string) {
		t.Helper()
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "arkame-agent"), antigo, 0o755); err != nil {
			t.Fatal(err)
		}
		path, restarts := systemctlFalso(t, binDir, units(filepath.Join(binDir, "arkame-agent")))
		return binDir, path, restarts
	}
	preparar := func(t *testing.T) (string, string, func() []string) {
		t.Helper()
		return prepararCom(t, func(bin string) map[string]string {
			return map[string]string{
				"arkame-agent":       bin,
				"arkame-agent-oci":   bin,
				"arkame-agent-falha": bin,
				"arkame-agent-outro": "/opt/outro/arkame-agent",
				"u:arkame-agent-usr": bin,
				"backup-oci":         bin,
				"nginx":              "/usr/sbin/nginx",
			}
		})
	}
	pares := func(r []string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(r); i += 2 {
			m[r[i]] = r[i+1]
		}
		return m
	}

	t.Run("sem token: todos, e não pede registro", func(t *testing.T) {
		binDir, path, restarts := preparar(t)
		out, terminou, _ := rodarInstallShEm(t, binDir, []string{path}, certo)
		if !terminou {
			t.Fatalf("não terminou:\n%s", out)
		}
		got := pares(restarts())
		quer := map[string]string{"arkame-agent": "novo", "arkame-agent-oci": "novo", "arkame-agent-falha": "novo", "--user:arkame-agent-usr": "novo", "backup-oci": "novo"}
		if fmt.Sprint(got) != fmt.Sprint(quer) {
			t.Fatalf("restarts = %v, queria %v (todos os do programa, depois da troca)\n%s", got, quer, out)
		}
		if !strings.Contains(out, "Continuam na versão antiga: arkame-agent-falha") {
			t.Fatalf("sem o aviso dos que ficaram na versão antiga:\n%s", out)
		}
		if strings.Contains(out, "registre este servidor") {
			t.Fatalf("atualização de servidor já instalado mandou registrar:\n%s", out)
		}
	})

	t.Run("sem token, só o legado: reinicia e não pede registro", func(t *testing.T) {
		binDir, path, restarts := prepararCom(t, func(bin string) map[string]string {
			return map[string]string{"backup-oci": bin, "nginx": "/usr/sbin/nginx"}
		})
		out, terminou, _ := rodarInstallShEm(t, binDir, []string{path}, certo)
		if !terminou {
			t.Fatalf("não terminou:\n%s", out)
		}
		if got := fmt.Sprint(restarts()); got != "[backup-oci novo]" {
			t.Fatalf("restarts = %v, queria [backup-oci novo]\n%s", got, out)
		}
		if strings.Contains(out, "registre este servidor") || !strings.Contains(out, "Atualização concluída") {
			t.Fatalf("servidor com agente de nome legado tratado como novo:\n%s", out)
		}
	})

	t.Run("com token: todos menos o --service-name", func(t *testing.T) {
		binDir, path, restarts := preparar(t)
		out, terminou, _ := rodarInstallShEm(t, binDir, []string{path}, certo,
			"--token=atk_abcdefghijklmnopqr", "--service-name=arkame-agent-oci", "--service-scope=system")
		if !terminou {
			t.Fatalf("não terminou:\n%s", out)
		}
		got := pares(restarts())
		quer := map[string]string{"arkame-agent": "novo", "arkame-agent-falha": "novo", "--user:arkame-agent-usr": "novo", "backup-oci": "novo"}
		if fmt.Sprint(got) != fmt.Sprint(quer) {
			t.Fatalf("restarts = %v, queria %v (o do install fica com o install)\n%s", got, quer, out)
		}
		if !strings.Contains(out, "Continuam na versão antiga: arkame-agent-falha") {
			t.Fatalf("sem o aviso:\n%s", out)
		}
	})

	t.Run("com token e install que falha: o próprio também", func(t *testing.T) {
		binDir, path, restarts := preparar(t)
		out, terminou, _ := rodarInstallShEm(t, binDir, []string{path, "ARKAME_FALSO_STATUS=3"}, certo,
			"--token=atk_abcdefghijklmnopqr", "--service-name=arkame-agent-oci", "--service-scope=system")
		if terminou {
			t.Fatalf("o install falhou e o install.sh terminou bem:\n%s", out)
		}
		if got := pares(restarts()); got["arkame-agent-oci"] != "novo" {
			t.Fatalf("o serviço do install que falhou não reiniciou: %v\n%s", got, out)
		}
	})
}

// O mesmo no macOS: os jobs app.arkame.* do launchd (inclusive os de nome
// legado, como app.arkame.backup-oci) que rodam o programa
// e estão rodando (launchctl print → state = running) reiniciam com
// kickstart -k no domínio de cada um.
func TestInstallShAgentesDoProgramaNoLaunchd(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh é do Linux e do macOS")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sem sh")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sistema, usuario, falso := filepath.Join(dir, "LaunchDaemons"), filepath.Join(dir, "LaunchAgents"), filepath.Join(dir, "falso")
	for _, d := range []string{sistema, usuario, falso} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin & cia", "arkame-agent")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := func(d, label, programa string) {
		t.Helper()
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
		p := "<plist><dict><key>Label</key><string>" + label + "</string>\n<key>ProgramArguments</key>\n<array>\n\t<string>" +
			esc(programa) + "</string>\n\t<string>run</string>\n</array></dict></plist>\n"
		if err := os.WriteFile(filepath.Join(d, label+".plist"), []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plist(sistema, "app.arkame.agent", bin)
	plist(sistema, "app.arkame.agent-oci", bin)
	plist(sistema, "app.arkame.agent-parado", bin)
	plist(sistema, "app.arkame.agent-outro", "/opt/outro/arkame-agent")
	plist(usuario, "app.arkame.agent-usr", bin)
	plist(sistema, "app.arkame.backup-oci", bin) // nome legado: agente pelo programa
	plist(sistema, "app.arkame.backup-outro", "/opt/outro/arkame-agent")
	launchctl := `#!/bin/sh
case "$1" in
  print) case "$2" in *parado*) echo "state = not running" ;; *) printf '%s = {
	state = running
}
' "$2" ;; esac ;;
  kickstart) echo "$3" >> "` + filepath.Join(dir, "kick.log") + `" ;;
esac
`
	if err := os.WriteFile(filepath.Join(falso, "launchctl"), []byte(launchctl), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `s=$1 sis=$2 usr=$3 bin=$4; set --; . "$s"
OS=darwin; LAUNCHD_DIR_SISTEMA=$sis; LAUNCHD_DIR_USUARIO=$usr
lista=$(agentes_do_programa "$bin")
printf '%s
' "$lista"
echo ---
reiniciar_agentes "$lista" system "$(label_launchd arkame-agent-oci)"`, "sh", script, sistema, usuario, bin)
	cmd.Env = append(os.Environ(), "ARKAME_INSTALL_SEM_MAIN=1", "NO_COLOR=1",
		"PATH="+falso+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lista, _, _ := strings.Cut(string(out), "---")
	linhas := strings.Split(strings.TrimSpace(lista), "\n")
	slices.Sort(linhas)
	if got := strings.Join(linhas, ","); got != "system app.arkame.agent,system app.arkame.agent-oci,system app.arkame.backup-oci,user app.arkame.agent-usr" {
		t.Fatalf("agentes do programa = %v\n%s", got, out)
	}
	kick, _ := os.ReadFile(filepath.Join(dir, "kick.log"))
	uid := fmt.Sprint(os.Getuid())
	kicks := strings.Fields(string(kick))
	slices.Sort(kicks)
	if got := fmt.Sprint(kicks); got != "[gui/"+uid+"/app.arkame.agent-usr system/app.arkame.agent system/app.arkame.backup-oci]" {
		t.Fatalf("kickstart = %v (o do --service-name fica com o install)\n%s", got, out)
	}
}
