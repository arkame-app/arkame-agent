//go:build windows

package aplicativos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

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

// InitializeAcl e AddAce, que o x/sys/windows não expõe.
var (
	advapi32          = windows.NewLazySystemDLL("advapi32.dll")
	procInitializeAcl = advapi32.NewProc("InitializeAcl")
	procAddAce        = advapi32.NewProc("AddAce")
)

// DonoAdministradores dá a pasta do programa e o programa ao grupo
// Administradores (BUILTIN\Administrators, por SID) e refaz a herança da
// DACL da pasta.
//
// Com a política "Proprietário padrão de objetos criados por membros do grupo
// Administradores" em "Criador do objeto" (o padrão do Windows 10/11), o dono
// de C:\Program Files\Arkame era o administrador do primeiro setup, e o
// CREATOR OWNER herdado do Program Files virava uma entrada de Controle Total
// para ele. O serviço (SYSTEM) só aceita o programa se o dono e as entradas
// forem de Administradores, SYSTEM, TrustedInstaller ou de quem roda o
// install: outro administrador (ou o mesmo, com a conta já removida) não
// reinstalava. Trocar só o dono não basta — a entrada herdada continua com o
// SID antigo —, por isso a DACL é regravada com as entradas próprias dela, e
// o Windows recalcula as herdadas com o dono novo (e as propaga ao programa).
// Pasta com a herança cortada (DACL protegida) fica como está.
func DonoAdministradores(pasta, programa string) error {
	adm, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	for _, c := range []string{programa, pasta} {
		if err := windows.SetNamedSecurityInfo(c, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION, adm, nil, nil, nil); err != nil {
			return fmt.Errorf("dono de %s: %w", c, err)
		}
	}
	return reherdarDACL(pasta)
}

// reherdarDACL regrava a DACL da pasta só com as entradas próprias (sem as
// herdadas) e sem proteção: o Windows refaz as herdadas a partir da mãe.
func reherdarDACL(pasta string) error {
	sd, err := windows.GetNamedSecurityInfo(pasta, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("lendo as permissões de %s: %w", pasta, err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return err
	}
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return err
	}
	var proprias []*windows.ACCESS_ALLOWED_ACE
	tam := uint32(8) // o cabeçalho da ACL
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		proprias = append(proprias, ace)
		tam += uint32(ace.Header.AceSize)
	}
	tam = (tam + 3) &^ 3
	buf := make([]uint32, tam/4) // alinhado em DWORD
	nova := (*windows.ACL)(unsafe.Pointer(&buf[0]))
	revisao := uintptr(*(*byte)(unsafe.Pointer(dacl))) // o AclRevision da original
	if r, _, e := procInitializeAcl.Call(uintptr(unsafe.Pointer(nova)), uintptr(tam), revisao); r == 0 {
		return fmt.Errorf("montando a lista de permissões de %s: %w", pasta, e)
	}
	for _, ace := range proprias {
		if r, _, e := procAddAce.Call(uintptr(unsafe.Pointer(nova)), revisao, 0xFFFFFFFF,
			uintptr(unsafe.Pointer(ace)), uintptr(ace.Header.AceSize)); r == 0 {
			return fmt.Errorf("montando a lista de permissões de %s: %w", pasta, e)
		}
	}
	err = windows.SetNamedSecurityInfo(pasta, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nova, nil)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(sd)
	if err != nil {
		return fmt.Errorf("refazendo a herança de %s: %w", pasta, err)
	}
	return nil
}
