package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/localstore"
	"github.com/arkame-app/agent/internal/segredo"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/internal/storage"
)

// newLocalStorageCmd: a cópia local ("local primeiro, nuvem depois").
func newLocalStorageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "local-storage",
		Short: "Cópia local: um armazenamento nesta máquina, gerido pelo agente",
	}
	cmd.AddCommand(newLocalStorageInstallCmd())
	return cmd
}

// opcoesDaCopiaLocal são as do `local-storage install`.
type opcoesDaCopiaLocal struct {
	storageID   string
	bucket      string
	pasta       string
	porta       int
	config      string
	servico     string
	servicoMain string
	// Para o teste de ponta a ponta (escondidas): sem instalar serviço, com
	// um RustFS já em disco e a pasta do programa noutro lugar.
	semServico     bool
	programaPronto string
	pastaPrograma  string
}

var (
	reUUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reBucketLocal = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// validar confere o que veio do painel e da pessoa, antes de mexer em nada.
func (o opcoesDaCopiaLocal) validar() error {
	if !reUUID.MatchString(o.storageID) {
		return fmt.Errorf("--storage-id tem de ser o id do armazenamento no painel (um UUID)")
	}
	if !reBucketLocal.MatchString(o.bucket) {
		return fmt.Errorf("--bucket inválido: de 3 a 63 letras minúsculas, números e hífens")
	}
	if o.porta < 1024 || o.porta > 65535 {
		return fmt.Errorf("--port tem de estar entre 1024 e 65535")
	}
	limpa := filepath.Clean(o.pasta)
	if !filepath.IsAbs(limpa) {
		return fmt.Errorf("--dir tem de ser um caminho absoluto: %s", o.pasta)
	}
	if runtime.GOOS != "windows" {
		// O serviço do agente vê /etc, /usr e /boot só para leitura
		// (ProtectSystem=full): o RustFS não gravaria ali.
		for _, proibida := range []string{"/etc", "/usr", "/boot", "/proc", "/sys", "/dev"} {
			if limpa == proibida || strings.HasPrefix(limpa, proibida+"/") {
				return fmt.Errorf("--dir não pode ficar em %s (o serviço do agente só lê ali); use outro disco, ou /srv, /var ou /mnt", proibida)
			}
		}
		if limpa == "/" {
			return fmt.Errorf("--dir não pode ser a raiz")
		}
	}
	return nil
}

// pastaDoPrograma é onde o RustFS fica instalado.
func pastaDoPrograma() string {
	if runtime.GOOS == "windows" {
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return filepath.Join(pf, "Arkame", "rustfs")
	}
	return "/opt/arkame/rustfs"
}

// linhasDaCopiaLocal monta o env-file do processo da cópia local: a MESMA
// identidade do agente principal (é o mesmo servidor no painel, e os planos
// encadeados — local primeiro, nuvem depois — exigem o mesmo servidor), o
// bucket local, e os buckets do principal como irmãos (os planos deles ficam
// para ele).
func linhasDaCopiaLocal(main *config.Config, o opcoesDaCopiaLocal, chave, segredo, programa, pastaDasChaves string) []string {
	endereco := "127.0.0.1:" + strconv.Itoa(o.porta)
	irmaos := slices.DeleteFunc(append([]string{main.StorageBucket}, main.SiblingBuckets...), func(b string) bool { return b == "" || b == o.bucket })
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	return []string{
		"PANEL_URL=" + main.PanelURL,
		"AGENT_ID=" + main.AgentID,
		"TOKEN_PATH=" + abs(main.TokenPath),
		"PRIVATE_KEY_PATH=" + abs(main.PrivateKeyPath),
		"AGENT_ID_PATH=" + abs(main.AgentIDPath),
		"STORAGE_ID=" + o.storageID,
		"STORAGE_BUCKET=" + o.bucket,
		"STORAGE_ENDPOINT=http://" + endereco,
		"STORAGE_REGION=" + config.DefaultRegion,
		"STORAGE_ACCESS_KEY=" + chave,
		"STORAGE_SECRET_KEY=" + segredo,
		"SIBLING_BUCKETS=" + strings.Join(irmaos, ","),
		"ARKAME_LOCAL_STORE_PROGRAM=" + programa,
		"ARKAME_LOCAL_STORE_DATA=" + filepath.Clean(o.pasta),
		"ARKAME_LOCAL_STORE_ADDRESS=" + endereco,
		"ARKAME_LOCAL_STORE_ACCESS_KEY_FILE=" + filepath.Join(pastaDasChaves, "access.key"),
		"ARKAME_LOCAL_STORE_SECRET_KEY_FILE=" + filepath.Join(pastaDasChaves, "secret.key"),
		"ARKAME_LOCAL_STORE_LOG=" + filepath.Join(filepath.Dir(filepath.Clean(o.pasta)), "arkame-rustfs.log"),
	}
}

// irmaosDoPrincipal: os buckets que o principal deixa para os irmãos, com o local.
func irmaosDoPrincipal(main *config.Config, bucketLocal string) string {
	irmaos := append([]string{}, main.SiblingBuckets...)
	if !slices.Contains(irmaos, bucketLocal) {
		irmaos = append(irmaos, bucketLocal)
	}
	return strings.Join(irmaos, ",")
}

// credencialLocal lê a credencial do RustFS, ou cria uma nova (20 e 40
// caracteres) — reinstalar mantém a mesma, e o que já está no bucket local
// continua acessível.
func credencialLocal(pasta string) (string, string, error) {
	ak, sk := filepath.Join(pasta, "access.key"), filepath.Join(pasta, "secret.key")
	a, errA := os.ReadFile(ak)
	s, errS := os.ReadFile(sk)
	if errA == nil && errS == nil && len(a) >= 16 && len(s) >= 32 {
		return strings.TrimSpace(string(a)), strings.TrimSpace(string(s)), nil
	}
	if err := segredo.CriarPasta(pasta); err != nil {
		return "", "", err
	}
	chave := "ARKLOCAL" + strings.ToUpper(aleatorio(9))[:12]
	segredoNovo := aleatorio(30)
	if err := segredo.Gravar(ak, []byte(chave), 0o600); err != nil {
		return "", "", err
	}
	if err := segredo.Gravar(sk, []byte(segredoNovo), 0o600); err != nil {
		return "", "", err
	}
	return chave, segredoNovo, nil
}

// aleatorio: n bytes aleatórios em base64url, sem os caracteres que o RustFS
// ou um env-file poderiam ler de outro jeito.
func aleatorio(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.NewReplacer("-", "x", "_", "y").Replace(base64.RawURLEncoding.EncodeToString(b))
}

func newLocalStorageInstallCmd() *cobra.Command {
	var o opcoesDaCopiaLocal
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Instala a cópia local: o RustFS nesta máquina e o processo do agente que grava nele",
		Long: `Instala a cópia local deste servidor, com os dados que o painel mostra
(Armazenamentos → Cópia local): baixa o RustFS ` + localstore.Versao + ` (conferido pelo SHA-256),
cria a pasta dos dados fechada para o administrador, gera a credencial, cria o
bucket versionado e instala um segundo serviço do agente (o mesmo servidor no
painel) que mantém o RustFS de pé e faz os backups locais. O RustFS escuta só
em 127.0.0.1: a cópia local não é um serviço da rede.

Prefira uma pasta noutro disco que não o dos dados: a cópia local serve para
restaurar rápido; a proteção contra um ataque é a cópia na nuvem.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return instalarCopiaLocal(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&o.storageID, "storage-id", "", "id do armazenamento da cópia local no painel")
	cmd.Flags().StringVar(&o.bucket, "bucket", "", "nome do bucket da cópia local (o que o painel mostra)")
	cmd.Flags().StringVar(&o.pasta, "dir", "", "pasta dos dados da cópia local (de preferência noutro disco)")
	cmd.Flags().IntVar(&o.porta, "port", 9900, "porta do RustFS em 127.0.0.1")
	cmd.Flags().StringVar(&o.config, "config", "", "env-file do agente principal (padrão: o da instalação)")
	cmd.Flags().StringVar(&o.servico, "service-name", "arkame-agent-local", "nome do serviço da cópia local")
	cmd.Flags().StringVar(&o.servicoMain, "main-service-name", service.DefaultName, "nome do serviço do agente principal")
	cmd.Flags().BoolVar(&o.semServico, "no-service", false, "")
	cmd.Flags().StringVar(&o.programaPronto, "rustfs-program", "", "")
	cmd.Flags().StringVar(&o.pastaPrograma, "program-dir", "", "")
	for _, h := range []string{"no-service", "rustfs-program", "program-dir"} {
		_ = cmd.Flags().MarkHidden(h)
	}
	for _, f := range []string{"storage-id", "bucket", "dir"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

type escritor interface{ Write([]byte) (int, error) }

func instalarCopiaLocal(ctx context.Context, o opcoesDaCopiaLocal, out escritor) error {
	if err := o.validar(); err != nil {
		return err
	}
	if o.config == "" {
		o.config = config.DefaultPath
	}
	main, err := config.Load(o.config, config.Overrides{})
	if err != nil {
		return fmt.Errorf("lendo a configuração do agente (%s): %w", o.config, err)
	}
	if main.AgentID == "" || main.StorageBucket == "" {
		return fmt.Errorf("o agente principal ainda não está instalado e aprovado (%s sem AGENT_ID ou STORAGE_BUCKET)", o.config)
	}
	if main.HostRoot != "/" && main.HostRoot != "" {
		return fmt.Errorf("a cópia local não roda em contêiner (HOST_ROOT=%s): instale o agente direto no servidor", main.HostRoot)
	}
	if o.bucket == main.StorageBucket {
		return fmt.Errorf("o bucket da cópia local não pode ser o mesmo do agente principal (%s)", o.bucket)
	}
	artefato, err := localstore.ArtefatoDaPlataforma()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Cópia local: %s, bucket %s, em 127.0.0.1:%d\n", filepath.Clean(o.pasta), o.bucket, o.porta)

	// 1. A pasta dos dados, fechada para o administrador.
	if err := segredo.PastaPrivada(filepath.Clean(o.pasta)); err != nil {
		return fmt.Errorf("pasta dos dados: %w", err)
	}
	// 2. O programa, conferido pelo SHA-256 (só baixa se ainda não está lá).
	pastaProg := pastaDoPrograma()
	if o.pastaPrograma != "" {
		pastaProg = o.pastaPrograma
	}
	programa := filepath.Join(pastaProg, artefato.Programa)
	if o.programaPronto != "" {
		programa = o.programaPronto
	} else if _, err := os.Stat(programa); err != nil {
		fmt.Fprintf(out, "Baixando o RustFS %s (cerca de 200 MB)…\n", localstore.Versao)
		if programa, err = localstore.Baixar(ctx, artefato, pastaProg); err != nil {
			return err
		}
	}
	// 3. A credencial, ao lado da configuração do agente.
	pastaDasChaves := filepath.Join(filepath.Dir(o.config), "rustfs")
	chave, segredoLocal, err := credencialLocal(pastaDasChaves)
	if err != nil {
		return fmt.Errorf("credencial da cópia local: %w", err)
	}
	// 4. O bucket versionado: sobe o RustFS aqui mesmo, só para criar (se o
	// serviço da cópia local já está rodando, ele é adotado).
	linhas := linhasDaCopiaLocal(main, o, chave, segredoLocal, programa, pastaDasChaves)
	localCfgPath := filepath.Join(filepath.Dir(o.config), "agent-local.env")
	sup := &localstore.Supervisor{
		Programa: programa, Dados: filepath.Clean(o.pasta), Endereco: "127.0.0.1:" + strconv.Itoa(o.porta),
		ArquivoDaChave: filepath.Join(pastaDasChaves, "access.key"), ArquivoDoSegredo: filepath.Join(pastaDasChaves, "secret.key"),
	}
	subCtx, parar := context.WithCancel(ctx)
	fim := make(chan struct{})
	go func() { sup.Rodar(subCtx); close(fim) }()
	errBucket := func() error {
		if err := sup.Pronto(ctx, time.Minute); err != nil {
			return err
		}
		c, err := storage.NewS3Client(ctx, &config.Config{StorageAccessKey: chave, StorageSecretKey: segredoLocal, StorageEndpoint: "http://" + sup.Endereco, StorageRegion: config.DefaultRegion})
		if err != nil {
			return err
		}
		return localstore.PrepararBucket(ctx, c, o.bucket)
	}()
	parar()
	<-fim
	if errBucket != nil {
		return fmt.Errorf("preparando o bucket da cópia local: %w", errBucket)
	}
	// 5. As configurações: a da cópia local, e o principal passa a deixar o
	// bucket local para o irmão.
	if err := setup.Gravar(localCfgPath, linhas); err != nil {
		return err
	}
	if err := setup.Gravar(o.config, []string{"SIBLING_BUCKETS=" + irmaosDoPrincipal(main, o.bucket)}); err != nil {
		return err
	}
	if o.semServico {
		fmt.Fprintf(out, "Configuração gravada em %s (sem serviço: rode `run --config` nela).\n", localCfgPath)
		return nil
	}
	// 6. O serviço da cópia local, e o principal reiniciado com os irmãos novos.
	localCfg, err := config.Load(localCfgPath, config.Overrides{})
	if err != nil {
		return err
	}
	inst, err := service.Install(ctx, localCfg, service.Options{Name: o.servico, Scope: service.ScopeSystem, Start: true})
	if err != nil {
		return fmt.Errorf("instalando o serviço da cópia local: %w", err)
	}
	if args := service.RestartArgs(o.servicoMain, service.ScopeSystem); len(args) > 0 {
		if saida, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(out, "Aviso: reinicie o serviço %s para ele deixar o bucket local para a cópia local (%v: %s)\n", o.servicoMain, err, strings.TrimSpace(string(saida)))
		}
	}
	fmt.Fprintf(out, "Pronto. Serviço %s instalado (%s).\n", inst.Name, inst.StatusCmd)
	fmt.Fprintln(out, "No painel, o armazenamento da cópia local fica disponível em alguns minutos, depois do primeiro teste do bucket.")
	return nil
}
