package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/arkame-app/agent/internal/aplicativos"
	"github.com/arkame-app/agent/internal/service"
	"github.com/spf13/cobra"
)

// newSetupCmd é a instalação de um comando no Windows, sem PowerShell.
//
// O comando do painel era `powershell -ExecutionPolicy Bypass -Command
// "&([scriptblock]::Create((irm …)))"`: exatamente o formato que golpes usam
// para baixar e rodar código, e o Windows Defender o barrou como
// Trojan:Win32/Commando.A!ml num Windows real (fundador, 28/09). Agora o
// Executar chama o `curl.exe` que vem no Windows para baixar este programa, e
// ele faz o resto: pede administrador (o "Sim" do Windows), se copia para
// Program Files, entra no PATH e roda o `install` de lá — que pergunta e testa
// a chave, registra e instala o serviço.
func newSetupCmd() *cobra.Command {
	var (
		enrollmentToken string
		panelURL        string
		elevado         bool
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
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			destino := aplicativos.ProgramaInstalado()
			if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(destino)) {
				// Trocar o programa por cima exige o serviço parado: em uso, o
				// Windows não deixa sobrescrever.
				service.Parar(service.DefaultName)
				if err := copiarPrograma(exe, destino); err != nil {
					return fmt.Errorf("copiando o programa para %s: %w", destino, err)
				}
				fmt.Fprintln(os.Stderr, "  ✓ Programa em", destino)
			}
			if err := aplicativos.AdicionarAoPath(filepath.Dir(destino)); err != nil {
				fmt.Fprintln(os.Stderr, "  ! não consegui pôr no PATH:", err)
			}

			// O install roda do programa instalado, para o serviço apontar para
			// ele, e não para a cópia baixada na pasta temporária.
			args := []string{"install", "--token=" + enrollmentToken}
			if panelURL != "" {
				args = append(args, "--panel-url="+panelURL)
			}
			c := exec.CommandContext(cmd.Context(), destino, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return errors.New("a instalação não terminou: veja a mensagem acima")
			}
			fmt.Fprintln(os.Stderr, "\n  ✓ Pronto. O painel mostra o servidor e o teste do bucket.")
			return nil
		},
	}
	cmd.Flags().StringVar(&enrollmentToken, "token", "", "código de instalação gerado no painel (atk_…)")
	cmd.Flags().StringVar(&panelURL, "panel-url", "", "URL do painel (padrão: https://save.arkame.app)")
	cmd.Flags().BoolVar(&elevado, "elevado", false, "")
	_ = cmd.Flags().MarkHidden("elevado")
	return cmd
}

// copiarPrograma copia para o destino por um arquivo temporário ao lado, e
// troca de uma vez: um programa pela metade nunca fica no lugar do bom.
func copiarPrograma(de, para string) error {
	if err := os.MkdirAll(filepath.Dir(para), 0o755); err != nil {
		return err
	}
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
	if err := os.Rename(tmp, para); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
