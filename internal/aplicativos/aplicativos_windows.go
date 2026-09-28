//go:build windows

package aplicativos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const chaveDoPath = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// Uma entrada por serviço: com mais de um agente na máquina (--service-name),
// cada um tem o seu Desinstalar.
func chaveDoApp(servico string) string {
	k := `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\ArkameAgent`
	if servico != "" && servico != "arkame-agent" {
		k += "-" + servico
	}
	return k
}

// ElevarSeNecessario reabre este comando como administrador (o "Sim" do
// Windows) quando ele não está elevado — o Desinstalar de "Aplicativos
// instalados" chama o programa sem elevação, e remover serviço exige.
// Devolve true quando reabriu: este processo só deve sair. `--elevado` marca a
// cópia reaberta: se ela ainda não estiver elevada (UAC desligado para usuário
// comum), para com erro em vez de reabrir de novo, em laço.
func ElevarSeNecessario() (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}
	args := os.Args[1:]
	for _, a := range args {
		if a == "--elevado" {
			return false, fmt.Errorf("sem permissão de administrador: abra como administrador e rode de novo")
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	verbo, _ := windows.UTF16PtrFromString("runas")
	arquivo, _ := windows.UTF16PtrFromString(exe)
	parametros, _ := windows.UTF16PtrFromString(strings.TrimSpace(argsNaLinha(append(append([]string{}, args...), "--elevado"))))
	pasta, _ := windows.UTF16PtrFromString(mustWd())
	if err := windows.ShellExecute(0, verbo, arquivo, parametros, pasta, windows.SW_NORMAL); err != nil {
		return false, fmt.Errorf("sem permissão de administrador (o pedido do Windows foi recusado?): %w", err)
	}
	return true, nil
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}

// Registrar põe o agente em "Aplicativos instalados", com o Desinstalar
// chamando `uninstall` com o nome de serviço e o arquivo da instalação.
func Registrar(exe, versao, servico string, argsDoUninstall []string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, chaveDoApp(servico), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("registrando em Aplicativos instalados: %w", err)
	}
	defer k.Close()
	nome := "Arkame — agente de backup"
	if servico != "" && servico != "arkame-agent" {
		nome += " (" + servico + ")"
	}
	valores := map[string]string{
		"DisplayName":     nome,
		"DisplayVersion":  strings.TrimPrefix(versao, "v"),
		"Publisher":       "Arkame",
		"URLInfoAbout":    "https://arkame.app/docs#remover",
		"InstallLocation": filepath.Dir(exe),
		"DisplayIcon":     exe,
		"UninstallString": fmt.Sprintf(`"%s" uninstall --pause%s`, exe, argsNaLinha(argsDoUninstall)),
	}
	for n, v := range valores {
		if err := k.SetStringValue(n, v); err != nil {
			return err
		}
	}
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	return nil
}

func argsNaLinha(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(" " + syscall.EscapeArg(a))
	}
	return b.String()
}

// ProgramaInstalado é onde o programa mora no Windows.
func ProgramaInstalado() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Arkame", "arkame-agent.exe")
}

// AdicionarAoPath põe a pasta do programa no PATH da máquina, uma vez.
func AdicionarAoPath(pasta string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, chaveDoPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	atual, _, err := k.GetStringValue("Path")
	if err != nil {
		return err
	}
	for _, p := range strings.Split(atual, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(pasta, `\`)) {
			return nil
		}
	}
	return k.SetExpandStringValue("Path", strings.TrimRight(atual, ";")+";"+pasta)
}

// RemoverEntrada tira o serviço de "Aplicativos instalados".
func RemoverEntrada(servico string) {
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, chaveDoApp(servico))
}

// RemoverPrograma tira a pasta do PATH e apaga o programa. O executável em uso
// não pode se apagar: um cmd à parte tenta de segundo em segundo, por até dois
// minutos, até o arquivo ser liberado (este processo sair, o serviço parar).
// Só o arquivo, e a pasta se ficar vazia — nunca um `rmdir /s`.
//
// A linha do cmd vai montada à mão (SysProcAttr.CmdLine): o cmd.exe não segue
// as regras de aspas que o Go usa nos argumentos, e o `del` recebia o caminho
// com barras e aspas a mais — o programa nunca era apagado.
func RemoverPrograma(exe string) error {
	pasta := filepath.Dir(exe)
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, chaveDoPath, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if atual, _, err := k.GetStringValue("Path"); err == nil {
			var ficam []string
			for _, p := range strings.Split(atual, ";") {
				if p != "" && !strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(pasta, `\`)) {
					ficam = append(ficam, p)
				}
			}
			_ = k.SetExpandStringValue("Path", strings.Join(ficam, ";"))
		}
		k.Close()
	}

	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	linha := fmt.Sprintf(
		`"%s" /d /s /c "for /l %%i in (1,1,120) do @(del /f /q "%s" >nul 2>&1 & if not exist "%s" (rmdir "%s" >nul 2>&1 & exit /b 0) & ping -n 2 127.0.0.1 >nul)"`,
		comspec, exe, exe, pasta)
	cmd := exec.Command(comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: linha, CreationFlags: windows.DETACHED_PROCESS, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agendando a remoção de %s: %w", exe, err)
	}
	return cmd.Process.Release()
}

// ProgramaSaiDepois: no Windows o programa é apagado depois que este processo
// termina, não na hora.
const ProgramaSaiDepois = true
