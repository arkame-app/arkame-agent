package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/atualizacao"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/segredo"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/internal/storage"
)

// newLocalStorageJoinCmd: este servidor passa a gravar backups na cópia
// local de outro servidor da rede (o comando que o `share` de lá mostra).
//
// Confere o certificado da cópia local pela impressão digital (é ela, e não
// outra máquina no caminho), testa a chave que o `share` criou e instala um
// processo irmão do agente para aquele bucket, como o da cópia local: o mesmo
// servidor no painel, com a chave só nesta máquina.
func newLocalStorageJoinCmd() *cobra.Command {
	var o opcoesDoJoin
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Grava os backups deste servidor na cópia local de outro servidor da rede",
		Long: `Liga este servidor à cópia local de outro servidor da mesma rede, com o
comando que o "local-storage share" de lá mostrou. Pede a chave de acesso e
o segredo (ou leia de ARKAME_STORAGE_ACCESS_KEY e ARKAME_STORAGE_SECRET_KEY),
testa no bucket e instala o serviço que grava nele. Depois, no painel, crie
o plano deste servidor com aquela cópia local como destino.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ligarNaCopiaDaRede(cmd.Context(), o, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&o.storageID, "storage-id", "", "id do armazenamento no painel")
	cmd.Flags().StringVar(&o.bucket, "bucket", "", "bucket da cópia local")
	cmd.Flags().StringVar(&o.endpoint, "endpoint", "", "endereço da cópia local (https://IP:porta)")
	cmd.Flags().StringVar(&o.impressao, "fingerprint", "", "SHA-256 do certificado da cópia local")
	cmd.Flags().StringVar(&o.config, "config", "", "env-file do agente principal (padrão: o da instalação)")
	cmd.Flags().StringVar(&o.servico, "service-name", "", "nome do serviço (padrão: arkame-agent-rede-BUCKET)")
	cmd.Flags().StringVar(&o.servicoMain, "main-service-name", service.DefaultName, "nome do serviço do agente principal")
	cmd.Flags().BoolVar(&o.semServico, "no-service", false, "")
	_ = cmd.Flags().MarkHidden("no-service")
	for _, f := range []string{"storage-id", "bucket", "endpoint", "fingerprint"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

type opcoesDoJoin struct {
	storageID, bucket, endpoint, impressao string
	config, servico, servicoMain           string
	semServico                             bool
}

var reImpressao = regexp.MustCompile(`^[0-9a-f]{64}$`)

// reNomeDeServico: o nome que vai para systemctl/docker, sem nada que o shell interprete.
var reNomeDeServico = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,99}$`)

func (o *opcoesDoJoin) validar() (*url.URL, error) {
	if !reUUID.MatchString(o.storageID) {
		return nil, fmt.Errorf("--storage-id tem de ser o id do armazenamento no painel (um UUID)")
	}
	if !reBucketLocal.MatchString(o.bucket) {
		return nil, fmt.Errorf("--bucket inválido: de 3 a 63 letras minúsculas, números e hífens")
	}
	o.impressao = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(o.impressao), ":", ""))
	if !reImpressao.MatchString(o.impressao) {
		return nil, fmt.Errorf("--fingerprint inválido: são 64 caracteres hexadecimais (copie do comando que o share mostrou)")
	}
	u, err := url.Parse(o.endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("--endpoint inválido: use https://IP:porta, como o share mostrou")
	}
	if o.servico == "" {
		o.servico = "arkame-agent-rede-" + o.bucket
	}
	return u, nil
}

// buscarCertificado pega o certificado que a cópia local apresenta e confere
// pela impressão digital. Só ela decide: o certificado é autoassinado, e a
// impressão veio do dono da cópia local, pelo comando do share.
var buscarCertificado = func(ctx context.Context, host, impressao string) ([]byte, error) {
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // conferido pela impressão abaixo
	c, cancelar := context.WithTimeout(ctx, 15*time.Second)
	defer cancelar()
	conn, err := d.DialContext(c, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("não consegui falar com a cópia local em %s: %w (a porta está liberada no firewall de lá? os dois servidores estão na mesma rede?)", host, err)
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("a cópia local em %s não apresentou certificado", host)
	}
	h := sha256.Sum256(certs[0].Raw)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(impressao)) != 1 {
		return nil, fmt.Errorf("o certificado em %s não é o da cópia local (a impressão digital não confere): confira o endereço, ou rode o share de novo e use o comando novo", host)
	}
	nome, _, _ := net.SplitHostPort(host)
	if err := certs[0].VerifyHostname(nome); err != nil {
		return nil, fmt.Errorf("o certificado da cópia local não cobre %s: no servidor dela, rode o share com --ip %s", nome, nome)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Raw}), nil
}

// credencialDoJoin: do ambiente, ou perguntada no terminal (e testada).
var credencialDoJoin = func(ctx context.Context, alvo *config.Config) (string, string, error) {
	ak, sk := strings.TrimSpace(os.Getenv("ARKAME_STORAGE_ACCESS_KEY")), strings.TrimSpace(os.Getenv("ARKAME_STORAGE_SECRET_KEY"))
	if ak != "" && sk != "" {
		c := *alvo
		c.StorageAccessKey, c.StorageSecretKey = ak, sk
		if err := storage.Check(ctx, &c); err != nil {
			return "", "", fmt.Errorf("a chave de ARKAME_STORAGE_ACCESS_KEY não funciona no bucket %s: %s (%v)", alvo.StorageBucket, storage.Causa(err), err)
		}
		return ak, sk, nil
	}
	t, err := abrirTerminal()
	if err != nil {
		return "", "", fmt.Errorf("rode num terminal para digitar a chave, ou defina ARKAME_STORAGE_ACCESS_KEY e ARKAME_STORAGE_SECRET_KEY")
	}
	defer t.Close()
	return setup.PerguntarETestar(ctx, t, alvo)
}

func ligarNaCopiaDaRede(ctx context.Context, o opcoesDoJoin, out io.Writer) error {
	u, err := o.validar()
	if err != nil {
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
		return fmt.Errorf("o agente deste servidor ainda não está instalado e aprovado (%s sem AGENT_ID ou STORAGE_BUCKET)", o.config)
	}
	if o.bucket == main.StorageBucket || slices.Contains(main.SiblingBuckets, o.bucket) && arquivoDoIrmao(o.config, o.bucket) == "" {
		return fmt.Errorf("este servidor já grava no bucket %s por outro processo do agente", o.bucket)
	}
	if err := setup.PodeGravar(o.config); err != nil {
		return err
	}
	dir := filepath.Dir(o.config)
	// 1. O certificado, conferido pela impressão.
	pemCert, err := buscarCertificado(ctx, u.Host, o.impressao)
	if err != nil {
		return err
	}
	pastaRede := filepath.Join(dir, "rede")
	if err := segredo.CriarPasta(pastaRede); err != nil {
		return err
	}
	caFile := filepath.Join(pastaRede, o.bucket+".pem")
	if err := segredo.Gravar(caFile, pemCert, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ Certificado da cópia local conferido (%s)\n", u.Host)
	// 2. A chave, testada no bucket.
	alvo := &config.Config{StorageID: o.storageID, StorageBucket: o.bucket, StorageEndpoint: "https://" + u.Host, StorageCAFile: caFile, StorageRegion: config.DefaultRegion}
	ak, sk, err := credencialDoJoin(ctx, alvo)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ O bucket %s aceitou a chave\n", o.bucket)
	// 3. A configuração do processo irmão, e os outros processos deixam o
	// bucket para ele.
	cfgPath := filepath.Join(dir, "agent-rede-"+o.bucket+".env")
	linhas := linhasDoJoin(main, o, ak, sk, caFile)
	if extras, err := opcoesDoPrincipal(o.config); err == nil {
		linhas = append(extras, linhas...)
	}
	if err := setup.Gravar(cfgPath, linhas); err != nil {
		return err
	}
	for _, irmao := range append([]string{o.config}, irmaosNaPasta(dir, main.AgentID, cfgPath)...) {
		c, err := config.Load(irmao, config.Overrides{})
		if err != nil || slices.Contains(c.SiblingBuckets, o.bucket) {
			continue
		}
		if err := setup.Gravar(irmao, []string{"SIBLING_BUCKETS=" + irmaosDoPrincipal(c, o.bucket)}); err != nil {
			return err
		}
	}
	if conteiner := raizDoHostPadrao(); conteiner != "" && conteiner != "/" && !o.semServico {
		// No Docker, o processo irmão é um contêiner próprio, e os outros
		// reiniciam para deixar o bucket para ele.
		fmt.Fprintf(out, "\nConfiguração gravada em %s.\n\nSuba o contêiner que grava na cópia local e reinicie os do agente:\n\n  sudo docker rm -f %s >/dev/null 2>&1; %s\n  sudo docker restart %s\n",
			cfgPath, o.servico, comandoDoConteinerIrmao(o.servico, cfgPath), strings.Join(append([]string{"arkame-agent"}, servicosDosIrmaos(dir, main.AgentID, cfgPath)...), " "))
		return nil
	}
	if o.semServico {
		fmt.Fprintf(out, "Configuração gravada em %s (sem serviço: rode `run --config` nela).\n", cfgPath)
		return nil
	}
	// 4. O serviço, e o principal reiniciado com o irmão novo.
	cfg, err := config.Load(cfgPath, config.Overrides{})
	if err != nil {
		return err
	}
	inst, err := service.Install(ctx, cfg, service.Options{Name: o.servico, Scope: service.ScopeSystem, Start: true})
	if err != nil {
		return fmt.Errorf("instalando o serviço %s: %w", o.servico, err)
	}
	// O principal e os outros irmãos só leem SIBLING_BUCKETS ao subir.
	reiniciarServicos(ctx, append([]string{o.servicoMain}, servicosDosIrmaos(dir, main.AgentID, cfgPath)...), o.bucket, out)
	fmt.Fprintf(out, "\nPronto. Serviço %s instalado (%s).\n", inst.Name, inst.StatusCmd)
	fmt.Fprintln(out, "Em alguns minutos o painel mostra a cópia local como destino possível dos planos deste servidor.")
	return nil
}

// linhasDoJoin: a identidade do principal (o mesmo servidor no painel), o
// bucket da rede com o certificado fixado, e os buckets do principal como
// irmãos.
func linhasDoJoin(main *config.Config, o opcoesDoJoin, chave, segredo, caFile string) []string {
	irmaos := slices.DeleteFunc(append([]string{main.StorageBucket}, main.SiblingBuckets...), func(b string) bool { return b == "" || b == o.bucket })
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	u, _ := url.Parse(o.endpoint)
	return []string{
		"PANEL_URL=" + main.PanelURL,
		"AGENT_ID=" + main.AgentID,
		"TOKEN_PATH=" + abs(main.TokenPath),
		"PRIVATE_KEY_PATH=" + abs(main.PrivateKeyPath),
		"AGENT_ID_PATH=" + abs(main.AgentIDPath),
		"STORAGE_ID=" + o.storageID,
		"STORAGE_BUCKET=" + o.bucket,
		"STORAGE_ENDPOINT=https://" + u.Host,
		"STORAGE_CA_FILE=" + caFile,
		"STORAGE_REGION=" + config.DefaultRegion,
		"STORAGE_ACCESS_KEY=" + chave,
		"STORAGE_SECRET_KEY=" + segredo,
		"SIBLING_BUCKETS=" + strings.Join(irmaos, ","),
		// Quem troca o programa é o agente principal; este o segue.
		"ARKAME_AUTO_UPDATE=" + atualizacao.ValorSegue,
		// O painel não toma este processo pelo servidor.
		"ARKAME_ROLE=" + api.PapelCopiaDaRede,
		"ARKAME_SERVICE_NAME=" + o.servico,
	}
}

// irmaosNaPasta: os env-files dos outros processos deste agente na pasta da
// configuração (a cópia local, outras cópias da rede), menos `exceto`.
func irmaosNaPasta(dir, agentID, exceto string) []string {
	arquivos, _ := filepath.Glob(filepath.Join(dir, "agent-*.env"))
	var irmaos []string
	for _, a := range arquivos {
		if a == exceto {
			continue
		}
		if c, err := config.Load(a, config.Overrides{}); err == nil && c.AgentID == agentID && c.StorageBucket != "" {
			irmaos = append(irmaos, a)
		}
	}
	return irmaos
}

// arquivoDoIrmao: o env-file da cópia da rede deste bucket, se já existe (um
// join repetido refaz a configuração em vez de recusar).
func arquivoDoIrmao(configPath, bucket string) string {
	p := filepath.Join(filepath.Dir(configPath), "agent-rede-"+bucket+".env")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// servicoDoIrmao: o serviço (ou contêiner) de um env-file de processo irmão:
// o que ele grava (ARKAME_SERVICE_NAME, 0.5.5) ou, sem isso, o nome padrão.
func servicoDoIrmao(arquivo string) string {
	if b, err := os.ReadFile(arquivo); err == nil {
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ARKAME_SERVICE_NAME="); ok && reNomeDeServico.MatchString(v) {
				return v
			}
		}
	}
	base := filepath.Base(arquivo)
	switch {
	case base == "agent-local.env":
		return "arkame-agent-local"
	case strings.HasPrefix(base, "agent-rede-") && strings.HasSuffix(base, ".env"):
		return "arkame-agent-rede-" + strings.TrimSuffix(strings.TrimPrefix(base, "agent-rede-"), ".env")
	}
	return ""
}

// servicosDosIrmaos: os serviços dos outros processos deste agente, menos `exceto`.
func servicosDosIrmaos(dir, agentID, exceto string) []string {
	var nomes []string
	for _, a := range irmaosNaPasta(dir, agentID, exceto) {
		if n := servicoDoIrmao(a); n != "" {
			nomes = append(nomes, n)
		}
	}
	return nomes
}

// reiniciarServicos reinicia cada serviço; a falha vira aviso (o serviço pode
// não estar instalado, como num teste ou numa instalação à mão).
var reiniciarServicos = func(ctx context.Context, nomes []string, bucket string, out io.Writer) {
	for _, nome := range nomes {
		args := service.RestartArgs(nome, service.ScopeSystem)
		if len(args) == 0 {
			continue
		}
		if saida, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(out, "Aviso: reinicie o serviço %s para ele deixar o bucket %s para o processo dele (%v: %s)\n", nome, bucket, err, strings.TrimSpace(string(saida)))
		}
	}
}
