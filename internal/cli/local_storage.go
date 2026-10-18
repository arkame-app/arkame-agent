package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/atualizacao"
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
	cmd.AddCommand(newLocalStorageInstallCmd(), newLocalStorageMoveCmd(), newLocalStorageShareCmd(), newLocalStorageJoinCmd())
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
	// raizDoHost: no agente em contêiner (Docker), onde a raiz do servidor
	// está montada (/host). A pasta dos dados (--dir) é a do servidor; o
	// programa, as chaves e o log ficam no volume da configuração, e quem
	// roda a cópia local é um segundo contêiner (o painel mostra o comando).
	raizDoHost string
}

// emConteiner: a cópia local montada de dentro do contêiner do agente.
func (o opcoesDaCopiaLocal) emConteiner() bool { return o.raizDoHost != "" && o.raizDoHost != "/" }

// pastaReal: a pasta dos dados como este processo a vê (com a raiz do host
// na frente, no contêiner).
func (o opcoesDaCopiaLocal) pastaReal() string {
	if o.emConteiner() {
		return filepath.Join(o.raizDoHost, filepath.Clean(o.pasta))
	}
	return filepath.Clean(o.pasta)
}

// raizDoHostPadrao: a do contêiner do agente (a imagem marca
// ARKAME_INSTALL_METHOD=docker e monta a raiz do servidor em /host).
func raizDoHostPadrao() string {
	if v := os.Getenv("HOST_ROOT"); v != "" {
		return v
	}
	if os.Getenv("ARKAME_INSTALL_METHOD") == "docker" {
		return "/host"
	}
	return ""
}

// ImagemDoAgente é a imagem publicada do agente (para os comandos de Docker).
const imagemDoAgente = "ghcr.io/arkame-app/arkame-agent:latest"

// comandoDoConteinerIrmao: o docker run do contêiner de um processo irmão
// (a cópia local, uma cópia da rede), com a rede do host: a cópia local
// escuta em 127.0.0.1 (e na rede, com o share) como no agente nativo.
func comandoDoConteinerIrmao(nome, configPath string) string {
	return fmt.Sprintf("sudo docker run -d --name %s --restart always --stop-timeout 150 --user 0 --security-opt label=disable --network host -v /:/host -v /etc/arkame:/etc/arkame %s run --host-root /host --config %s", nome, imagemDoAgente, configPath)
}

var (
	reUUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reBucketLocal = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// pastasTemporarias: onde o sistema apaga o que fica velho. Variável para os
// testes, que montam a cópia local dentro da pasta temporária.
var pastasTemporarias = []string{"/tmp", "/var/tmp"}

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
		// O systemd-tmpfiles apaga o que fica velho ali: a cópia local
		// perderia as versões antigas, que são justamente as que se restauram.
		for _, temp := range pastasTemporarias {
			if limpa == temp || strings.HasPrefix(limpa, temp+"/") {
				return fmt.Errorf("--dir não pode ficar em %s (o sistema apaga arquivos antigos ali); use outro disco, ou /srv, /var ou /mnt", temp)
			}
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
// programaDaCopia é o RustFS que o install deixou: caminho e SHA-256.
type programaDaCopia struct{ caminho, sha256 string }

func linhasDaCopiaLocal(main *config.Config, o opcoesDaCopiaLocal, chave, segredo string, prog programaDaCopia, pastaDasChaves, pastaTLS, log string) []string {
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
		"STORAGE_ENDPOINT=https://" + endereco,
		"STORAGE_CA_FILE=" + filepath.Join(pastaTLS, localstore.ArquivoDoCertificado),
		"STORAGE_REGION=" + config.DefaultRegion,
		"STORAGE_ACCESS_KEY=" + chave,
		"STORAGE_SECRET_KEY=" + segredo,
		"SIBLING_BUCKETS=" + strings.Join(irmaos, ","),
		// Quem troca o programa é o agente principal; este processo segue o
		// principal (reinicia com ele) e não se atualiza sozinho.
		"ARKAME_AUTO_UPDATE=" + atualizacao.ValorSegue,
		"ARKAME_LOCAL_STORE_PROGRAM=" + prog.caminho,
		"ARKAME_LOCAL_STORE_PROGRAM_SHA256=" + prog.sha256,
		"ARKAME_LOCAL_STORE_VERSION=" + localstore.Versao,
		"ARKAME_LOCAL_STORE_TLS=" + pastaTLS,
		"ARKAME_LOCAL_STORE_DATA=" + o.pastaReal(),
		"ARKAME_LOCAL_STORE_ADDRESS=" + endereco,
		"ARKAME_LOCAL_STORE_ACCESS_KEY_FILE=" + filepath.Join(pastaDasChaves, "access.key"),
		"ARKAME_LOCAL_STORE_SECRET_KEY_FILE=" + filepath.Join(pastaDasChaves, "secret.key"),
		"ARKAME_LOCAL_STORE_LOG=" + log,
		// No contêiner, as pastas do plano são lidas pela raiz montada.
		"HOST_ROOT=" + o.raizDoHostOuBarra(),
	}
}

func (o opcoesDaCopiaLocal) raizDoHostOuBarra() string {
	if o.emConteiner() {
		return o.raizDoHost
	}
	return "/"
}

// chavesSoDoPrincipal não vão do env-file do principal para o da cópia local:
// a identidade e o armazenamento são escritos de propósito (linhasDaCopiaLocal),
// e o código de ativação já foi usado.
var chavesSoDoPrincipal = regexp.MustCompile(`^(AWS_[A-Z_0-9]+|STORAGE_[A-Z_]+|SIBLING_BUCKETS|ARKAME_LOCAL_STORE_[A-Z_0-9]+|ARKAME_AUTO_UPDATE|ENROLLMENT_TOKEN|PANEL_URL|AGENT_ID|AGENT_ID_PATH|TOKEN_PATH|PRIVATE_KEY_PATH|HOST_ROOT|ARKAME_ROLE)$`)

// opcoesDoPrincipal: o resto do env-file do principal (proxy, pasta do
// estado, concorrência, intervalos…), que a cópia local tem de seguir — atrás
// de um proxy obrigatório, sem ele o processo local nunca falaria com o painel.
func opcoesDoPrincipal(caminho string) ([]string, error) {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return nil, err
	}
	var linhas []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		k, _, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(l, "#") || chavesSoDoPrincipal.MatchString(strings.TrimSpace(k)) {
			continue
		}
		linhas = append(linhas, l)
	}
	return linhas, nil
}

// logDaCopiaLocal: no Linux, em /var/log/arkame (o serviço vê /etc só para
// leitura); nos outros, ao lado da configuração, na pasta só do
// administrador. Nunca na pasta de cima da dos dados, que pode ser de outro.
func logDaCopiaLocal(configPath string) string {
	if runtime.GOOS == "linux" && configPath == config.DefaultPath {
		return "/var/log/arkame/rustfs.log"
	}
	return filepath.Join(filepath.Dir(configPath), "rustfs.log")
}

// fecharPastaDosDados deixa a pasta dos dados só para o administrador. A pasta
// é dedicada: a que já existia só é fechada se estiver vazia ou já for da
// cópia local (só o .rustfs.sys do RustFS e o bucket); com outra coisa dentro, e aberta,
// é recusada — fechar mudaria o acesso de quem usa aquilo.
func fecharPastaDosDados(pasta, bucket string) error {
	if err := segredo.PastaPrivada(pasta); err != nil {
		return err
	}
	aberta, err := pastaAberta(pasta)
	if err != nil {
		return err
	}
	if !aberta {
		return nil
	}
	entradas, err := os.ReadDir(pasta)
	if err != nil {
		return err
	}
	for _, e := range entradas {
		// O que já está ali tem de ser pasta de verdade (nem link, nem
		// junção) e de dono confiável: numa pasta aberta, um usuário comum
		// plantava um link com o nome do bucket, e o RustFS (como root)
		// gravava o backup onde ele lê.
		fi, err := os.Lstat(filepath.Join(pasta, e.Name()))
		permitida := e.Name() == ".rustfs.sys" || e.Name() == bucket
		if permitida && err == nil && fi.Mode().Type() == os.ModeDir && segredo.ConferirDono(filepath.Join(pasta, e.Name())) == nil {
			continue
		}
		if permitida {
			return fmt.Errorf("%s/%s não é uma pasta da cópia local (é um link, ou de outro usuário): use uma pasta nova ou vazia", pasta, e.Name())
		}
		return fmt.Errorf("%s já tem arquivos e outros usuários da máquina podem lê-la: use uma pasta nova ou vazia (por exemplo %s), que o agente cria fechada", pasta, filepath.Join(pasta, "arkame-local"))
	}
	return fecharPasta(pasta)
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
em 127.0.0.1; para receber backups de outros servidores da rede, veja
"local-storage share".

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
	cmd.Flags().StringVar(&o.raizDoHost, "host-root", raizDoHostPadrao(), "no contêiner do agente: onde a raiz do servidor está montada (padrão: /host na imagem do agente)")
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
	if o.bucket == main.StorageBucket {
		return fmt.Errorf("o bucket da cópia local não pode ser o mesmo do agente principal (%s)", o.bucket)
	}
	artefato, err := localstore.ArtefatoDaPlataforma()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Cópia local: %s, bucket %s, em 127.0.0.1:%d\n", filepath.Clean(o.pasta), o.bucket, o.porta)
	if o.emConteiner() {
		// O programa no volume da configuração: o /opt do contêiner some
		// quando ele é recriado.
		if o.pastaPrograma == "" {
			o.pastaPrograma = filepath.Join(filepath.Dir(o.config), "rustfs-bin")
		}
		if _, err := os.Stat(o.raizDoHost); err != nil {
			return fmt.Errorf("a raiz do servidor não está montada em %s: rode o contêiner com -v /:%s", o.raizDoHost, o.raizDoHost)
		}
	}

	localCfgPath := filepath.Join(filepath.Dir(o.config), "agent-local.env")
	// 1. A pasta dos dados, fechada para o administrador.
	if err := fecharPastaDosDados(o.pastaReal(), o.bucket); err != nil {
		return fmt.Errorf("pasta dos dados: %w", err)
	}
	avisarDoDisco(o.pastaReal(), out)
	// 2. O programa: roda como root/SYSTEM, então só fica o que o SHA-256
	// confere e numa pasta que só o administrador troca.
	prog, err := programaConferido(ctx, o, artefato, localCfgPath, out)
	if err != nil {
		return err
	}
	// 3. A credencial e o certificado TLS, ao lado da configuração do agente.
	pastaDasChaves := filepath.Join(filepath.Dir(o.config), "rustfs")
	chave, segredoLocal, err := credencialLocal(pastaDasChaves)
	if err != nil {
		return fmt.Errorf("credencial da cópia local: %w", err)
	}
	pastaTLS := filepath.Join(pastaDasChaves, "tls")
	certificado, err := localstore.PrepararTLS(pastaTLS)
	if err != nil {
		return fmt.Errorf("certificado da cópia local: %w", err)
	}
	log := logDaCopiaLocal(o.config)
	if o.emConteiner() {
		log = filepath.Join(filepath.Dir(o.config), "rustfs.log")
	}
	if err := segredo.PastaPrivada(filepath.Dir(log)); err != nil {
		return fmt.Errorf("pasta do log da cópia local: %w", err)
	}
	// 4. O bucket versionado: sobe o RustFS aqui mesmo, só para criar (se o
	// serviço da cópia local já está rodando, ele é adotado).
	linhas := linhasDaCopiaLocal(main, o, chave, segredoLocal, prog, pastaDasChaves, pastaTLS, log)
	if extras, err := opcoesDoPrincipal(o.config); err == nil {
		linhas = append(extras, linhas...)
	}
	sup := &localstore.Supervisor{
		Programa: prog.caminho, Dados: o.pastaReal(), Endereco: "127.0.0.1:" + strconv.Itoa(o.porta),
		ArquivoDaChave: filepath.Join(pastaDasChaves, "access.key"), ArquivoDoSegredo: filepath.Join(pastaDasChaves, "secret.key"),
		PastaTLS: pastaTLS, SHA256: prog.sha256, Log: log,
		Conferir: func(p string) error { return service.ConferirAuxiliar(p) },
	}
	subCtx, parar := context.WithCancel(ctx)
	fim := make(chan struct{})
	go func() { sup.Rodar(subCtx); close(fim) }()
	errBucket := func() error {
		if err := sup.Pronto(ctx, time.Minute); err != nil {
			return fmt.Errorf("%w (se o serviço %s já está rodando de uma instalação anterior, pare-o e rode o install de novo; o log do RustFS está em %s)", err, o.servico, log)
		}
		c, err := storage.NewS3Client(ctx, &config.Config{StorageAccessKey: chave, StorageSecretKey: segredoLocal, StorageEndpoint: "https://" + sup.Endereco, StorageCAFile: certificado, StorageRegion: config.DefaultRegion})
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
	// As cópias da rede (`join`) deste agente também deixam o bucket local
	// para o processo dele.
	for _, irmao := range irmaosNaPasta(filepath.Dir(o.config), main.AgentID, localCfgPath) {
		if c, err := config.Load(irmao, config.Overrides{}); err == nil && !slices.Contains(c.SiblingBuckets, o.bucket) {
			if err := setup.Gravar(irmao, []string{"SIBLING_BUCKETS=" + irmaosDoPrincipal(c, o.bucket)}); err != nil {
				return err
			}
		}
	}
	if o.emConteiner() {
		// No Docker, o processo da cópia local é um segundo contêiner, e o
		// principal precisa reiniciar para deixar o bucket local para ele.
		fmt.Fprintf(out, "Configuração gravada em %s.\n\nSuba o contêiner da cópia local e reinicie os do agente:\n\n  sudo docker rm -f %s >/dev/null 2>&1; %s\n  sudo docker restart %s\n",
			localCfgPath, o.servico, comandoDoConteinerIrmao(o.servico, localCfgPath), strings.Join(append([]string{"arkame-agent"}, servicosDosIrmaos(filepath.Dir(o.config), main.AgentID, localCfgPath)...), " "))
		return nil
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
	reiniciarServicos(ctx, append([]string{o.servicoMain}, servicosDosIrmaos(filepath.Dir(o.config), main.AgentID, localCfgPath)...), o.bucket, out)
	fmt.Fprintf(out, "Pronto. Serviço %s instalado (%s).\n", inst.Name, inst.StatusCmd)
	fmt.Fprintln(out, "No painel, o armazenamento da cópia local fica disponível em alguns minutos, depois do primeiro teste do bucket.")
	return nil
}

// programaConferido devolve o RustFS da cópia local, conferido. O que já está
// em disco só é reaproveitado se for da mesma versão e tiver o SHA-256 que o
// install anterior registrou; senão, baixa de novo (conferindo o zip pelo
// SHA-256 embutido no agente). Nos dois casos, a pasta tem de ser só do
// administrador: quem a trocasse viraria root/SYSTEM no próximo início.
func programaConferido(ctx context.Context, o opcoesDaCopiaLocal, artefato localstore.Artefato, localCfgPath string, out escritor) (programaDaCopia, error) {
	conferir := func(p string) (programaDaCopia, error) {
		if err := service.ConferirAuxiliar(p); err != nil {
			return programaDaCopia{}, err
		}
		sha, err := localstore.HashDoArquivo(p)
		if err != nil {
			return programaDaCopia{}, err
		}
		return programaDaCopia{caminho: p, sha256: sha}, nil
	}
	if o.programaPronto != "" {
		return conferir(o.programaPronto)
	}
	pastaProg := pastaDoPrograma()
	if o.pastaPrograma != "" {
		pastaProg = o.pastaPrograma
	}
	programa := filepath.Join(pastaProg, artefato.Programa)
	if anterior, err := config.Load(localCfgPath, config.Overrides{}); err == nil && anterior.LocalStoreProgram == programa && anterior.LocalStoreProgramSHA != "" && versaoAnterior(localCfgPath) == localstore.Versao {
		if p, err := conferir(programa); err == nil && p.sha256 == anterior.LocalStoreProgramSHA {
			return p, nil
		}
	}
	fmt.Fprintf(out, "Baixando o RustFS %s (cerca de 200 MB)…\n", localstore.Versao)
	baixado, err := localstore.Baixar(ctx, artefato, pastaProg)
	if err != nil {
		return programaDaCopia{}, err
	}
	return conferir(baixado)
}

// versaoAnterior: a versão do RustFS que o install anterior deixou.
func versaoAnterior(localCfgPath string) string {
	b, err := os.ReadFile(localCfgPath)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ARKAME_LOCAL_STORE_VERSION="); ok {
			return v
		}
	}
	return ""
}
