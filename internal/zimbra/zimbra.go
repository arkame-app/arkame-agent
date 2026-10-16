// Package zimbra exporta e restaura as contas de um servidor Zimbra, para o
// backup do agente levar.
//
// O que vai por conta (pedido do fundador, 16/10): as mensagens, a
// configuração (os atributos da própria conta: preferências, nome, cota,
// recursos, apelidos, a classe de serviço), os filtros (Sieve, de entrada e de
// saída), as assinaturas, e os contatos, a agenda, as tarefas e os documentos.
// A senha não vai: a API do Zimbra não a entrega (VALUE-BLOCKED), e um backup
// com o hash da senha de todo mundo seria mais um lugar de onde ela vaza. O
// painel não vê nada disso: o índice tem só os nomes dos arquivos (a conta, o
// mês) e o tamanho, como em qualquer outro backup — nem assunto, nem
// remetente.
//
// As mensagens saem pela API REST do próprio Zimbra (`?fmt=tgz`), **um
// arquivo por mês**. Um .tgz da caixa inteira mudaria todo dia, e o backup
// reenviaria todo o e-mail do servidor a cada sessão; o mês fechado não muda e
// não é exportado de novo. O plano escolhe "todas as mensagens" (os últimos 24
// meses mês a mês, e um arquivo com tudo o que é mais antigo) ou "dos últimos
// N anos" (os meses de fora saem da pasta, e a retenção do armazenamento cuida
// do que já subiu).
//
// Roda como root (o agente), fala com o Zimbra pela porta de administração em
// localhost (cliente.go) e grava ele mesmo os arquivos. A pasta da exportação
// tem a caixa de todo mundo: fica fechada para o administrador, como a de
// qualquer dump.
package zimbra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// reConta: o endereço de uma conta. Entra em caminho de arquivo e em URL:
// nada de barra, espaço, aspas nem `..`.
var reConta = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}@[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ContaValida diz se o endereço pode virar nome de pasta.
func ContaValida(c string) bool { return reConta.MatchString(c) && !strings.Contains(c, "..") }

// contasDoSistema: as que o Zimbra cria para ele mesmo e não marca como tal
// em toda versão (antispam, quarentena, sincronia da lista global).
var contasDoSistema = regexp.MustCompile(`^(spam\.|ham\.|virus-quarantine\.|galsync)`)

// ContaListada é uma conta do servidor. Motivo vazio: entra no backup.
type ContaListada struct {
	Nome   string
	Motivo string
}

// porPagina: quantas contas cada pedido ao diretório traz.
var porPagina = 500

// ListarContas devolve as contas de pessoas do servidor; as que ficam de fora
// vêm com o motivo, para aparecer no resumo em vez de sumir.
func (c *Cliente) ListarContas(ctx context.Context) ([]ContaListada, error) {
	var r []ContaListada
	for offset := 0; ; offset += porPagina {
		var resp struct {
			Account []contaDoDiretorio `json:"account"`
			More    bool               `json:"more"`
		}
		err := c.admin(ctx, "SearchDirectoryRequest", map[string]any{
			// maxResults 0: sem o teto do servidor (que daria
			// TOO_MANY_SEARCH_RESULTS num diretório grande antes de paginar).
			"types": "accounts", "query": "", "limit": porPagina, "offset": offset, "sortBy": "name", "maxResults": 0,
			"attrs": "zimbraIsSystemAccount,zimbraIsSystemResource,zimbraMailHost",
		}, &resp)
		if err != nil {
			return nil, fmt.Errorf("listando as contas do Zimbra: %w", err)
		}
		for _, a := range resp.Account {
			nome := strings.ToLower(a.Name)
			at := a.atributos()
			switch {
			case at.um("zimbraIsSystemAccount") == "TRUE" || at.um("zimbraIsSystemResource") == "TRUE" || contasDoSistema.MatchString(nome):
				continue
			case !ContaValida(nome):
				r = append(r, ContaListada{nome, "endereço com caracteres que não viram nome de pasta"})
			case c.servidor != "" && at.um("zimbraMailHost") != "" && !strings.EqualFold(at.um("zimbraMailHost"), c.servidor):
				// A caixa está noutro servidor do Zimbra: o agente de lá a exporta.
				r = append(r, ContaListada{nome, "a caixa está em " + at.um("zimbraMailHost")})
			default:
				r = append(r, ContaListada{Nome: nome})
			}
		}
		if !resp.More || len(resp.Account) == 0 {
			return r, nil
		}
	}
}

// contaDoDiretorio é uma conta como o SOAP em JSON a devolve.
type contaDoDiretorio struct {
	Name string     `json:"name"`
	ID   string     `json:"id"`
	A    []atributo `json:"a"`
}

type atributo struct {
	N string `json:"n"`
	V string `json:"_content"`
}

// atributos: atributo → valores (o mesmo nome se repete nos de vários valores).
type atributos map[string][]string

func (a contaDoDiretorio) atributos() atributos {
	r := atributos{}
	for _, x := range a.A {
		r[x.N] = append(r[x.N], x.V)
	}
	return r
}

func (a atributos) um(nome string) string {
	if v := a[nome]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ErrSemZimbra: não há Zimbra onde devia.
var ErrSemZimbra = errors.New("não achei o Zimbra")

// Instalado diz se a pasta tem um Zimbra (a configuração e o certificado que
// o cliente usa).
func Instalado(pasta string) error {
	if pasta == "" {
		pasta = PastaPadrao
	}
	for _, p := range []string{filepath.Join("conf", "localconfig.xml"), filepath.Join("ssl", "zimbra", "server", "server.crt")} {
		if _, err := os.Stat(filepath.Join(pasta, p)); err != nil {
			return fmt.Errorf("%w em %s (falta %s): este servidor tem o Zimbra instalado em outro lugar? use --zimbra-dir", ErrSemZimbra, pasta, p)
		}
	}
	return nil
}
