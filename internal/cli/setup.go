package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/pkg/version"
	"github.com/spf13/cobra"
)

// newSetupCmd é a instalação de um comando no Windows, sem PowerShell.
//
// O comando do painel era `powershell -ExecutionPolicy Bypass -Command
// "&([scriptblock]::Create((irm …)))"`: exatamente o formato que golpes usam
// para baixar e rodar código, e o Windows Defender o barrou como
// Trojan:Win32/Commando.A!ml num Windows real (fundador, 28/09). Agora o
// Executar chama o `curl.exe` que vem no Windows para baixar este programa, e
// ele faz o resto: pede administrador (o "Sim" do Windows), confere o próprio
// checksum com o do release, se copia para Program Files, entra no PATH e roda o `install` de lá — que pergunta e testa
// a chave, registra e instala o serviço.
func newSetupCmd() *cobra.Command {
	var (
		enrollmentToken string
		panelURL        string
		elevado         bool
		pularChecksum   bool
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Instala o agente nesta máquina (Windows): copia o programa, registra e instala o serviço",
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			pausar := true
			defer func() {
				if pausar {
					esperarEnter(&err)
				}
			}()
			if reaberto, eerr := aplicativos.ElevarSeNecessario(); eerr != nil {
				return eerr
			} else if reaberto {
				pausar = false // a janela elevada segue daqui
				return nil
			}
			if !strings.HasPrefix(enrollmentToken, "atk_") {
				return errors.New("código de instalação ausente ou inválido: copie de novo o comando do painel")
			}

			fmt.Fprintln(os.Stderr, "\n  Instalador do agente Arkame")
			// O serviço que rodava o programa trocado e que o install, se não
			// terminar, deixaria no .old.
			reiniciarSeFalhar := ""
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			destino := aplicativos.ProgramaInstalado()
			if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(destino)) {
				// O programa baixado (curl.exe → get.arkame.app/agente.exe)
				// só entra em Program Files se for o publicado no release
				// desta versão: confere o SHA-256 dele com o checksums.txt,
				// como o install.ps1 e o install.sh fazem com o pacote.
				if err := verificarBaixado(cmd.Context(), clienteDoChecksum, releasesDoAgente,
					version.Version, nomeNoReleaseDeste(), exe, pularChecksum, os.Stderr); err != nil {
					return err
				}
				// O serviço segue rodando o programa antigo até o `install`
				// recriá-lo; se algo falhar aqui, ele continua de pé. Os
				// outros que rodam o mesmo exe (um segundo agente) são
				// anotados antes da troca, para reiniciar depois dela.
				rodando := service.RodandoOPrograma(destino)
				if err := copiarPrograma(exe, destino); err != nil {
					return fmt.Errorf("copiando o programa para %s: %w", destino, err)
				}
				fmt.Fprintln(os.Stderr, "  ✓ Programa em", destino)
				reiniciarOutrosDoPrograma(os.Stderr, rodando, service.DefaultName, service.Reiniciar)
				// O próprio fica para o install re-registrar — mas ele só
				// chega lá se terminar. Avisado de que o serviço rodava o
				// programa trocado, o install o reinicia se falhar antes.
				if slices.ContainsFunc(rodando, func(n string) bool { return strings.EqualFold(n, service.DefaultName) }) {
					reiniciarSeFalhar = service.DefaultName
				}
			}
			// Copiado agora ou já no lugar: a pasta e o programa passam aos
			// Administradores. Do administrador do primeiro setup, outro
			// administrador não reinstalava (o install recusa o dono).
			if err := aplicativos.DonoAdministradores(filepath.Dir(destino), destino); err != nil {
				fmt.Fprintln(os.Stderr, "  ! não consegui passar a pasta do programa aos Administradores:", err)
			}
			if err := aplicativos.AdicionarAoPath(filepath.Dir(destino)); err != nil {
				fmt.Fprintln(os.Stderr, "  ! não consegui pôr no PATH:", err)
			}

			// O install roda do programa instalado, para o serviço apontar para
			// ele, e não para a cópia baixada na pasta temporária. E este
			// processo sai logo em seguida, sem esperar: a cópia baixada fica
			// livre, e rodar o comando de novo com uma janela ainda aberta não
			// esbarra mais no arquivo em uso — o curl falhava com "(23) … on
			// write" (fundador, 28/09). A janela segue com o install, que espera
			// o Enter no fim.
			args := []string{"install", "--token=" + enrollmentToken, "--pause"}
			if reiniciarSeFalhar != "" {
				args = append(args, "--restart-on-failure="+reiniciarSeFalhar)
			}
			if panelURL != "" {
				args = append(args, "--panel-url="+panelURL)
			}
			c := exec.CommandContext(cmd.Context(), destino, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			// O caminho padrão da configuração (/etc/arkame) não tem unidade:
			// o serviço o resolve no disco do sistema, e o install tem de
			// resolver no mesmo — não no do perfil de quem roda.
			c.Dir = discoDoSistema()
			if err := c.Start(); err != nil {
				return fmt.Errorf("iniciando %s: %w", destino, err)
			}
			pausar = false
			return c.Process.Release()
		},
	}
	cmd.Flags().StringVar(&enrollmentToken, "token", "", "código de instalação gerado no painel (atk_…)")
	cmd.Flags().StringVar(&panelURL, "panel-url", "", "URL do painel (padrão: https://save.arkame.app)")
	cmd.Flags().BoolVar(&pularChecksum, "skip-checksum", false, "instalar mesmo sem conseguir baixar o checksums.txt do release (checksum diferente aborta sempre)")
	cmd.Flags().BoolVar(&elevado, "elevado", false, "")
	_ = cmd.Flags().MarkHidden("elevado")
	return cmd
}

// reiniciarOutrosDoPrograma reinicia os serviços que estavam rodando o
// programa trocado, menos o proprio: o `install` que o setup abre o
// re-registra e inicia (sempre com o nome padrão). Sem isso, um segundo
// agente no mesmo exe (arkame-agent-oci) seguia rodando o .old, sem aviso,
// até o próximo boot — no Linux e no macOS também: o processo segue com o
// arquivo antigo depois da troca. Devolve os que não reiniciaram, que saem num aviso.
func reiniciarOutrosDoPrograma(w io.Writer, rodando []string, proprio string, reiniciar func(string) error) []string {
	var falharam []string
	for _, nome := range rodando {
		if strings.EqualFold(nome, proprio) {
			continue
		}
		if err := reiniciar(nome); err != nil {
			fmt.Fprintf(w, "  ! não consegui reiniciar o serviço %s: %v\n", nome, err)
			falharam = append(falharam, nome)
			continue
		}
		fmt.Fprintln(w, "  ✓ Serviço", nome, "reiniciado com a versão nova")
	}
	if len(falharam) > 0 {
		fmt.Fprintln(w, "  ! Continuam na versão antiga:", strings.Join(falharam, ", "))
		fmt.Fprintln(w, "    Reinicie-os ("+comoReiniciar(runtime.GOOS)+") para a versão nova valer.")
	}
	return falharam
}

// comoReiniciar é onde o operador reinicia um serviço à mão, em cada SO.
func comoReiniciar(goos string) string {
	switch goos {
	case "windows":
		return "services.msc"
	case "darwin":
		return "sudo launchctl kickstart -k system/<label>, ou launchctl kickstart -k gui/$(id -u)/<label>"
	}
	return "sudo systemctl restart <serviço>, ou systemctl --user restart <serviço>"
}

// reiniciarSeOInstallFalhou: o install não terminou (chave errada ou
// cancelada, código vencido) e não chegou a re-registrar o serviço que rodava
// o programa trocado — ele segue no .old até o próximo boot. Reinicia-o, ou
// avisa que continua na versão antiga. Sem nome (o setup não trocou o
// programa, ou o serviço não rodava), nada a fazer.
func reiniciarSeOInstallFalhou(w io.Writer, errDoInstall error, nome string, reiniciar func(string) error) {
	if errDoInstall == nil || nome == "" {
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  A instalação não terminou, mas o programa já foi trocado.")
	reiniciarOutrosDoPrograma(w, []string{nome}, "", reiniciar)
}

func discoDoSistema() string {
	if d := os.Getenv("SystemDrive"); d != "" {
		return d + string(filepath.Separator)
	}
	return string(filepath.Separator)
}

// copiarPrograma copia para o destino por um arquivo temporário ao lado, e
// troca de uma vez: um programa pela metade nunca fica no lugar do bom. O
// programa em uso (o serviço rodando) não pode ser sobrescrito no Windows,
// mas pode ser renomeado: vai para .old, e sai na próxima vez. Se o .old
// anterior ainda estiver em uso (um segundo agente roda o mesmo exe, ou a
// janela foi fechada no meio do setup), o rename não o substitui — volta
// "Access is denied" —, e o atual sai com um nome único, como no install.ps1.
func copiarPrograma(de, para string) error {
	if err := os.MkdirAll(filepath.Dir(para), 0o755); err != nil {
		return err
	}
	limparAntigos(para)
	origem, err := os.Open(de)
	if err != nil {
		return err
	}
	defer origem.Close()
	tmp := para + ".novo"
	destino, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destino, origem); err != nil {
		destino.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := destino.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	antigo := nomeDoAntigo(para)
	if _, err := os.Stat(para); err == nil {
		if err := os.Rename(para, antigo); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, para); err != nil {
		_ = os.Rename(antigo, para) // devolve o que estava lá
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(antigo) // em uso, fica para a próxima
	return nil
}

// limparAntigos tira os .old e .old-<aleatório> de trocas anteriores ao lado
// do programa. O que ainda estiver em uso não sai, e fica para a próxima.
func limparAntigos(para string) {
	entradas, err := os.ReadDir(filepath.Dir(para))
	if err != nil {
		return
	}
	base := filepath.Base(para) + ".old"
	for _, e := range entradas {
		if n := e.Name(); n == base || strings.HasPrefix(n, base+"-") {
			_ = os.Remove(filepath.Join(filepath.Dir(para), n))
		}
	}
}

// nomeDoAntigo é para onde o programa atual sai: .old, ou .old-<aleatório>
// quando um .old que não saiu na limpeza (em uso) ainda ocupa o nome.
func nomeDoAntigo(para string) string {
	antigo := para + ".old"
	if _, err := os.Lstat(antigo); errors.Is(err, fs.ErrNotExist) {
		return antigo
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b) // nunca falha (crypto/rand, Go 1.24+)
	return antigo + "-" + hex.EncodeToString(b)
}
