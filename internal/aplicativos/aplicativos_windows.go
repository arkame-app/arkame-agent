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

const chaveDoApp = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\ArkameAgent`
const chaveDoPath = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// ElevarSeNecessario reabre este comando como administrador (o "Sim" do
// Windows) quando ele não está elevado — o Desinstalar de "Aplicativos
// instalados" chama o programa sem elevação, e remover serviço exige.
// Devolve true quando reabriu: este processo só deve sair.
func ElevarSeNecessario() (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	args := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		args = append(args, syscall.EscapeArg(a))
	}
	verbo, _ := windows.UTF16PtrFromString("runas")
	arquivo, _ := windows.UTF16PtrFromString(exe)
	parametros, _ := windows.UTF16PtrFromString(strings.Join(args, " "))
	if err := windows.ShellExecute(0, verbo, arquivo, parametros, nil, windows.SW_NORMAL); err != nil {
		return false, fmt.Errorf("sem permissão de administrador: %w", err)
	}
	return true, nil
}

// Registrar põe o agente em "Aplicativos instalados", com o Desinstalar
// chamando `uninstall`.
func Registrar(exe, versao string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, chaveDoApp, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("registrando em Aplicativos instalados: %w", err)
	}
	defer k.Close()
	valores := map[string]string{
		"DisplayName":     "Arkame — agente de backup",
		"DisplayVersion":  strings.TrimPrefix(versao, "v"),
		"Publisher":       "Arkame",
		"URLInfoAbout":    "https://arkame.app/docs",
		"InstallLocation": filepath.Dir(exe),
		"DisplayIcon":     exe,
		"UninstallString": fmt.Sprintf(`"%s" uninstall --pause`, exe),
	}
	for nome, v := range valores {
		if err := k.SetStringValue(nome, v); err != nil {
			return err
		}
	}
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	return nil
}

// Remover tira a entrada de "Aplicativos instalados", a pasta do PATH e o
// programa. O executável em uso não pode se apagar: um processo à parte o
// apaga logo depois que este termina. Só o arquivo e a pasta, se ela ficar
// vazia — nunca um `rmdir /s`.
func Remover(exe string) error {
	_ = registry.DeleteKey(registry.LOCAL_MACHINE, chaveDoApp)

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

	cmd := exec.Command("cmd.exe", "/c",
		fmt.Sprintf(`ping 127.0.0.1 -n 3 >nul & del /f /q "%s" & rmdir "%s"`, exe, pasta))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 /* DETACHED_PROCESS */, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agendando a remoção de %s: %w", exe, err)
	}
	return cmd.Process.Release()
}
