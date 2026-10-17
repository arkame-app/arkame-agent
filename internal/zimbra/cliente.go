package zimbra

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cliente fala com o Zimbra pela API dele (SOAP em JSON e REST), na porta de
// administração em localhost, como o `zmprov` e o `zmmailbox -z` fazem — mas
// sem eles. Antes, o agente chamava as ferramentas como o usuário `zimbra`:
// cada chamada subia uma JVM (um servidor de mil contas não cabia no prazo do
// comando), o `zimbra` herdava o ambiente e o terminal do root, os arquivos
// passavam por uma pasta dele (o root trocava o dono de um caminho que ele
// podia trocar por um link), e a saída do `zmprov ga` era texto ambíguo — uma
// resposta automática com uma linha "zimbraMailForwardingAddress: …" virava
// atributo na restauração (revisão do S18). Aqui é uma conexão, JSON, e o
// arquivo vai e volta em fluxo, gravado pelo próprio agente.
//
// A credencial é a que o Zimbra guarda para as próprias ferramentas
// (zimbra_ldap_user/zimbra_ldap_password, no localconfig.xml, legível só pelo
// root e pelo zimbra). A conexão confia só no certificado do próprio servidor
// (o arquivo em ssl/zimbra/server): a porta 7071 aceita qualquer processo que
// escute nela quando o Zimbra está parado, e a senha de administrador não pode
// ir para ele.
type Cliente struct {
	base string // https://127.0.0.1:7071
	http *http.Client
	// envio: o da importação, sem prazo para o cabeçalho da resposta — o
	// Zimbra só responde quando termina de importar, e um mês de vários GB
	// leva mais que os 10 minutos do outro. O prazo é o do ctx.
	envio *http.Client
	token string
	// servidor: o nome deste servidor no Zimbra (zimbra_server_hostname). A
	// conta cuja caixa está noutro servidor fica para o agente de lá.
	servidor string
}

// Pasta é onde o Zimbra está (padrão /opt/zimbra).
const PastaPadrao = "/opt/zimbra"

// localconfig lê chaves do localconfig.xml.
func localconfig(pasta string, chaves ...string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(pasta, "conf", "localconfig.xml"))
	if err != nil {
		return nil, fmt.Errorf("lendo a configuração do Zimbra: %w", err)
	}
	var lc struct {
		Keys []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value"`
		} `xml:"key"`
	}
	if err := xml.Unmarshal(b, &lc); err != nil {
		return nil, fmt.Errorf("lendo %s: %w", filepath.Join(pasta, "conf", "localconfig.xml"), err)
	}
	// O arquivo só guarda o que difere do padrão do Zimbra.
	r := map[string]string{"zimbra_ldap_user": "zimbra", "zimbra_admin_service_port": "7071"}
	for _, k := range lc.Keys {
		if k.Value != "" {
			r[k.Name] = k.Value
		}
	}
	for _, c := range chaves {
		if r[c] == "" {
			return nil, fmt.Errorf("a configuração do Zimbra não tem %s", c)
		}
	}
	return r, nil
}

// certificadoFixado: a conexão só aceita um certificado que esteja num dos
// arquivos: o autoassinado da instalação (obrigatório) e o comercial que o
// `zmcertmgr deploycrt comm` instala, quando há. Um Zimbra com Let's Encrypt
// apresenta o comercial na porta de administração, e só o autoassinado fazia a
// exportação falhar logo na entrada (Zimbra Adubos, 17/10). Os dois arquivos
// são do Zimbra, na mesma pasta protegida.
func certificadoFixado(arquivo string, opcionais ...string) (*tls.Config, error) {
	b, err := os.ReadFile(arquivo)
	if err != nil {
		return nil, fmt.Errorf("lendo o certificado do Zimbra: %w", err)
	}
	aceitos := certificadosDoPEM(b)
	if len(aceitos) == 0 {
		return nil, fmt.Errorf("%s não tem certificado", arquivo)
	}
	for _, o := range opcionais {
		if b, err := os.ReadFile(o); err == nil {
			aceitos = append(aceitos, certificadosDoPEM(b)...)
		}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// O nome no certificado é o do servidor, e a conexão é em 127.0.0.1:
		// em vez da cadeia e do nome, o certificado apresentado tem de ser
		// exatamente o de um dos arquivos.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(brutos [][]byte, _ [][]*x509.Certificate) error {
			if len(brutos) == 0 {
				return errors.New("o servidor não apresentou certificado")
			}
			for _, a := range aceitos {
				if bytes.Equal(a, brutos[0]) {
					return nil
				}
			}
			return errors.New("o certificado na porta de administração não é o do Zimbra deste servidor (nem o autoassinado, nem o comercial instalado)")
		},
	}, nil
}

func certificadosDoPEM(b []byte) [][]byte {
	var aceitos [][]byte
	for {
		var bloco *pem.Block
		bloco, b = pem.Decode(b)
		if bloco == nil {
			return aceitos
		}
		if bloco.Type == "CERTIFICATE" {
			aceitos = append(aceitos, bloco.Bytes)
		}
	}
}

// Conectar autentica como o administrador das ferramentas do Zimbra.
func Conectar(ctx context.Context, pasta string) (*Cliente, error) {
	if pasta == "" {
		pasta = PastaPadrao
	}
	lc, err := localconfig(pasta, "zimbra_ldap_user", "zimbra_ldap_password")
	if err != nil {
		return nil, err
	}
	porta := lc["zimbra_admin_service_port"]
	tc, err := certificadoFixado(filepath.Join(pasta, "ssl", "zimbra", "server", "server.crt"),
		filepath.Join(pasta, "ssl", "zimbra", "commercial", "commercial.crt"))
	if err != nil {
		return nil, err
	}
	c := &Cliente{
		base:     "https://127.0.0.1:" + porta,
		servidor: lc["zimbra_server_hostname"],
		// Sem teto total: uma caixa de vários GB leva tempo. O prazo é o do ctx.
		http: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tc, ResponseHeaderTimeout: 10 * time.Minute},
			// Sem seguir redirecionamento: o Go reenviaria o cookie com o token
			// (e, num 307, a senha do AuthRequest) para outra porta do mesmo
			// host — que pode ser de qualquer usuário local, e em texto claro.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	envio := *c.http
	envio.Transport = &http.Transport{TLSClientConfig: tc}
	c.envio = &envio
	var resp struct {
		AuthToken []struct {
			Content string `json:"_content"`
		} `json:"authToken"`
	}
	if err := c.soap(ctx, "/service/admin/soap", "", "AuthRequest", "urn:zimbraAdmin",
		map[string]any{"name": lc["zimbra_ldap_user"], "password": lc["zimbra_ldap_password"]}, &resp); err != nil {
		return nil, fmt.Errorf("entrando no Zimbra como %s: %w", lc["zimbra_ldap_user"], err)
	}
	if len(resp.AuthToken) == 0 || resp.AuthToken[0].Content == "" {
		return nil, errors.New("o Zimbra não devolveu o token de administração")
	}
	c.token = resp.AuthToken[0].Content
	return c, nil
}

// ErroDoZimbra é a falha que o Zimbra devolve (Fault), com o código.
type ErroDoZimbra struct{ Codigo, Motivo string }

func (e *ErroDoZimbra) Error() string { return e.Codigo + ": " + e.Motivo }

// soap faz um pedido SOAP em JSON. `conta` (opcional) é a conta em nome de
// quem o administrador age (pedidos de conta, como as assinaturas: a porta de
// administração os aceita no contexto da conta; a de usuário, com o token de
// administrador, responde 500).
func (c *Cliente) soap(ctx context.Context, caminho, conta, pedido, ns string, corpo map[string]any, saida any) error {
	if corpo == nil {
		corpo = map[string]any{}
	}
	corpo["_jsns"] = ns
	contexto := map[string]any{"_jsns": "urn:zimbra", "format": map[string]any{"type": "js"}}
	if c.token != "" {
		contexto["authToken"] = c.token
	}
	if conta != "" {
		contexto["account"] = map[string]any{"by": "name", "_content": conta}
	}
	b, err := json.Marshal(map[string]any{"Header": map[string]any{"context": contexto}, "Body": map[string]any{pedido: corpo}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+caminho, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	bruto, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	var env struct {
		Body map[string]json.RawMessage `json:"Body"`
	}
	if err := json.Unmarshal(bruto, &env); err != nil {
		return fmt.Errorf("%s: resposta que não é JSON (HTTP %d)", pedido, res.StatusCode)
	}
	if f, ok := env.Body["Fault"]; ok {
		var fault struct {
			Reason struct {
				Text string `json:"Text"`
			} `json:"Reason"`
			Detail struct {
				Error struct {
					Code string `json:"Code"`
				} `json:"Error"`
			} `json:"Detail"`
		}
		_ = json.Unmarshal(f, &fault)
		return &ErroDoZimbra{Codigo: fault.Detail.Error.Code, Motivo: fault.Reason.Text}
	}
	nome := strings.TrimSuffix(pedido, "Request") + "Response"
	r, ok := env.Body[nome]
	if !ok {
		return fmt.Errorf("%s: o Zimbra não devolveu %s", pedido, nome)
	}
	if saida == nil {
		return nil
	}
	return json.Unmarshal(r, saida)
}

func (c *Cliente) admin(ctx context.Context, pedido string, corpo map[string]any, saida any) error {
	return c.soap(ctx, "/service/admin/soap", "", pedido, "urn:zimbraAdmin", corpo, saida)
}

func (c *Cliente) daConta(ctx context.Context, conta, pedido string, corpo map[string]any, saida any) error {
	return c.soap(ctx, "/service/admin/soap", conta, pedido, "urn:zimbraAccount", corpo, saida)
}

// urlREST é o endereço REST da caixa de uma conta.
func (c *Cliente) urlREST(conta string, params url.Values) string {
	return c.base + "/home/" + url.PathEscape(conta) + "/?" + params.Encode()
}

// ErrSemConteudo: a exportação não achou nada (o Zimbra responde 204).
var ErrSemConteudo = errors.New("nenhum item")

// baixar grava em w o .tgz da consulta. 204 é ErrSemConteudo.
func (c *Cliente) baixar(ctx context.Context, conta string, params url.Values, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.urlREST(conta, params), nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "ZM_ADMIN_AUTH_TOKEN", Value: c.token})
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNoContent:
		return ErrSemConteudo
	case res.StatusCode != http.StatusOK:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 600))
		return fmt.Errorf("exportando de %s: HTTP %d: %s", conta, res.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(w, res.Body)
	return err
}

// enviar importa o .tgz na caixa da conta.
func (c *Cliente) enviar(ctx context.Context, conta string, params url.Values, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.urlREST(conta, params), r)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "ZM_ADMIN_AUTH_TOKEN", Value: c.token})
	req.Header.Set("Content-Type", "application/x-compressed-tar")
	res, err := c.envio.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 600))
		return fmt.Errorf("importando em %s: HTTP %d: %s", conta, res.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}
