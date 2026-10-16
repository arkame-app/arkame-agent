package zimbra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/arkame-app/agent/internal/segredo"
)

// OpcoesDeRestauracao da restauração de uma conta.
type OpcoesDeRestauracao struct {
	// De: a pasta da conta, como o backup a restaurou (…/contas/<conta>).
	De string
	// Para: a conta que recebe (padrão: a mesma do backup). Outra conta só
	// recebe as mensagens — a configuração é da original.
	Para string
	// Resolver: o que fazer com o item que já está na caixa — skip (padrão:
	// deixa o que está), modify, replace ou reset (apaga a caixa antes).
	Resolver string
	// SemMensagens: só a configuração, os filtros e as assinaturas.
	SemMensagens bool
	// Configuracao: numa conta que ainda existe, reaplica também a
	// configuração (sem ela, a conta que existe só recebe as mensagens: a
	// pessoa pode ter mudado as preferências depois do backup).
	Configuracao bool
	// Encaminhamentos: reaplica também o que manda e-mail para fora (o
	// encaminhamento, a resposta automática, o aviso de mensagem nova). Sem
	// pedido explícito, não: restaurar uma conta não deve voltar a mandar o
	// e-mail dela para um endereço que alguém tirou dali de propósito.
	Encaminhamentos bool
}

var resolucoes = map[string]bool{"skip": true, "modify": true, "replace": true, "reset": true}

// atributosDeContato: os do LDAP que voltam, além dos zimbraPref* e
// zimbraFeature*.
var atributosDeContato = map[string]bool{
	"displayName": true, "givenName": true, "sn": true, "cn": true, "initials": true, "description": true,
	"telephoneNumber": true, "mobile": true, "title": true, "company": true, "street": true, "l": true,
	"st": true, "postalCode": true, "co": true,
	"zimbraMailQuota": true, "zimbraAccountStatus": true,
	"zimbraMailSieveScript": true, "zimbraMailOutgoingSieveScript": true,
}

// encaminhamentos: o que manda a correspondência (ou um aviso dela) para
// fora da caixa, ou libera a conta a fazê-lo.
func encaminhamento(nome string) bool {
	switch nome {
	case "zimbraPrefMailForwardingAddress", "zimbraMailForwardingAddress", "zimbraPrefMailLocalDeliveryDisabled",
		"zimbraPrefCalendarForwardInvitesTo", "zimbraPrefNewMailNotificationAddress", "zimbraPrefNewMailNotificationEnabled",
		"zimbraPrefCalendarReminderEmail", "zimbraPrefCalendarReminderDeviceEmail":
		return true
	}
	return strings.HasPrefix(nome, "zimbraPrefOutOfOffice") || (strings.HasPrefix(nome, "zimbraFeature") && strings.Contains(nome, "Forwarding"))
}

// filtroQueEncaminha: um filtro com `redirect` (ou `notify`) manda e-mail
// para fora como um encaminhamento. Reaplicado pelo administrador, ele ainda
// pula a conferência que o Zimbra faz quando a própria pessoa salva o filtro.
var filtroQueEncaminha = regexp.MustCompile(`(?i)\b(redirect|notify|reply|vacation)\b`)

func sieve(nome string) bool {
	return nome == "zimbraMailSieveScript" || nome == "zimbraMailOutgoingSieveScript"
}

// restauravel: o que volta para a conta (o que encaminha, atributosParaOZimbra
// decide antes). Os identificadores, o servidor da caixa e as datas são do
// Zimbra; as assinaturas voltam por CreateSignature (com id novo), e as
// preferências que apontam para elas são refeitas depois.
func restauravel(nome string) bool {
	if atributosSecretos.MatchString(nome) || strings.HasPrefix(nome, "zimbraSignature") || strings.HasPrefix(nome, "zimbraPrefMailSignature") {
		return false
	}
	switch nome {
	case "zimbraPrefDefaultSignatureId", "zimbraPrefForwardReplySignatureId", "zimbraPrefIdentityId":
		return false
	}
	return atributosDeContato[nome] || strings.HasPrefix(nome, "zimbraPref") || strings.HasPrefix(nome, "zimbraFeature")
}

// atributosParaOZimbra: o formato de CreateAccount e ModifyAccount, em ordem
// (para o log e os testes), e o que ficou de fora por mandar e-mail para fora.
func atributosParaOZimbra(at atributos, comEncaminhamentos bool) (r []atributo, nomes, fora []string) {
	for n, vs := range at {
		switch {
		case encaminhamento(n) && !comEncaminhamentos:
			fora = append(fora, n)
		case sieve(n) && !comEncaminhamentos && slices.ContainsFunc(vs, filtroQueEncaminha.MatchString):
			fora = append(fora, n+" (o filtro encaminha)")
		case restauravel(n):
			nomes = append(nomes, n)
		}
	}
	sort.Strings(nomes)
	sort.Strings(fora)
	for _, n := range nomes {
		for _, v := range at[n] {
			r = append(r, atributo{N: n, V: v})
		}
	}
	return r, nomes, fora
}

// Restaurar devolve uma conta a partir da pasta exportada.
func Restaurar(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, log io.Writer) error {
	if o.Resolver == "" {
		o.Resolver = "skip"
	}
	if !resolucoes[o.Resolver] {
		return fmt.Errorf("--resolve tem de ser skip, modify, replace ou reset")
	}
	origem := filepath.Base(filepath.Clean(o.De))
	if !ContaValida(origem) {
		return fmt.Errorf("--from tem de ser a pasta da conta (…/contas/<conta>): %s", o.De)
	}
	if o.Para == "" {
		o.Para = origem
	}
	o.Para = strings.ToLower(o.Para)
	if !ContaValida(o.Para) {
		return fmt.Errorf("conta de destino inválida: %s", o.Para)
	}
	real, err := conferirPastaDaRestauracao(o.De)
	if err != nil {
		return err
	}
	o.De = real
	b, err := lerDaPasta(o.De, "conta.json")
	if err != nil {
		return fmt.Errorf("a pasta %s não tem a exportação da conta (conta.json): %w", o.De, err)
	}
	var cfg configuracaoDaConta
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("lendo %s: %w", filepath.Join(o.De, "conta.json"), err)
	}
	mesma := o.Para == origem
	if !mesma && (o.Configuracao || o.Encaminhamentos || o.SemMensagens) {
		return fmt.Errorf("--to restaura só as mensagens: --settings, --with-forwarding e --settings-only valem só para a própria conta")
	}

	id, err := idDaConta(ctx, c, o.Para)
	if err != nil {
		return err
	}
	switch {
	case id == "" && !mesma:
		return fmt.Errorf("a conta %s não existe: crie-a antes de restaurar nela as mensagens de %s", o.Para, origem)
	case id == "":
		if id, err = recriar(ctx, c, o, cfg, log); err != nil {
			return err
		}
	case mesma && (o.Configuracao || o.SemMensagens):
		if err := reaplicar(ctx, c, o, cfg, id, log); err != nil {
			return err
		}
	case mesma:
		fmt.Fprintf(log, "a conta %s existe: só as mensagens voltam (--settings reaplica também a configuração, os filtros e as assinaturas)\n", o.Para)
	}
	if o.SemMensagens {
		return nil
	}
	return restaurarMensagens(ctx, c, o, log)
}

// idDaConta: o zimbraId da conta, ou "" se ela não existe.
func idDaConta(ctx context.Context, c *Cliente, conta string) (string, error) {
	var ga struct {
		Account []contaDoDiretorio `json:"account"`
	}
	err := c.admin(ctx, "GetAccountRequest", map[string]any{"attrs": "zimbraId", "account": map[string]any{"by": "name", "_content": conta}}, &ga)
	var ez *ErroDoZimbra
	if errors.As(err, &ez) && ez.Codigo == "account.NO_SUCH_ACCOUNT" {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("procurando a conta %s: %w", conta, err)
	}
	if len(ga.Account) != 1 || ga.Account[0].ID == "" {
		return "", fmt.Errorf("procurando a conta %s: o Zimbra não devolveu o id", conta)
	}
	return ga.Account[0].ID, nil
}

// idDaClasse: o id da classe de serviço pelo nome, ou "" se não há.
func idDaClasse(ctx context.Context, c *Cliente, nome string) string {
	if nome == "" {
		return ""
	}
	var cos struct {
		Cos []struct {
			ID string `json:"id"`
		} `json:"cos"`
	}
	if err := c.admin(ctx, "GetCosRequest", map[string]any{"cos": map[string]any{"by": "name", "_content": nome}}, &cos); err != nil || len(cos.Cos) != 1 {
		return ""
	}
	return cos.Cos[0].ID
}

// recriar cria a conta que sumiu, com a configuração de antes e uma senha
// aleatória: a de antes não foi para o backup.
func recriar(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, cfg configuracaoDaConta, log io.Writer) (string, error) {
	as, nomes, fora := atributosParaOZimbra(cfg.Atributos, o.Encaminhamentos)
	if cos := idDaClasse(ctx, c, cfg.Classe); cos != "" {
		as = append(as, atributo{N: "zimbraCOSId", V: cos})
	} else if cfg.Classe != "" {
		fmt.Fprintf(log, "a classe de serviço %q não existe neste servidor: a conta fica na padrão\n", cfg.Classe)
	}
	senha := make([]byte, 24)
	if _, err := rand.Read(senha); err != nil {
		return "", err
	}
	var resp struct {
		Account []contaDoDiretorio `json:"account"`
	}
	if err := c.admin(ctx, "CreateAccountRequest", map[string]any{"name": o.Para, "password": hex.EncodeToString(senha), "a": as}, &resp); err != nil {
		return "", fmt.Errorf("recriando a conta %s: %w", o.Para, err)
	}
	if len(resp.Account) != 1 || resp.Account[0].ID == "" {
		return "", fmt.Errorf("recriando a conta %s: o Zimbra não devolveu o id", o.Para)
	}
	id := resp.Account[0].ID
	fmt.Fprintf(log, "conta %s recriada, com uma senha aleatória: defina a senha dela no console de administração do Zimbra\n", o.Para)
	fmt.Fprintf(log, "configuração aplicada: %s\n", listaOuNada(nomes))
	avisarEncaminhamentos(fora, log)
	apelidos(ctx, c, id, cfg, log)
	assinaturasOuAviso(ctx, c, o, cfg, id, log)
	return id, nil
}

// assinaturasOuAviso: uma assinatura que não volta (o teto da classe de
// serviço de destino, por exemplo) não impede as mensagens de voltarem.
func assinaturasOuAviso(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, cfg configuracaoDaConta, id string, log io.Writer) {
	if err := restaurarAssinaturas(ctx, c, o, cfg, id, log); err != nil {
		fmt.Fprintf(log, "AVISO: %v (as mensagens seguem)\n", err)
	}
}

// reaplicar devolve a configuração a uma conta que existe (nunca a senha).
func reaplicar(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, cfg configuracaoDaConta, id string, log io.Writer) error {
	as, nomes, fora := atributosParaOZimbra(cfg.Atributos, o.Encaminhamentos)
	if cos := idDaClasse(ctx, c, cfg.Classe); cos != "" {
		as = append(as, atributo{N: "zimbraCOSId", V: cos})
	}
	if len(as) > 0 {
		if err := c.admin(ctx, "ModifyAccountRequest", map[string]any{"id": id, "a": as}, nil); err != nil {
			return fmt.Errorf("restaurando a configuração de %s: %w", o.Para, err)
		}
	}
	fmt.Fprintf(log, "configuração reaplicada em %s: %s\n", o.Para, listaOuNada(nomes))
	avisarEncaminhamentos(fora, log)
	apelidos(ctx, c, id, cfg, log)
	assinaturasOuAviso(ctx, c, o, cfg, id, log)
	return nil
}

func listaOuNada(nomes []string) string {
	if len(nomes) == 0 {
		return "nada"
	}
	return strings.Join(nomes, ", ")
}

// avisarEncaminhamentos: o que ficou de fora sem --with-forwarding.
func avisarEncaminhamentos(fora []string, log io.Writer) {
	if len(fora) > 0 {
		fmt.Fprintf(log, "não reaplicados (mandam e-mail para fora; use --with-forwarding): %s\n", strings.Join(fora, ", "))
	}
}

// apelidos devolve os endereços alternativos. Um que outra conta já usa fica
// de fora, com aviso: não é motivo para parar a restauração.
func apelidos(ctx context.Context, c *Cliente, id string, cfg configuracaoDaConta, log io.Writer) {
	for _, a := range cfg.Atributos["zimbraMailAlias"] {
		err := c.admin(ctx, "AddAccountAliasRequest", map[string]any{"id": id, "alias": a}, nil)
		var ez *ErroDoZimbra
		switch {
		case err == nil:
			fmt.Fprintf(log, "apelido %s devolvido\n", a)
		case errors.As(err, &ez) && ez.Codigo == "account.ACCOUNT_EXISTS":
			// Já é desta conta (ou de outra: o Zimbra não distingue aqui).
		default:
			fmt.Fprintf(log, "apelido %s não devolvido: %v\n", a, err)
		}
	}
}

type assinatura struct {
	Name    string `json:"name"`
	ID      string `json:"id,omitempty"`
	Content []struct {
		Type string `json:"type"`
		V    string `json:"_content"`
	} `json:"content,omitempty"`
}

// restaurarAssinaturas recria as que faltam (pelo nome) e aponta as padrões
// para os ids novos.
func restaurarAssinaturas(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, cfg configuracaoDaConta, id string, log io.Writer) error {
	b, err := lerDaPasta(o.De, "assinaturas.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil // exportação sem assinaturas
	}
	if err != nil {
		return err
	}
	var salvas []assinatura
	if err := json.Unmarshal(b, &salvas); err != nil {
		return fmt.Errorf("lendo assinaturas.json: %w", err)
	}
	var atuais struct {
		Signature []assinatura `json:"signature"`
	}
	if err := c.daConta(ctx, o.Para, "GetSignaturesRequest", nil, &atuais); err != nil {
		return fmt.Errorf("lendo as assinaturas de %s: %w", o.Para, err)
	}
	idPorNome := map[string]string{}
	for _, s := range atuais.Signature {
		idPorNome[s.Name] = s.ID
	}
	novoID := map[string]string{} // id antigo → id atual
	criadas := 0
	for _, s := range salvas {
		if s.Name == "" {
			continue
		}
		if atual, ok := idPorNome[s.Name]; ok {
			novoID[s.ID] = atual
			continue
		}
		var resp struct {
			Signature []assinatura `json:"signature"`
		}
		nova := map[string]any{"name": s.Name}
		if len(s.Content) > 0 {
			nova["content"] = s.Content
		}
		if err := c.daConta(ctx, o.Para, "CreateSignatureRequest", map[string]any{"signature": nova}, &resp); err != nil {
			return fmt.Errorf("recriando a assinatura %q de %s: %w", s.Name, o.Para, err)
		}
		if len(resp.Signature) == 1 {
			novoID[s.ID] = resp.Signature[0].ID
		}
		criadas++
	}
	var padrao []atributo
	for _, n := range []string{"zimbraPrefDefaultSignatureId", "zimbraPrefForwardReplySignatureId"} {
		if v := novoID[cfg.Atributos.um(n)]; v != "" {
			padrao = append(padrao, atributo{N: n, V: v})
		}
	}
	if len(padrao) > 0 {
		if err := c.admin(ctx, "ModifyAccountRequest", map[string]any{"id": id, "a": padrao}, nil); err != nil {
			return fmt.Errorf("apontando a assinatura padrão de %s: %w", o.Para, err)
		}
	}
	fmt.Fprintf(log, "assinaturas de %s: %d recriada(s)\n", o.Para, criadas)
	return nil
}

// arquivosDeMensagens: o que a pasta tem para importar, na ordem — o mais
// antigo primeiro, a estrutura (contatos, agenda) por último.
func arquivosDeMensagens(dir string) ([]string, error) {
	entradas, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var meses []string
	for _, e := range entradas {
		if m := reArquivoDoMes.FindStringSubmatch(e.Name()); m != nil && m[2] == "tgz" {
			meses = append(meses, e.Name())
		}
	}
	sort.Strings(meses)
	var r []string
	if existe(dir, "anteriores.tgz") {
		r = append(r, "anteriores.tgz")
	}
	r = append(r, meses...)
	if existe(dir, "estrutura.tgz") {
		r = append(r, "estrutura.tgz")
	}
	return r, nil
}

// restaurarMensagens importa os .tgz da pasta, em fluxo, direto do arquivo.
func restaurarMensagens(ctx context.Context, c *Cliente, o OpcoesDeRestauracao, log io.Writer) error {
	arquivos, err := arquivosDeMensagens(o.De)
	if err != nil {
		return err
	}
	if len(arquivos) == 0 {
		fmt.Fprintf(log, "nenhuma mensagem para restaurar em %s\n", o.De)
		return nil
	}
	for i, nome := range arquivos {
		// reset apaga a caixa: só na primeira importação, senão cada mês
		// apagaria o anterior.
		r := o.Resolver
		if r == "reset" && i > 0 {
			r = "skip"
		}
		if err := importar(ctx, c, o.Para, o.De, nome, r); err != nil {
			return fmt.Errorf("importando %s em %s: %w", nome, o.Para, err)
		}
		fmt.Fprintf(log, "%s importado em %s\n", strings.TrimSuffix(nome, ".tgz"), o.Para)
	}
	return nil
}

func importar(ctx context.Context, c *Cliente, conta, dir, nome, resolver string) error {
	f, err := abrirDaPasta(dir, nome)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.enviar(ctx, conta, url.Values{"fmt": {"tgz"}, "resolve": {resolver}}, f)
}

// conferirPastaDaRestauracao: a pasta restaurada e todas as de cima, até a
// raiz, são de dono confiável, são pastas de verdade e ninguém mais escreve
// nelas. O root lê dali a configuração que vai aplicar e as mensagens que vai
// pôr numa caixa — e a conta de destino sai do nome da pasta: quem plantasse
// arquivos ali (ou trocasse uma pasta do caminho pela sua) escolheria o que o
// administrador do Zimbra grava em nome dele.
//
// Um link no caminho (o /home → var/home do Fedora atômico) passa se a pasta
// em que ele está é fechada: só quem escreve nela troca o link. O caminho
// resolvido é conferido de novo, inteiro.
//
// Devolve o caminho resolvido: é dele que a restauração lê, para nenhum link
// do caminho mudar entre a conferência e a leitura.
func conferirPastaDaRestauracao(dir string) (string, error) {
	dir = filepath.Clean(dir)
	if err := conferirCaminho(dir, true); err != nil {
		return "", fmt.Errorf("pasta da restauração: %w", err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("pasta da restauração: %w", err)
	}
	if real != dir {
		if err := conferirCaminho(real, false); err != nil {
			return "", fmt.Errorf("pasta da restauração: %w", err)
		}
	}
	return real, nil
}

func conferirCaminho(dir string, aceitaLink bool) error {
	for p := dir; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if !aceitaLink || p == dir {
				return fmt.Errorf("%s é um link", p)
			}
			// Numa pasta sticky aberta a todos, o link pode ser de qualquer
			// um — e quem o criou o refaz apontando para onde quiser.
			if pai, err := os.Lstat(filepath.Dir(p)); err != nil || (pai.Mode()&os.ModeSticky != 0 && pai.Mode().Perm()&0o022 != 0) {
				return fmt.Errorf("%s é um link numa pasta aberta a outros", p)
			}
		case !fi.IsDir():
			return fmt.Errorf("%s não é uma pasta", p)
		default:
			// Uma pasta de cima com o bit sticky (/tmp) pode ser de todos:
			// nela, só o dono renomeia ou apaga a própria entrada.
			if runtime.GOOS != "windows" && fi.Mode().Perm()&0o022 != 0 && (p == dir || fi.Mode()&os.ModeSticky == 0) {
				return fmt.Errorf("outros usuários escrevem em %s (restaure numa pasta só do administrador, como /var/backups/arkame/zimbra-restaurar)", p)
			}
			if err := segredo.ConferirDono(p); err != nil {
				return err
			}
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// abrirDaPasta abre um arquivo regular da pasta, sem seguir link: o que foi
// aberto tem de ser o mesmo que o Lstat viu.
func abrirDaPasta(dir, nome string) (*os.File, error) {
	caminho := filepath.Join(dir, nome)
	fi, err := os.Lstat(caminho)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s não é um arquivo comum", caminho)
	}
	f, err := os.Open(caminho)
	if err != nil {
		return nil, err
	}
	if aberto, err := f.Stat(); err != nil || !os.SameFile(fi, aberto) {
		f.Close()
		return nil, fmt.Errorf("%s mudou enquanto era aberto", caminho)
	}
	return f, nil
}

func lerDaPasta(dir, nome string) ([]byte, error) {
	f, err := abrirDaPasta(dir, nome)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 16<<20))
}
