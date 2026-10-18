package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/localstore"
	"github.com/arkame-app/agent/internal/service"
	"github.com/arkame-app/agent/internal/setup"
	"github.com/arkame-app/agent/internal/storage"
)

// newLocalStorageShareCmd: a cópia local recebendo backups de outros
// servidores da mesma rede (Hugo, 18/10: quem troca um Bacula/Bareos tem um
// servidor de discos grandes que guarda o backup da empresa inteira).
//
// O RustFS passa a escutar na rede, com um certificado que cobre os endereços
// dela, e cada servidor cliente ganha uma chave própria que grava e lê neste
// bucket, mas não apaga: quem tomar um desses servidores não leva os backups
// guardados aqui. Só o processo da cópia local, com a credencial do RustFS,
// expurga.
func newLocalStorageShareCmd() *cobra.Command {
	var (
		cfgPath  string
		servico  string
		nomes    []string
		cliente  string
		desligar bool
		revogar  string
		listar   bool
		agentID  string
		prefixo  string
	)
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Deixa outros servidores da rede gravarem backups nesta cópia local",
		Long: `Abre a cópia local desta máquina para outros servidores da mesma rede.

  share --server NOME --agent-id ID
                               cria a chave de um servidor cliente (o id é o
                               do servidor no painel) e mostra o comando
                               "join" para rodar nele
  share --server NOME --ip IP  idem, dizendo por qual endereço ele chega aqui
                               (padrão: os endereços de rede privada desta máquina)
  share --list                 lista as chaves dos servidores clientes
  share --revoke USUARIO       tira o acesso de um servidor cliente
  share --off                  volta a escutar só nesta máquina

A chave de cada cliente grava e lê só a pasta dele no bucket, e não apaga
nada: o expurgo da retenção continua com esta máquina. Libere a porta só para a rede interna no firewall.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return compartilharCopiaLocal(cmd.Context(), opcoesDoShare{cfgPath: cfgPath, servico: servico, nomes: nomes, cliente: cliente, desligar: desligar, revogar: revogar, listar: listar, agentID: agentID, prefixo: prefixo}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "", "env-file do agente principal (padrão: o da instalação)")
	cmd.Flags().StringVar(&servico, "service-name", "arkame-agent-local", "nome do serviço da cópia local")
	cmd.Flags().StringSliceVar(&nomes, "ip", nil, "endereço (IP ou nome) por onde os outros servidores chegam aqui; repita para mais de um")
	cmd.Flags().StringVar(&cliente, "server", "", "nome do servidor cliente (vira parte do nome da chave)")
	cmd.Flags().BoolVar(&desligar, "off", false, "volta a escutar só nesta máquina")
	cmd.Flags().StringVar(&revogar, "revoke", "", "tira o acesso deste usuário (veja --list)")
	cmd.Flags().BoolVar(&listar, "list", false, "lista as chaves dos servidores clientes")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "id do servidor cliente no painel (a chave só vale para a pasta dele no bucket)")
	cmd.Flags().StringVar(&prefixo, "prefix", "arkame/", "prefixo do armazenamento no bucket (o da cópia local é fixo)")
	_ = cmd.Flags().MarkHidden("prefix")
	return cmd
}

type opcoesDoShare struct {
	cfgPath, servico string
	nomes            []string
	cliente          string
	desligar         bool
	revogar          string
	listar           bool
	agentID, prefixo string
}

// Para os testes: reiniciar o serviço e esperar o RustFS responder.
var (
	reiniciarServicoDaCopia = func(ctx context.Context, nome string) error {
		args := service.RestartArgs(nome, service.ScopeSystem)
		if len(args) == 0 {
			return fmt.Errorf("reinicie o serviço %s à mão", nome)
		}
		if saida, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(saida)))
		}
		return nil
	}
	esperarCopiaLocal = func(ctx context.Context, cfg *config.Config) error {
		sup := &localstore.Supervisor{Endereco: cfg.LocalStoreAddress, PastaTLS: cfg.LocalStoreTLS}
		return sup.Pronto(ctx, time.Minute)
	}
)

var reNomeDoCliente = regexp.MustCompile(`[^a-z0-9]+`)

func compartilharCopiaLocal(ctx context.Context, o opcoesDoShare, out io.Writer) error {
	if o.cfgPath == "" {
		o.cfgPath = config.DefaultPath
	}
	localCfgPath := filepath.Join(filepath.Dir(o.cfgPath), "agent-local.env")
	local, err := config.Load(localCfgPath, config.Overrides{})
	if err != nil {
		return fmt.Errorf("lendo a configuração da cópia local (%s): %w", localCfgPath, err)
	}
	if local.LocalStoreProgram == "" || local.LocalStoreAddress == "" || local.LocalStoreTLS == "" || local.StorageBucket == "" {
		return fmt.Errorf("esta máquina não tem cópia local instalada (%s): instale-a antes, pela aba Cópia local do servidor no painel", localCfgPath)
	}
	_, porta, err := net.SplitHostPort(local.LocalStoreAddress)
	if err != nil {
		return fmt.Errorf("endereço da cópia local inválido em %s: %s", localCfgPath, local.LocalStoreAddress)
	}
	admin := func() (*localstore.Admin, error) {
		cert := filepath.Join(local.LocalStoreTLS, localstore.ArquivoDoCertificado)
		tc, err := storage.ConfigTLSFixado(cert)
		if err != nil {
			return nil, err
		}
		ak, err1 := os.ReadFile(local.LocalStoreAccessFile)
		sk, err2 := os.ReadFile(local.LocalStoreSecretFile)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("lendo a credencial da cópia local: rode como administrador")
		}
		return &localstore.Admin{
			Endereco: "https://" + local.LocalStoreAddress,
			Chave:    strings.TrimSpace(string(ak)),
			Segredo:  strings.TrimSpace(string(sk)),
			Cliente:  &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}},
		}, nil
	}

	switch {
	case o.desligar:
		if err := setup.Gravar(localCfgPath, []string{"ARKAME_LOCAL_STORE_LISTEN="}); err != nil {
			return err
		}
		if conteiner := raizDoHostPadrao(); conteiner != "" && conteiner != "/" {
			fmt.Fprintf(out, "Configuração gravada. Reinicie o contêiner da cópia local: sudo docker restart %s\n", o.servico)
			return nil
		}
		if err := reiniciarServicoDaCopia(ctx, o.servico); err != nil {
			return fmt.Errorf("reiniciando o serviço %s: %w", o.servico, err)
		}
		fmt.Fprintln(out, "Pronto: a cópia local voltou a escutar só nesta máquina. As chaves dos servidores clientes continuam no RustFS; use --revoke para tirá-las.")
		return nil
	case o.listar || o.revogar != "":
		a, err := admin()
		if err != nil {
			return err
		}
		if o.revogar != "" {
			if !strings.HasPrefix(o.revogar, "arkame-") {
				return fmt.Errorf("%s não é uma chave de servidor cliente (elas começam com arkame-)", o.revogar)
			}
			if err := a.RemoverUsuario(ctx, o.revogar); err != nil {
				return err
			}
			fmt.Fprintf(out, "Pronto: %s não grava mais nesta cópia local.\n", o.revogar)
			return nil
		}
		usuarios, err := a.Usuarios(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, u := range usuarios {
			if strings.HasPrefix(u, "arkame-") {
				fmt.Fprintln(out, u)
				n++
			}
		}
		if n == 0 {
			fmt.Fprintln(out, "Nenhum servidor cliente ainda (crie com --server NOME).")
		}
		return nil
	}

	if !reUUID.MatchString(o.agentID) {
		return fmt.Errorf("diga o id do servidor cliente no painel: --agent-id (o comando do painel já traz)")
	}
	if o.prefixo == "" {
		o.prefixo = "arkame/"
	}
	slug := strings.Trim(reNomeDoCliente.ReplaceAllString(strings.ToLower(o.cliente), "-"), "-")
	if slug == "" {
		return fmt.Errorf("diga o nome do servidor cliente: --server NOME")
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	nomes := o.nomes
	if len(nomes) == 0 {
		nomes = enderecosDaRede()
		if len(nomes) == 0 {
			return fmt.Errorf("não achei um endereço de rede privada nesta máquina: diga por onde os outros servidores chegam aqui com --ip")
		}
	}
	for _, n := range nomes {
		if strings.ContainsAny(n, " /:") || n == "" {
			return fmt.Errorf("--ip inválido: %q (só o IP ou o nome, sem porta)", n)
		}
	}
	// 1. O certificado cobrindo os endereços da rede, e o RustFS escutando nela.
	certAntes, _ := localstore.ImpressaoDoCertificado(filepath.Join(local.LocalStoreTLS, localstore.ArquivoDoCertificado))
	cert, err := localstore.PrepararTLS(local.LocalStoreTLS, nomes...)
	if err != nil {
		return fmt.Errorf("certificado da cópia local: %w", err)
	}
	impressao, err := localstore.ImpressaoDoCertificado(cert)
	if err != nil {
		return err
	}
	escuta := "0.0.0.0:" + porta
	if local.LocalStoreListen != escuta || impressao != certAntes {
		if err := setup.Gravar(localCfgPath, []string{"ARKAME_LOCAL_STORE_LISTEN=" + escuta}); err != nil {
			return err
		}
		if conteiner := raizDoHostPadrao(); conteiner != "" && conteiner != "/" {
			// No Docker, quem reinicia o contêiner da cópia local é a pessoa;
			// a chave do cliente sai na segunda vez, com ele escutando na rede.
			fmt.Fprintf(out, "Configuração gravada. Reinicie o contêiner da cópia local e rode este mesmo comando de novo:\n\n  sudo docker restart %s\n", o.servico)
			return nil
		}
		if err := reiniciarServicoDaCopia(ctx, o.servico); err != nil {
			return fmt.Errorf("reiniciando o serviço %s: %w", o.servico, err)
		}
		if err := esperarCopiaLocal(ctx, local); err != nil {
			return err
		}
		fmt.Fprintf(out, "  ✓ Cópia local escutando na rede (porta %s)\n", porta)
		if impressao != certAntes && certAntes != "" {
			fmt.Fprintln(out, "  ! O certificado mudou: servidores clientes que já existiam precisam rodar o \"join\" de novo.")
		}
	}
	// 2. A chave do cliente: grava e lê, não apaga.
	a, err := admin()
	if err != nil {
		return err
	}
	sufixo := make([]byte, 2)
	if _, err := rand.Read(sufixo); err != nil {
		return err
	}
	usuario := "arkame-" + slug + "-" + hex.EncodeToString(sufixo)
	segredo := aleatorio(30)
	if err := a.DarAcessoDaRede(ctx, local.StorageBucket, o.prefixo, o.agentID, usuario, segredo); err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ Chave do servidor %s criada (grava e lê só a pasta dele; não apaga)\n\n", o.cliente)
	partes := fmt.Sprintf("local-storage join --storage-id %s --bucket %s --endpoint https://%s --fingerprint %s", local.StorageID, local.StorageBucket, net.JoinHostPort(nomes[0], porta), impressao)
	fmt.Fprintf(out, "No servidor %s, rode como administrador o comando do sistema dele:\n\n", o.cliente)
	fmt.Fprintf(out, "  Linux ou macOS:\n    sudo arkame-agent %s\n\n", partes)
	fmt.Fprintf(out, "  Windows (PowerShell de administrador):\n    & 'C:\\Program Files\\Arkame\\arkame-agent.exe' %s\n\n", partes)
	fmt.Fprintf(out, "  Docker:\n    sudo docker run --rm -it --user 0 --security-opt label=disable --network host -v /:/host -v /etc/arkame:/etc/arkame %s %s\n\n", imagemDoAgente, partes)
	fmt.Fprintf(out, "Quando ele pedir a credencial, informe (aparece só agora; guarde num cofre se precisar):\n\n  Chave de acesso: %s\n  Segredo:         %s\n\n", usuario, segredo)
	fmt.Fprintf(out, "Libere a porta %s no firewall desta máquina só para a rede interna.\n", porta)
	return nil
}

// enderecosDaRede: os IPv4 de rede privada das interfaces ligadas.
func enderecosDaRede() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || interfaceVirtual(i.Name) {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				ips = append(ips, n.IP.String())
			}
		}
	}
	return ips
}

// interfaceVirtual: as pontes e pares virtuais (Docker, Podman, libvirt, LXC,
// Kubernetes), cujos endereços não são por onde outro servidor chega aqui.
func interfaceVirtual(nome string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vnet", "lxc", "lxd", "podman", "cni", "flannel", "cali", "kube", "tun", "tap", "wg", "zt"} {
		if strings.HasPrefix(nome, p) {
			return true
		}
	}
	return false
}
