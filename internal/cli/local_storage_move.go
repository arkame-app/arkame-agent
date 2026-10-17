package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

// newLocalStorageMoveCmd: trocar a pasta dos dados da cópia local, sem perder
// as versões. O disco encheu, ou chegou um disco novo: o cliente escolhe a
// pasta nova no painel e roda este comando (Hugo, 17/10 — no Zimbra Adubos foi
// preciso fazer à mão).
func newLocalStorageMoveCmd() *cobra.Command {
	var (
		nova        string
		cfgPath     string
		servico     string
		servicoMain string
	)
	cmd := &cobra.Command{
		Use:   "move",
		Short: "Muda a pasta dos dados da cópia local, levando o que já está nela",
		Long: `Para o serviço da cópia local, copia os dados (o bucket, as versões e a
configuração do RustFS) para a pasta nova, reinstala a cópia local apontando
para ela e religa o serviço. A pasta antiga fica como estava: depois de
conferir no painel, apague-a.

A pasta nova não pode existir com conteúdo, nem ficar dentro da antiga (ou a
antiga dentro dela), e o disco dela precisa ter espaço para os dados.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return moverCopiaLocal(cmd.Context(), nova, cfgPath, servico, servicoMain, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&nova, "dir", "", "a pasta nova dos dados da cópia local")
	cmd.Flags().StringVar(&cfgPath, "config", "", "env-file do agente principal (padrão: o da instalação)")
	cmd.Flags().StringVar(&servico, "service-name", "arkame-agent-local", "nome do serviço da cópia local")
	cmd.Flags().StringVar(&servicoMain, "main-service-name", service.DefaultName, "nome do serviço do agente principal")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

// Variáveis para os testes: parar e religar o serviço, e a instalação.
var (
	pararServicoDaCopia   = pararServicoDoSistema
	religarServicoDaCopia = religarServicoDoSistema
	instalarParaMover     = instalarCopiaLocal
)

func moverCopiaLocal(ctx context.Context, nova, cfgPath, servico, servicoMain string, out io.Writer) error {
	if cfgPath == "" {
		cfgPath = config.DefaultPath
	}
	localCfg := filepath.Join(filepath.Dir(cfgPath), "agent-local.env")
	atual, err := config.Load(localCfg, config.Overrides{})
	if err != nil {
		return fmt.Errorf("lendo a configuração da cópia local (%s): %w", localCfg, err)
	}
	antiga := filepath.Clean(atual.LocalStoreData)
	if antiga == "" || antiga == "." || atual.StorageID == "" || atual.StorageBucket == "" {
		return fmt.Errorf("esta máquina não tem cópia local instalada (%s sem a pasta dos dados)", localCfg)
	}
	porta := 9900
	if _, p, err := net.SplitHostPort(atual.LocalStoreAddress); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			porta = n
		}
	}
	o := opcoesDaCopiaLocal{storageID: atual.StorageID, bucket: atual.StorageBucket, pasta: nova, porta: porta, config: cfgPath, servico: servico, servicoMain: servicoMain}
	if err := o.validar(); err != nil {
		return err
	}
	nova = filepath.Clean(nova)
	if mesmaPasta(nova, antiga) {
		return fmt.Errorf("a cópia local já está em %s", antiga)
	}
	if dentroDaPasta(nova, antiga) || dentroDaPasta(antiga, nova) {
		return fmt.Errorf("a pasta nova (%s) e a atual (%s) não podem ficar uma dentro da outra", nova, antiga)
	}
	// A pasta nova: que não existe, ou uma pasta vazia (e não um link nem um
	// arquivo — na falha, só se apaga o que este comando criou).
	novaExistia := false
	if fi, err := os.Lstat(nova); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s já existe e não é uma pasta", nova)
		}
		if ents, err := os.ReadDir(nova); err != nil || len(ents) > 0 {
			return fmt.Errorf("a pasta nova %s já tem arquivos: escolha uma pasta vazia ou que ainda não existe", nova)
		}
		novaExistia = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("conferindo %s: %w", nova, err)
	}
	tamanho, err := tamanhoDaArvore(antiga)
	if err != nil {
		return fmt.Errorf("medindo os dados atuais (%s): %w", antiga, err)
	}
	if livre, ok := espacoLivreEm(primeiraQueExiste(nova)); ok && livre < tamanho+tamanho/10+(1<<30) {
		return fmt.Errorf("o disco de %s tem %s livres; os dados da cópia local ocupam %s (com folga, precisa de %s)",
			nova, bytesParaLer(livre), bytesParaLer(tamanho), bytesParaLer(tamanho+tamanho/10+(1<<30)))
	}
	fmt.Fprintf(out, "Cópia local: de %s para %s (%s)\n", antiga, nova, bytesParaLer(tamanho))

	// Parado, o RustFS não grava enquanto a cópia anda: a pasta nova sai
	// igual à antiga, com as versões.
	if err := pararServicoDaCopia(servico); err != nil {
		return fmt.Errorf("parando o serviço %s: %w", servico, err)
	}
	fmt.Fprintln(out, "  ✓ Serviço da cópia local parado")
	religarAntiga := func() {
		if err := religarServicoDaCopia(servico); err != nil {
			fmt.Fprintf(out, "  ! não consegui religar o serviço %s na pasta antiga: %v\n", servico, err)
		}
	}
	if err := copiarArvore(antiga, nova); err != nil {
		if novaExistia {
			esvaziar(nova)
		} else {
			_ = os.RemoveAll(nova)
		}
		religarAntiga()
		return fmt.Errorf("copiando os dados para %s (nada mudou; a cópia local segue em %s): %w", nova, antiga, err)
	}
	fmt.Fprintln(out, "  ✓ Dados copiados")
	if err := instalarParaMover(ctx, o, out); err != nil {
		religarAntiga()
		return fmt.Errorf("reinstalando a cópia local em %s: %w (os dados antigos seguem em %s; confira em %s qual pasta ficou configurada antes de rodar de novo)", nova, err, antiga, localCfg)
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintf(out, "\nA pasta antiga ficou em %s. Depois de conferir no painel, apague-a:\n  Remove-Item -Recurse -Force '%s'\n", antiga, antiga)
	} else {
		fmt.Fprintf(out, "\nA pasta antiga ficou em %s. Depois de conferir no painel, apague-a:\n  sudo rm -rf %s\n", antiga, antiga)
	}
	return nil
}

func mesmaPasta(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// dentroDaPasta: filho está dentro de pai (não é a própria).
func dentroDaPasta(filho, pai string) bool {
	rel, err := filepath.Rel(pai, filho)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// primeiraQueExiste: a pasta, ou a primeira acima dela que existe (para medir
// o disco de uma pasta que ainda vai ser criada).
func primeiraQueExiste(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		pai := filepath.Dir(p)
		if pai == p {
			return p
		}
		p = pai
	}
}

func tamanhoDaArvore(raiz string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(raiz, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += uint64(fi.Size())
			}
		}
		return nil
	})
	return total, err
}

// copiarArvore copia pastas e arquivos com as mesmas permissões. Link ou
// arquivo especial dentro dos dados do RustFS não é esperado: recusa, em vez
// de seguir para fora da pasta.
func copiarArvore(de, para string) error {
	return filepath.WalkDir(de, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(de, p)
		if err != nil {
			return err
		}
		alvo := filepath.Join(para, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(alvo, fi.Mode().Perm())
		case d.Type().IsRegular():
			return copiarArquivo(p, alvo, fi.Mode().Perm())
		default:
			return fmt.Errorf("%s não é pasta nem arquivo comum", p)
		}
	})
}

func copiarArquivo(de, para string, modo fs.FileMode) error {
	o, err := os.Open(de)
	if err != nil {
		return err
	}
	defer o.Close()
	d, err := os.OpenFile(para, os.O_CREATE|os.O_EXCL|os.O_WRONLY, modo)
	if err != nil {
		return err
	}
	if _, err := io.Copy(d, o); err != nil {
		d.Close()
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// esvaziar apaga o conteúdo de uma pasta que já existia vazia antes da cópia.
func esvaziar(p string) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(p, e.Name()))
	}
}

// religarServicoDoSistema religa o serviço parado por pararServicoDoSistema.
// No macOS o `bootout` descarrega o serviço, e o `kickstart` do Reiniciar não
// acha mais nada: volta com `bootstrap` do plist.
func religarServicoDoSistema(nome string) error {
	if runtime.GOOS == "darwin" {
		plist := "/Library/LaunchDaemons/" + service.LaunchdLabel(nome) + ".plist"
		if saida, err := exec.Command("launchctl", "bootstrap", "system", plist).CombinedOutput(); err != nil && !strings.Contains(string(saida), "already") {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(saida)))
		}
		return nil
	}
	return service.Reiniciar(nome)
}

// pararServicoDoSistema para o serviço da cópia local e espera ele parar.
func pararServicoDoSistema(nome string) error {
	switch runtime.GOOS {
	case "windows":
		service.Parar(nome)
		return nil
	case "darwin":
		saida, err := exec.Command("launchctl", "bootout", "system/"+service.LaunchdLabel(nome)).CombinedOutput()
		if err != nil && !strings.Contains(string(saida), "No such process") {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(saida)))
		}
		return nil
	default:
		saida, err := exec.Command("systemctl", "stop", nome).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(saida)))
		}
		return nil
	}
}
