//go:build !windows

package zimbra

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// contaFalsa é uma conta do Zimbra de mentira.
type contaFalsa struct {
	id          string
	at          atributos
	sistema     bool
	host        string
	mensagens   []time.Time // a data de cada mensagem
	assinaturas []assinatura
	quebrada    bool // o REST dela responde 500
	mesQuebrado string
	estrutura   string // muda o conteúdo dos contatos
	semAssinar  bool   // CreateSignature falha
}

// zimbraFalso responde como a porta 7071 de um Zimbra: SOAP em JSON e REST.
type zimbraFalso struct {
	mu      sync.Mutex
	contas  map[string]*contaFalsa
	classes map[string]string // nome → id
	pedidos []string          // "Pedido conta" de cada chamada
	corpos  map[string][]map[string]any
	rest    []string // "GET conta query" / "POST conta resolve"
	senha   string
	pasta   string // a pasta do Zimbra (localconfig e certificado)
	srv     *httptest.Server
	atraso  time.Duration // quanto cada exportação REST demora (o teste do prazo)
	lentoSe string        // só as consultas com este trecho demoram (vazio: todas)
	// redirecionar: o SOAP responde 307 para /desvio (o teste do redirecionamento).
	redirecionar bool
	desviados    int
	// semAssinarAoCriar: a conta recriada não aceita assinatura.
	semAssinarAoCriar bool
}

func novoZimbra(t *testing.T) *zimbraFalso {
	t.Helper()
	z := &zimbraFalso{contas: map[string]*contaFalsa{}, classes: map[string]string{"executivos": "cos-exec"}, corpos: map[string][]map[string]any{}, senha: "senha-do-ldap"}
	z.srv = httptest.NewTLSServer(http.HandlerFunc(z.servir))
	t.Cleanup(z.srv.Close)
	z.pasta = t.TempDir()
	porta := z.srv.URL[strings.LastIndex(z.srv.URL, ":")+1:]
	os.MkdirAll(filepath.Join(z.pasta, "conf"), 0o755)
	os.MkdirAll(filepath.Join(z.pasta, "ssl", "zimbra", "server"), 0o755)
	os.WriteFile(filepath.Join(z.pasta, "conf", "localconfig.xml"), []byte(`<?xml version="1.0"?><localconfig>
<key name="zimbra_ldap_password"><value>`+z.senha+`</value></key>
<key name="zimbra_admin_service_port"><value>`+porta+`</value></key>
<key name="zimbra_server_hostname"><value>mail.exemplo.com.br</value></key>
</localconfig>`), 0o600)
	os.WriteFile(filepath.Join(z.pasta, "ssl", "zimbra", "server", "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: z.srv.Certificate().Raw}), 0o644)
	return z
}

func (z *zimbraFalso) conta(nome string, c *contaFalsa) {
	if c.id == "" {
		c.id = "id-" + nome
	}
	if c.at == nil {
		c.at = atributos{}
	}
	z.contas[nome] = c
}

func (z *zimbraFalso) cliente(t *testing.T) *Cliente {
	t.Helper()
	c, err := Conectar(context.Background(), z.pasta)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (z *zimbraFalso) contar(prefixo string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := 0
	for _, p := range append(slices.Clone(z.pedidos), z.rest...) {
		if strings.HasPrefix(p, prefixo) {
			n++
		}
	}
	return n
}

func (z *zimbraFalso) limpar() {
	z.mu.Lock()
	z.pedidos, z.rest, z.corpos = nil, nil, map[string][]map[string]any{}
	z.mu.Unlock()
}

func fault(w http.ResponseWriter, codigo, motivo string) {
	json.NewEncoder(w).Encode(map[string]any{"Body": map[string]any{"Fault": map[string]any{"Reason": map[string]any{"Text": motivo}, "Detail": map[string]any{"Error": map[string]any{"Code": codigo}}}}})
}

func responder(w http.ResponseWriter, pedido string, corpo any) {
	json.NewEncoder(w).Encode(map[string]any{"Body": map[string]any{strings.TrimSuffix(pedido, "Request") + "Response": corpo}})
}

func listaDeAtributos(at atributos) []map[string]string {
	var r []map[string]string
	for n, vs := range at {
		for _, v := range vs {
			r = append(r, map[string]string{"n": n, "_content": v})
		}
	}
	return r
}

func atributosDoCorpo(corpo map[string]any) atributos {
	r := atributos{}
	l, _ := corpo["a"].([]any)
	for _, x := range l {
		m := x.(map[string]any)
		r[m["n"].(string)] = append(r[m["n"].(string)], m["_content"].(string))
	}
	return r
}

func (z *zimbraFalso) servir(w http.ResponseWriter, r *http.Request) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/home/") {
		z.servirREST(w, r)
		return
	}
	if r.URL.Path == "/desvio" {
		z.desviados++
		return
	}
	if z.redirecionar {
		http.Redirect(w, r, "/desvio", http.StatusTemporaryRedirect)
		return
	}
	var env struct {
		Header struct {
			Context struct {
				AuthToken string `json:"authToken"`
				Account   struct {
					V string `json:"_content"`
				} `json:"account"`
			} `json:"context"`
		} `json:"Header"`
		Body map[string]map[string]any `json:"Body"`
	}
	json.NewDecoder(r.Body).Decode(&env)
	var pedido string
	var corpo map[string]any
	for pedido, corpo = range env.Body {
	}
	contexto := env.Header.Context.Account.V
	z.pedidos = append(z.pedidos, strings.TrimSpace(pedido+" "+contexto))
	z.corpos[pedido] = append(z.corpos[pedido], corpo)
	if pedido != "AuthRequest" && env.Header.Context.AuthToken != "token-adm" {
		fault(w, "service.AUTH_REQUIRED", "sem token")
		return
	}
	porNome := func() string { m, _ := corpo["account"].(map[string]any); s, _ := m["_content"].(string); return s }
	switch pedido {
	case "AuthRequest":
		if corpo["name"] != "zimbra" || corpo["password"] != z.senha {
			fault(w, "account.AUTH_FAILED", "senha errada")
			return
		}
		responder(w, pedido, map[string]any{"authToken": []map[string]string{{"_content": "token-adm"}}})
	case "SearchDirectoryRequest":
		var nomes []string
		for n := range z.contas {
			nomes = append(nomes, n)
		}
		slices.Sort(nomes)
		off, lim := int(corpo["offset"].(float64)), int(corpo["limit"].(float64))
		var l []map[string]any
		for i := off; i < len(nomes) && i < off+lim; i++ {
			c := z.contas[nomes[i]]
			at := atributos{}
			if c.sistema {
				at["zimbraIsSystemAccount"] = []string{"TRUE"}
			}
			if c.host != "" {
				at["zimbraMailHost"] = []string{c.host}
			}
			l = append(l, map[string]any{"name": nomes[i], "id": c.id, "a": listaDeAtributos(at)})
		}
		responder(w, pedido, map[string]any{"account": l, "more": off+lim < len(nomes)})
	case "GetAccountRequest":
		c, ok := z.contas[porNome()]
		if !ok {
			fault(w, "account.NO_SUCH_ACCOUNT", "no such account: "+porNome())
			return
		}
		at := atributos{}
		for n, v := range c.at {
			at[n] = v
		}
		at["userPassword"] = []string{"VALUE-BLOCKED"}
		at["zimbraAuthTokens"] = []string{"tok1"}
		at["zimbraId"] = []string{c.id}
		responder(w, pedido, map[string]any{"account": []map[string]any{{"name": porNome(), "id": c.id, "a": listaDeAtributos(at)}}})
	case "GetCosRequest":
		m := corpo["cos"].(map[string]any)
		for nome, id := range z.classes {
			if (m["by"] == "name" && m["_content"] == nome) || (m["by"] == "id" && m["_content"] == id) {
				responder(w, pedido, map[string]any{"cos": []map[string]string{{"name": nome, "id": id}}})
				return
			}
		}
		fault(w, "account.NO_SUCH_COS", "no such cos")
	case "CreateAccountRequest":
		nome := corpo["name"].(string)
		z.conta(nome, &contaFalsa{at: atributosDoCorpo(corpo), semAssinar: z.semAssinarAoCriar})
		responder(w, pedido, map[string]any{"account": []map[string]any{{"name": nome, "id": "id-" + nome}}})
	case "ModifyAccountRequest", "AddAccountAliasRequest":
		responder(w, pedido, map[string]any{})
	case "GetSignaturesRequest":
		responder(w, pedido, map[string]any{"signature": z.contas[contexto].assinaturas})
	case "CreateSignatureRequest":
		if c := z.contas[contexto]; c != nil && c.semAssinar {
			fault(w, "account.TOO_MANY_ENTRIES", "too many signatures")
			return
		}
		responder(w, pedido, map[string]any{"signature": []map[string]string{{"id": "sig-nova"}}})
	default:
		fault(w, "service.UNKNOWN_DOCUMENT", pedido)
	}
}

// servirREST: GET exporta as mensagens que a consulta acha (uma linha por
// data), POST importa.
func (z *zimbraFalso) servirREST(w http.ResponseWriter, r *http.Request) {
	conta, _ := url.PathUnescape(strings.Trim(strings.TrimPrefix(r.URL.Path, "/home/"), "/"))
	if ck, err := r.Cookie("ZM_ADMIN_AUTH_TOKEN"); err != nil || ck.Value != "token-adm" {
		http.Error(w, "sem token", 401)
		return
	}
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		z.rest = append(z.rest, "POST "+conta+" "+q.Get("resolve")+" "+strings.Join(conteudosDoTgz(b), "\n"))
		return
	}
	z.rest = append(z.rest, "GET "+conta+" "+q.Get("types")+" "+q.Get("query"))
	if atraso := z.atraso; atraso > 0 && (z.lentoSe == "" || strings.Contains(q.Get("types")+" "+q.Get("query"), z.lentoSe) || strings.Contains(conta, z.lentoSe)) {
		z.mu.Unlock()
		select {
		case <-time.After(atraso):
		case <-r.Context().Done():
		}
		z.mu.Lock()
	}
	c := z.contas[conta]
	if c.quebrada || (c.mesQuebrado != "" && strings.Contains(q.Get("query"), c.mesQuebrado)) {
		http.Error(w, "service.FAILURE", 500)
		return
	}
	if q.Get("types") != "message" {
		w.Write(tgz(map[string]string{"Contacts/Cliente.meta": "ESTRUTURA" + c.estrutura}))
		return
	}
	var depois, antes int64 = -1 << 62, 1 << 62
	for _, p := range strings.Fields(q.Get("query")) {
		if v, ok := strings.CutPrefix(p, "after:"); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			depois = n / 1000
		}
		if v, ok := strings.CutPrefix(p, "before:"); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			antes = n / 1000
		}
	}
	achadas := map[string]string{}
	for _, m := range c.mensagens {
		if s := m.Unix(); s > depois && s < antes { // estrita nas duas pontas, ao segundo
			d := m.UTC().Format(time.RFC3339)
			achadas["Inbox/"+d+".eml"] = d
		}
	}
	if len(achadas) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Write(tgz(achadas))
}

// tgz monta um .tgz como o do Zimbra, em ordem de nome, com a hora no
// cabeçalho do gzip (que muda a cada exportação, como no de verdade).
func tgz(entradas map[string]string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	gz.ModTime = time.Now()
	tw := tar.NewWriter(gz)
	nomes := slices.Sorted(maps.Keys(entradas))
	for _, n := range nomes {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(entradas[n])), ModTime: time.Now()})
		tw.Write([]byte(entradas[n]))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

// conteudosDoTgz: o conteúdo de cada entrada, em ordem.
func conteudosDoTgz(b []byte) []string {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return []string{"(não é tgz)"}
	}
	tr := tar.NewReader(gz)
	var r []string
	for {
		if _, err := tr.Next(); err != nil {
			return r
		}
		c, _ := io.ReadAll(tr)
		r = append(r, string(c))
	}
}

var agora = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func data(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ler: o arquivo; de um .tgz, o conteúdo das entradas, uma por linha.
func ler(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(p, ".tgz") {
		return strings.Join(conteudosDoTgz(b), "\n")
	}
	return string(b)
}

func TestConsultaDoMesEJanela(t *testing.T) {
	jan := inicioDoMes(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
	// 2026-01-01T00:00:00Z menos um segundo, e 2026-02-01T00:00:00Z.
	if got := consultaDoMes(jan); got != "is:anywhere after:1767225599000 before:1769904000000" {
		t.Fatalf("janeiro: %s", got)
	}
	ms := mesesDaJanela(agora, 1)
	if len(ms) != 13 || ms[0].nome() != "2025-10" || ms[12].nome() != "2026-10" {
		t.Fatalf("janela de 1 ano: %d meses, %s a %s (tem de cobrir desde 16/10/2025)", len(ms), ms[0].nome(), ms[len(ms)-1].nome())
	}
	if ms := mesesDaJanela(agora, 0); len(ms) != MesesDeTodas {
		t.Fatalf("todas: %d meses", len(ms))
	}
}

func TestContaValida(t *testing.T) {
	for _, c := range []string{"ana@exemplo.com.br", "a.b+c@x.io"} {
		if !ContaValida(c) {
			t.Errorf("%s recusada", c)
		}
	}
	for _, c := range []string{"../x@y.com", "a b@c.com", "a/b@c.com", "ana@exemplo", "a@b..com", `a"@b.com`} {
		if ContaValida(c) {
			t.Errorf("%s aceita (vira caminho)", c)
		}
	}
}

func TestConectarSoComOCertificadoDoZimbra(t *testing.T) {
	z := novoZimbra(t)
	// O certificado do arquivo é outro: quem atende na porta não é o Zimbra
	// deste servidor, e a senha não pode ir para ele.
	chave, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	modelo := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.exemplo.com.br"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, modelo, modelo, &chave.PublicKey, chave)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(z.pasta, "ssl", "zimbra", "server", "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	if _, err := Conectar(context.Background(), z.pasta); err == nil || !strings.Contains(err.Error(), "não é o do Zimbra") {
		t.Fatalf("aceitou um certificado que não é o do Zimbra: %v", err)
	}
	if z.contar("AuthRequest") != 0 {
		t.Fatal("a senha foi enviada antes de conferir o certificado")
	}
}

func TestListarContasPaginaEDeixaDeFora(t *testing.T) {
	z := novoZimbra(t)
	defer func(n int) { porPagina = n }(porPagina)
	porPagina = 2
	z.conta("ana@exemplo.com.br", &contaFalsa{})
	z.conta("bruno@exemplo.com.br", &contaFalsa{})
	z.conta("carla@exemplo.com.br", &contaFalsa{host: "mail.exemplo.com.br"})
	z.conta("galsync.x@exemplo.com.br", &contaFalsa{})
	z.conta("spam.abc@exemplo.com.br", &contaFalsa{})
	z.conta("robo@exemplo.com.br", &contaFalsa{sistema: true})
	z.conta("filial@exemplo.com.br", &contaFalsa{host: "mail2.exemplo.com.br"})
	z.conta("a b@exemplo.com.br", &contaFalsa{})
	cs, err := z.cliente(t).ListarContas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var nomes []string
	fora := map[string]string{}
	for _, c := range cs {
		if c.Motivo != "" {
			fora[c.Nome] = c.Motivo
			continue
		}
		nomes = append(nomes, c.Nome)
	}
	if strings.Join(nomes, ",") != "ana@exemplo.com.br,bruno@exemplo.com.br,carla@exemplo.com.br" {
		t.Fatalf("contas: %v (a busca vem em páginas de 2)", nomes)
	}
	if !strings.Contains(fora["filial@exemplo.com.br"], "mail2") || fora["a b@exemplo.com.br"] == "" || len(fora) != 2 {
		t.Fatalf("fora: %v", fora)
	}
}

// servidorDeExemplo: ana com mensagens em vários meses, a Lixeira inclusive;
// quebra, cuja caixa falha.
func servidorDeExemplo(t *testing.T) *zimbraFalso {
	z := novoZimbra(t)
	z.conta("ana@exemplo.com.br", &contaFalsa{
		at: atributos{
			"displayName": {"Ana Souza"}, "zimbraCOSId": {"cos-exec"}, "zimbraMailAlias": {"aninha@exemplo.com.br"},
			"zimbraMailSieveScript":           {"require [\"fileinto\"];\nif header :contains \"subject\" \"fatura\" { fileinto \"Financeiro\"; }"},
			"zimbraPrefOutOfOfficeReply":      {"Estou fora.\nzimbraMailForwardingAddress: atacante@mal.test"},
			"zimbraPrefMailForwardingAddress": {"ana@pessoal.com"},
			"zimbraPrefDefaultSignatureId":    {"sig-antiga"},
			"zimbraTwoFactorAuthSecret":       {"segredo-2fa"},
			"zimbraZimletUserProperties":      {"com_zimbra_x:senha:segredo-zimlet"},
		},
		mensagens: []time.Time{
			data("2019-03-15T10:00:00Z"),                               // anteriores
			data("2024-03-10T10:00:00Z"),                               // anteriores no modo "todas"; mês próprio com 3 anos
			data("2024-12-31T23:59:59Z"), data("2025-01-01T00:00:00Z"), // a virada exata
			data("2026-09-30T23:59:59Z"), data("2026-10-01T00:00:00Z"),
			data("2026-10-10T10:00:00Z"),
		},
		assinaturas: []assinatura{{Name: "Principal", ID: "sig-antiga", Content: []struct {
			Type string `json:"type"`
			V    string `json:"_content"`
		}{{"text/plain", "Ana — Financeiro"}}}},
	})
	z.conta("quebra@exemplo.com.br", &contaFalsa{quebrada: true})
	return z
}

func TestExportarPorContaEPorMes(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	var log bytes.Buffer
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Falhas != 1 || len(inv.Contas) != 2 || !strings.Contains(log.String(), "FALHOU quebra@exemplo.com.br") {
		t.Fatalf("esperado 2 contas e a falha de quebra@: %+v\n%s", inv, log.String())
	}
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	// Cada mensagem num só arquivo: a virada do mês não duplica nem perde.
	for arq, quer := range map[string]string{
		"caixa-2026-10.tgz": "2026-10-01T00:00:00Z\n2026-10-10T10:00:00Z",
		"caixa-2026-09.tgz": "2026-09-30T23:59:59Z",
		"anteriores.tgz":    "2019-03-15T10:00:00Z\n2024-03-10T10:00:00Z", // a janela de 24 meses começa em 2024-11
	} {
		if got := ler(t, filepath.Join(dir, arq)); got != quer {
			t.Errorf("%s = %q, quer %q", arq, got, quer)
		}
	}
	if got := ler(t, filepath.Join(dir, "caixa-2024-12.tgz")); got != "2024-12-31T23:59:59Z" {
		t.Errorf("caixa-2024-12 = %q", got)
	}
	if got := ler(t, filepath.Join(dir, "caixa-2025-01.tgz")); got != "2025-01-01T00:00:00Z" {
		t.Errorf("caixa-2025-01 = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "caixa-2026-08.vazio")); err != nil {
		t.Error("mês sem mensagem não virou .vazio")
	}
	if ler(t, filepath.Join(dir, "estrutura.tgz")) != "ESTRUTURA" {
		t.Error("faltou a estrutura (contatos, agenda)")
	}
	if !strings.Contains(ler(t, filepath.Join(dir, "filtros.sieve")), `fileinto "Financeiro"`) {
		t.Error("filtro não exportado")
	}
	conta := ler(t, filepath.Join(dir, "conta.json"))
	for _, nao := range []string{"VALUE-BLOCKED", "userPassword", "zimbraAuthTokens", "segredo-2fa", "segredo-zimlet"} {
		if strings.Contains(conta, nao) {
			t.Errorf("conta.json leva %s", nao)
		}
	}
	if !strings.Contains(conta, `"classe_de_servico": "executivos"`) {
		t.Errorf("a classe de serviço não foi pelo nome:\n%s", conta)
	}
	if !strings.Contains(ler(t, filepath.Join(dir, "assinaturas.json")), "Ana — Financeiro") {
		t.Error("assinatura não exportada")
	}
	if _, err := os.Stat(filepath.Join(pasta, "contas", "quebra@exemplo.com.br", MarcadorDeFalha)); err != nil {
		t.Error("a conta que falhou ficou sem o marcador")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "conta.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("conta.json com modo %v", fi.Mode().Perm())
	}
	if !strings.Contains(z.rest[len(z.rest)-1], "is:anywhere") {
		t.Error("a consulta não inclui Lixeira e Spam")
	}

	// Segunda sessão, um mês depois: só o mês que corria e o novo saem de
	// novo; os fechados, não; o anteriores, também não.
	z.limpar()
	z.contas["quebra@exemplo.com.br"].quebrada = false
	depois := agora.AddDate(0, 1, 0)
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: depois}, &log); err != nil {
		t.Fatal(err)
	}
	var meses []string
	for _, p := range z.rest {
		if strings.HasPrefix(p, "GET ana@exemplo.com.br message") {
			meses = append(meses, p)
		}
	}
	// 2026-10 (exportado enquanto corria), 2026-11 (o corrente) e um mês
	// fechado conferido no rodízio (o mais antigo da pasta, 2024-11, que já
	// saiu da janela mas fica no modo "todas") — que, sem mudança, não é
	// regravado (não sobe de novo).
	if len(meses) != 3 || !strings.Contains(meses[2], "before:"+ms(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))) {
		t.Fatalf("segunda sessão: quer o mês que corria, o novo e a conferência de 2024-11:\n%s", strings.Join(meses, "\n"))
	}
	if fi, _ := os.Stat(filepath.Join(dir, "caixa-2024-11.vazio")); !fi.ModTime().Equal(agora) {
		t.Error("o mês conferido sem mudança foi regravado")
	}
	if _, err := os.Stat(filepath.Join(pasta, "contas", "quebra@exemplo.com.br", MarcadorDeFalha)); err == nil {
		t.Error("o marcador de falha ficou depois de a conta exportar")
	}
}

func TestMesExportadoEnquantoCorriaEhRefeito(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	o := Opcoes{Pasta: pasta, Contas: []string{"ana@exemplo.com.br"}, Agora: agora}
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	// O agente fica parado quatro meses: outubro foi exportado no dia 16 e
	// nunca mais — não pode ficar pela metade para sempre.
	z.limpar()
	o.Agora = agora.AddDate(0, 4, 0)
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if z.contar("GET ana@exemplo.com.br message is:anywhere after:"+ms(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(-time.Second))) != 1 {
		t.Fatalf("outubro (exportado no meio do mês) não foi refeito:\n%s", strings.Join(z.rest, "\n"))
	}
}

func TestMesQueFalhaNaoParaOsOutros(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["ana@exemplo.com.br"].mesQuebrado = "before:" + ms(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) // agosto
	pasta := filepath.Join(t.TempDir(), "zimbra")
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Contas: []string{"ana@exemplo.com.br"}, Agora: agora}, io.Discard)
	// A conta sai com a falha (e o marcador), mas o comando não: o que saiu
	// tem de subir — uma falha não pode segurar o backup do servidor.
	if err != nil || inv.Falhas != 1 || !strings.Contains(inv.Contas[0].Erro, "mês 2026-08") {
		t.Fatalf("a falha de agosto não apareceu, ou derrubou o comando: %v %+v", err, inv)
	}
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	for _, arq := range []string{"caixa-2026-07.vazio", "caixa-2025-01.tgz", "anteriores.tgz", MarcadorDeFalha} {
		if _, err := os.Stat(filepath.Join(dir, arq)); err != nil {
			t.Errorf("faltou %s: a falha de agosto parou o resto", arq)
		}
	}
}

func TestAnosRemoveOsDeForaETrocaDeModoNaoDuplica(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	o := Opcoes{Pasta: pasta, Contas: []string{"ana@exemplo.com.br"}, Agora: agora}
	// Começa com "todas": o anteriores sai. Passa a 1 ano: ele e os meses de
	// fora saem da pasta.
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	o.Anos = 3
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "anteriores.tgz")); err == nil {
		t.Fatal("com --years o anteriores ficou na pasta")
	}
	if _, err := os.Stat(filepath.Join(dir, "caixa-2023-10.vazio")); err != nil {
		t.Fatal("3 anos não cobrem desde 2023-10")
	}
	// Volta para "todas": o anteriores novo começa antes do mês mais antigo
	// que a pasta tem (2023-10), não antes dos 24 meses — senão os meses de
	// 2023-10 a 2024-10 sairiam duas vezes.
	z.limpar()
	o.Anos = 0
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := ler(t, filepath.Join(dir, "anteriores.tgz")); got != "2019-03-15T10:00:00Z" {
		t.Fatalf("anteriores = %q (quer só o de antes de 2023-10)", got)
	}
}

func TestPrazoDeixaORestoParaAProximaSessao(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	pasta := filepath.Join(t.TempDir(), "zimbra")
	// Prazo dentro da folga já no começo: o plano está mal configurado, e o
	// comando diz isso (senão ficaria pendente para sempre, em silêncio).
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Prazo: time.Now().Add(folgaDoPrazo / 2)}, io.Discard); err == nil || !strings.Contains(err.Error(), "curto demais") {
		t.Fatalf("prazo curto demais não foi erro: %v", err)
	}
	// O prazo vence no meio da conta (cada mês leva 100ms): ela fica
	// pendente, sem marcador de falha, e o comando sai sem erro.
	z.mu.Lock()
	z.atraso = 100 * time.Millisecond
	z.mu.Unlock()
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}, Prazo: time.Now().Add(folgaDoPrazo + 1500*time.Millisecond)}, io.Discard)
	z.mu.Lock()
	z.atraso = 0
	z.mu.Unlock()
	if err != nil || inv.Pendentes != 1 || inv.Falhas != 0 || inv.Contas[0].Erro != "" {
		t.Fatalf("prazo vencido no meio da conta virou falha: %v %+v", err, inv)
	}
	if _, err := os.Stat(filepath.Join(pasta, "contas", "ana@exemplo.com.br", MarcadorDeFalha)); err == nil {
		t.Fatal("a conta interrompida pelo prazo ficou marcada como falha")
	}
	if !inv.Contas[0].UltimoSucesso.IsZero() {
		t.Fatal("a conta interrompida ganhou data de sucesso")
	}
	if !existe(filepath.Join(pasta, "contas", "ana@exemplo.com.br"), "estrutura.tgz") {
		t.Fatal("os contatos ficaram presos atrás das mensagens que não couberam no prazo")
	}
}

// primeiraConta: a conta do primeiro GetAccount da sessão.
func (z *zimbraFalso) primeiraConta() string {
	z.mu.Lock()
	defer z.mu.Unlock()
	if len(z.corpos["GetAccountRequest"]) == 0 {
		return ""
	}
	return fmt.Sprint(z.corpos["GetAccountRequest"][0]["account"].(map[string]any)["_content"])
}

func TestFilaPelaUltimaTentativa(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	z.conta("zeca@exemplo.com.br", &contaFalsa{})
	pasta := filepath.Join(t.TempDir(), "zimbra")
	// ana e quebra já foram tentadas; zeca, nunca: vem antes, mesmo sendo a
	// última em ordem alfabética.
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Excluir: []string{"zeca@exemplo.com.br"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	z.limpar()
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Hour)}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := z.primeiraConta(); got != "zeca@exemplo.com.br" {
		t.Fatalf("a primeira conta foi %s (quer a que nunca foi tentada)", got)
	}
	// A fila anda pela última tentativa, não pelo último sucesso: ana nunca
	// terminou (prazo curto), mas foi tentada agora há pouco; quebra terminou
	// há tempos e foi tentada antes. Pelo sucesso, ana seguraria a fila.
	inv := Inventario{Contas: []ResultadoDaConta{
		{Conta: "ana@exemplo.com.br", UltimaTentativa: agora.Add(5 * time.Hour)},
		{Conta: "quebra@exemplo.com.br", UltimoSucesso: agora, UltimaTentativa: agora.Add(time.Hour)},
		{Conta: "zeca@exemplo.com.br", UltimoSucesso: agora, UltimaTentativa: agora.Add(6 * time.Hour)},
	}}
	if err := gravarJSON(filepath.Join(pasta, "inventario.json"), inv); err != nil {
		t.Fatal(err)
	}
	z.limpar()
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(7 * time.Hour)}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := z.primeiraConta(); got != "quebra@exemplo.com.br" {
		t.Fatalf("a primeira conta foi %s (quer quebra, tentada há mais tempo)", got)
	}
}

func TestMarcadorSaiNoSucessoMesmoComOPrazoVencido(t *testing.T) {
	dir := t.TempDir()
	marcador := filepath.Join(dir, MarcadorDeFalha)
	os.WriteFile(marcador, []byte("velho"), 0o600)
	if fecharMarcador(dir, nil, true) != "" || !existe(dir, MarcadorDeFalha) {
		t.Fatal("interrompida sem erro: o marcador é de Exportar decidir, e sumiu")
	}
	if fecharMarcador(dir, nil, false) != "" || existe(dir, MarcadorDeFalha) {
		t.Fatal("completa, sem erro (mesmo com o prazo vencido depois do último arquivo): o marcador velho ficou")
	}
	if fecharMarcador(dir, []string{"mês 2026-08: falhou"}, true) == "" || ler(t, marcador) != "mês 2026-08: falhou\n" {
		t.Fatal("o erro de verdade não deixou o marcador porque o prazo venceu depois")
	}
}

func TestContaQueNaoAvancaViraFalha(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Daqui em diante cada pedido demora mais que o prazo: nenhum arquivo
	// novo sai. Duas sessões pendentes; na terceira, falha com o motivo.
	z.mu.Lock()
	z.atraso = time.Hour
	z.mu.Unlock()
	defer func() { z.mu.Lock(); z.atraso = 0; z.mu.Unlock() }()
	for i := 1; i <= maxSemAvanco; i++ {
		o.Agora = agora.Add(time.Duration(i) * time.Hour)
		o.Prazo = time.Now().Add(folgaDoPrazo + 100*time.Millisecond)
		inv, err := Exportar(context.Background(), z.cliente(t), o, io.Discard)
		if err != nil && i < maxSemAvanco {
			t.Fatalf("sessão %d: %v", i, err)
		}
		r := inv.Contas[0]
		if i < maxSemAvanco && (!r.Pendente || r.SemAvanco != i) {
			t.Fatalf("sessão %d: quer pendente com %d sem avanço: %+v", i, i, r)
		}
		if i == maxSemAvanco {
			if r.Pendente || !strings.Contains(r.Erro, "aumente o prazo") {
				t.Fatalf("depois de %d sessões sem avanço não virou falha: %+v", i, r)
			}
			if _, err := os.Stat(filepath.Join(pasta, "contas", "ana@exemplo.com.br", MarcadorDeFalha)); err != nil {
				t.Fatal("a falha não deixou o marcador para o painel")
			}
		}
	}
}

func TestRodizioPegaMensagemAntigaQueChegouDepois(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Uma caixa migrada: chega uma mensagem de março de 2025, mês fechado.
	z.contas["ana@exemplo.com.br"].mensagens = append(z.contas["ana@exemplo.com.br"].mensagens, data("2025-03-20T10:00:00Z"))
	for i := 1; i <= MesesDeTodas+1; i++ {
		o.Agora = agora.Add(time.Duration(i) * time.Hour)
		if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
			t.Fatal(err)
		}
		if existe(dir, "caixa-2025-03.tgz") {
			if ler(t, filepath.Join(dir, "caixa-2025-03.tgz")) != "2025-03-20T10:00:00Z" || existe(dir, "caixa-2025-03.vazio") {
				t.Fatal("o mês conferido ficou com o conteúdo errado")
			}
			return
		}
	}
	t.Fatal("o rodízio deu a volta na janela sem pegar a mensagem que chegou depois")
}

func TestEstruturaSoTrocaSeMudou(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	arq := filepath.Join(pasta, "contas", "ana@exemplo.com.br", "estrutura.tgz")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	o.Agora = agora.Add(time.Hour)
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if fi, _ := os.Stat(arq); !fi.ModTime().Equal(agora) {
		t.Fatal("os contatos sem mudança foram regravados (subiriam de novo a cada sessão)")
	}
	z.contas["ana@exemplo.com.br"].estrutura = " editado"
	o.Agora = agora.Add(2 * time.Hour)
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if ler(t, arq) != "ESTRUTURA editado" {
		t.Fatal("o contato editado não foi para o backup")
	}
}

func TestContaQueSaiFicaMarcada(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard)
	delete(z.contas, "quebra@exemplo.com.br")
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Hour)}, io.Discard)
	if !existe(filepath.Join(pasta, "contas", "quebra@exemplo.com.br"), MarcadorForaDoBackup) {
		t.Fatal("a conta apagada do Zimbra não ficou marcada")
	}
	if existe(filepath.Join(pasta, "contas", "ana@exemplo.com.br"), MarcadorForaDoBackup) {
		t.Fatal("a conta exportada ficou marcada como fora")
	}
	if !existe(filepath.Join(pasta, "contas", "quebra@exemplo.com.br"), "conta.json") {
		t.Fatal("os arquivos da conta que saiu foram apagados (são o último backup dela)")
	}
}

func TestUmaExportacaoPorPasta(t *testing.T) {
	pasta := t.TempDir()
	soltar, err := travar(pasta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := travar(pasta); err == nil || !strings.Contains(err.Error(), "outra exportação") {
		t.Fatalf("a segunda exportação na mesma pasta passou: %v", err)
	}
	soltar()
	s2, err := travar(pasta)
	if err != nil {
		t.Fatalf("depois de solta, a pasta continuou travada: %v", err)
	}
	s2()
}

func TestAssinaturaQueFalhaNaoImpedeAsMensagens(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	delete(z.contas, "ana@exemplo.com.br")
	z.limpar()
	// A conta recriada cai numa classe com teto de assinaturas.
	var log bytes.Buffer
	z.mu.Lock()
	z.semAssinarAoCriar = true
	z.mu.Unlock()
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir}, &log); err != nil {
		t.Fatalf("a assinatura que falhou parou a restauração: %v", err)
	}
	if z.contar("POST") == 0 || !strings.Contains(log.String(), "AVISO") {
		t.Fatalf("as mensagens não voltaram, ou o aviso faltou:\n%s", log.String())
	}
}

// exportarAna exporta ana e devolve a pasta da conta.
func exportarAna(t *testing.T, z *zimbraFalso) string {
	t.Helper()
	pasta := filepath.Join(t.TempDir(), "zimbra")
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Contas: []string{"ana@exemplo.com.br"}, Agora: agora}, io.Discard); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(pasta, "contas", "ana@exemplo.com.br")
}

func TestRestaurarRecriaSemSenhaNemEncaminhamento(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	delete(z.contas, "ana@exemplo.com.br") // a conta foi apagada
	z.limpar()
	var log bytes.Buffer
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir}, &log); err != nil {
		t.Fatal(err)
	}
	cria := z.corpos["CreateAccountRequest"][0]
	at := atributosDoCorpo(cria)
	if at.um("displayName") != "Ana Souza" || at.um("zimbraCOSId") != "cos-exec" || !strings.Contains(at.um("zimbraMailSieveScript"), "Financeiro") {
		t.Fatalf("configuração não recriada: %v", at)
	}
	for _, nao := range []string{"userPassword", "zimbraPrefMailForwardingAddress", "zimbraPrefOutOfOfficeReply", "zimbraMailForwardingAddress", "zimbraTwoFactorAuthSecret", "zimbraId", "zimbraPrefDefaultSignatureId"} {
		if _, ok := at[nao]; ok {
			t.Errorf("a recriação levou %s", nao)
		}
	}
	if s, _ := cria["password"].(string); len(s) < 32 || s == "VALUE-BLOCKED" {
		t.Errorf("senha da conta recriada: %q", s)
	}
	if !strings.Contains(log.String(), "não reaplicados") || !strings.Contains(log.String(), "zimbraPrefMailForwardingAddress") || !strings.Contains(log.String(), "defina a senha") {
		t.Errorf("o log não diz o que ficou de fora:\n%s", log.String())
	}
	if z.contar("AddAccountAliasRequest") != 1 || z.contar("CreateSignatureRequest") != 1 {
		t.Errorf("apelido ou assinatura não voltou: %v", z.pedidos)
	}
	padrao := z.corpos["ModifyAccountRequest"][len(z.corpos["ModifyAccountRequest"])-1]
	if atributosDoCorpo(padrao).um("zimbraPrefDefaultSignatureId") != "sig-nova" {
		t.Errorf("a assinatura padrão não aponta para a recriada: %v", padrao)
	}
	var posts []string
	for _, p := range z.rest {
		if strings.HasPrefix(p, "POST") {
			posts = append(posts, p)
		}
	}
	if len(posts) < 3 || !strings.HasPrefix(posts[0], "POST ana@exemplo.com.br skip 2019") || posts[len(posts)-1] != "POST ana@exemplo.com.br skip ESTRUTURA" {
		t.Fatalf("importação fora de ordem:\n%s", strings.Join(posts, "\n"))
	}
	for _, p := range posts {
		if !strings.Contains(p, " skip ") {
			t.Fatalf("importação que não mescla: %s", p)
		}
	}
}

// A restauração só mescla (Hugo, 19/10): o que apaga ou substitui o que está
// na caixa é recusado antes de qualquer pedido ao Zimbra.
func TestRestaurarSoMescla(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	for _, r := range []string{"reset", "replace", "modify"} {
		z.limpar()
		if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Resolver: r}, io.Discard); err == nil {
			t.Errorf("--resolve %s foi aceito", r)
		}
		for _, p := range z.rest {
			if strings.HasPrefix(p, "POST") {
				t.Errorf("--resolve %s chegou a importar: %s", r, p)
			}
		}
	}
}

func TestRestaurarContaQueExisteSoMensagens(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	z.limpar()
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if z.contar("ModifyAccountRequest") != 0 || z.contar("CreateAccountRequest") != 0 {
		t.Fatalf("mexeu na configuração de uma conta que existe sem --settings: %v", z.pedidos)
	}
	if z.contar("POST") == 0 {
		t.Fatal("as mensagens não voltaram")
	}
	// Com --settings e --with-forwarding: a configuração volta, o
	// encaminhamento também, a senha nunca.
	z.limpar()
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Configuracao: true, Encaminhamentos: true, SemMensagens: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	at := atributosDoCorpo(z.corpos["ModifyAccountRequest"][0])
	if at.um("zimbraPrefMailForwardingAddress") != "ana@pessoal.com" || at.um("zimbraPrefOutOfOfficeReply") == "" {
		t.Errorf("--with-forwarding não reaplicou: %v", at)
	}
	if _, ok := at["userPassword"]; ok || z.contar("POST") != 0 {
		t.Errorf("senha ou mensagens no --settings-only: %v", at)
	}
}

func TestRestaurarEmOutraConta(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	z.limpar()
	// Outra conta que não existe: criada vazia (sem a configuração da
	// original) só para receber as mensagens.
	var log bytes.Buffer
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Para: "nova@exemplo.com.br"}, &log); err != nil {
		t.Fatalf("outra conta que não existe não foi criada: %v", err)
	}
	z.mu.Lock()
	criada := z.contas["nova@exemplo.com.br"]
	z.mu.Unlock()
	if criada == nil || z.contar("POST nova@exemplo.com.br") == 0 || !strings.Contains(log.String(), "criada para receber") {
		t.Fatalf("a conta nova não recebeu as mensagens: %v %q", criada, log.String())
	}
	if len(criada.at) > 0 {
		t.Fatalf("a conta nova levou a configuração da original: %v", criada.at)
	}
	z.conta("bruno@exemplo.com.br", &contaFalsa{})
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Para: "bruno@exemplo.com.br", Configuracao: true}, io.Discard); err == nil {
		t.Fatal("--settings em outra conta passou")
	}
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Para: "bruno@exemplo.com.br"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if z.contar("ModifyAccountRequest") != 0 || z.contar("POST bruno@exemplo.com.br") == 0 {
		t.Fatalf("em outra conta: só as mensagens. %v %v", z.pedidos, z.rest)
	}
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, Resolver: "apagar"}, io.Discard); err == nil {
		t.Fatal("--resolve inválido passou")
	}
}

func TestNaoSegueRedirecionamento(t *testing.T) {
	z := novoZimbra(t)
	z.redirecionar = true
	if _, err := Conectar(context.Background(), z.pasta); err == nil {
		t.Fatal("conectou seguindo um redirecionamento")
	}
	if z.desviados != 0 {
		t.Fatal("a senha do AuthRequest foi reenviada para o endereço do redirecionamento")
	}
}

func TestFiltroQueEncaminhaSoComPedido(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["ana@exemplo.com.br"].at["zimbraMailSieveScript"] = []string{"require [\"copy\"];\nredirect :copy \"atacante@mal.test\";"}
	z.contas["ana@exemplo.com.br"].at["zimbraPrefCalendarReminderEmail"] = []string{"atacante@mal.test"}
	z.contas["ana@exemplo.com.br"].at["zimbraFeatureMailForwardingInFiltersEnabled"] = []string{"TRUE"}
	dir := exportarAna(t, z)
	delete(z.contas, "ana@exemplo.com.br")
	z.limpar()
	var log bytes.Buffer
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, SemMensagens: true}, &log); err != nil {
		t.Fatal(err)
	}
	at := atributosDoCorpo(z.corpos["CreateAccountRequest"][0])
	for _, nao := range []string{"zimbraMailSieveScript", "zimbraPrefCalendarReminderEmail", "zimbraFeatureMailForwardingInFiltersEnabled"} {
		if _, ok := at[nao]; ok {
			t.Errorf("sem --with-forwarding, a recriação levou %s", nao)
		}
	}
	if !strings.Contains(log.String(), "zimbraMailSieveScript (o filtro encaminha)") {
		t.Errorf("o log não diz que o filtro ficou de fora:\n%s", log.String())
	}
	z.limpar()
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir, SemMensagens: true, Configuracao: true, Encaminhamentos: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(atributosDoCorpo(z.corpos["ModifyAccountRequest"][0]).um("zimbraMailSieveScript"), "redirect") {
		t.Error("com --with-forwarding, o filtro não voltou")
	}
}

func TestPastaDaRestauracaoFechada(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	z.limpar()
	// Aberta a outros usuários: quem escreve ali escolheria o que o root aplica.
	os.Chmod(dir, 0o777)
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir}, io.Discard); err == nil || !strings.Contains(err.Error(), "outros usuários escrevem") {
		t.Fatalf("restaurou de uma pasta aberta: %v", err)
	}
	os.Chmod(dir, 0o700)
	// conta.json trocado por um link para outro arquivo.
	outro := filepath.Join(t.TempDir(), "plantado.json")
	os.WriteFile(outro, []byte(`{"conta":"ana@exemplo.com.br","atributos":{"zimbraMailSieveScript":["redirect \"x@mal.test\";"]}}`), 0o600)
	os.Remove(filepath.Join(dir, "conta.json"))
	if err := os.Symlink(outro, filepath.Join(dir, "conta.json")); err != nil {
		t.Fatal(err)
	}
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: dir}, io.Discard); err == nil || !strings.Contains(err.Error(), "não é um arquivo comum") {
		t.Fatalf("seguiu o link no lugar do conta.json: %v", err)
	}
	if z.contar("CreateAccountRequest")+z.contar("ModifyAccountRequest")+z.contar("POST") != 0 {
		t.Fatal("aplicou alguma coisa antes de recusar a pasta")
	}
}

func (z *zimbraFalso) lento(atraso time.Duration, trecho string) {
	z.mu.Lock()
	z.atraso, z.lentoSe = atraso, trecho
	z.mu.Unlock()
}

func TestUmaContaParadaNaoSeguraAsOutras(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	pasta := filepath.Join(t.TempDir(), "zimbra")
	if _, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Só a caixa de ana demora mais que o prazo, sessão após sessão: ela vira
	// falha, mas o comando sai com sucesso — quebra@ exportou o mês corrente,
	// e ele tem de subir.
	z.lento(time.Hour, "ana@")
	defer z.lento(0, "")
	var inv *Inventario
	var err error
	for i := 1; i <= maxSemAvanco; i++ {
		inv, err = Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Duration(i) * 24 * time.Hour), Prazo: time.Now().Add(folgaDoPrazo + 3*time.Second)}, io.Discard)
	}
	var ana ResultadoDaConta
	for _, r := range inv.Contas {
		if r.Conta == "ana@exemplo.com.br" {
			ana = r
		}
	}
	if err != nil || ana.Erro == "" {
		t.Fatalf("quer ana com falha e o comando sem erro: %v %+v", err, ana)
	}
}

func TestRodizioInterrompidoNaoDeixaPendente(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Só a conferência do mês mais antigo (2024-11) demora mais que o prazo.
	z.lento(time.Hour, "before:"+ms(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)))
	defer z.lento(0, "")
	o.Agora, o.Prazo = agora.Add(time.Hour), time.Now().Add(folgaDoPrazo+3*time.Second)
	inv, err := Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := inv.Contas[0]
	if r.Pendente || r.Erro != "" || r.SemAvanco != 0 || !r.UltimoSucesso.Equal(o.Agora) || r.Conferido != "caixa-2024-11" {
		t.Fatalf("o rodízio (opcional) cortado pelo prazo mudou o resultado da conta: %+v", r)
	}
}

func TestRodizioCobreAsAnteriores(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	// Um imapsync de anos atrás, depois do primeiro backup.
	z.contas["ana@exemplo.com.br"].mensagens = append(z.contas["ana@exemplo.com.br"].mensagens, data("2016-06-01T10:00:00Z"))
	for i := 1; i <= MesesDeTodas+3; i++ {
		o.Agora = agora.Add(time.Duration(i) * time.Hour)
		Exportar(context.Background(), z.cliente(t), o, io.Discard)
		if strings.Contains(ler(t, filepath.Join(dir, "anteriores.tgz")), "2016-06-01") {
			return
		}
	}
	t.Fatal("o rodízio deu a volta sem conferir as anteriores")
}

func TestExportacaoManualNaoApagaAsOutras(t *testing.T) {
	z := servidorDeExemplo(t)
	z.contas["quebra@exemplo.com.br"].quebrada = false
	pasta := filepath.Join(t.TempDir(), "zimbra")
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard)
	// À mão, só ana: quebra continua no inventário e não fica "fora do backup".
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Hour), Contas: []string{"ana@exemplo.com.br"}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, r := range inv.Contas {
		ok = ok || (r.Conta == "quebra@exemplo.com.br" && r.UltimoSucesso.Equal(agora))
	}
	if !ok || existe(filepath.Join(pasta, "contas", "quebra@exemplo.com.br"), MarcadorForaDoBackup) {
		t.Fatalf("a exportação de uma conta só apagou o estado da outra: %+v", inv.Contas)
	}
	// Excluída pelo plano: aí sim, fora do backup.
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(2 * time.Hour), Excluir: []string{"Quebra@exemplo.com.br"}}, io.Discard)
	if !existe(filepath.Join(pasta, "contas", "quebra@exemplo.com.br"), MarcadorForaDoBackup) {
		t.Fatal("a conta excluída pelo plano não ficou marcada")
	}
}

func TestPendenteQueAvancaPerdeOMarcador(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	os.MkdirAll(dir, 0o700)
	os.Chmod(pasta, 0o700)
	os.Chmod(filepath.Join(pasta, "contas"), 0o700)
	os.WriteFile(filepath.Join(dir, MarcadorDeFalha), []byte("velho"), 0o600)
	// Primeira sessão, cortada no meio dos meses: saíram arquivos novos.
	z.lento(100*time.Millisecond, "")
	defer z.lento(0, "")
	inv, err := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}, Prazo: time.Now().Add(folgaDoPrazo + time.Second)}, io.Discard)
	if err != nil || !inv.Contas[0].Pendente || inv.Contas[0].Novos == 0 {
		t.Fatalf("quer pendente com arquivos novos: %v %+v", err, inv.Contas)
	}
	if existe(dir, MarcadorDeFalha) {
		t.Fatal("a conta que avançou continuou marcada como falha")
	}
}

func TestMesQueCorreSemNovidadeNaoSobeDeNovo(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	arq := filepath.Join(pasta, "contas", "ana@exemplo.com.br", "caixa-2026-10.tgz")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	antes := ler(t, arq)
	b1, _ := os.ReadFile(arq)
	o.Agora = agora.Add(time.Hour)
	inv, _ := Exportar(context.Background(), z.cliente(t), o, io.Discard)
	b2, _ := os.ReadFile(arq)
	fi, _ := os.Stat(arq)
	if !bytes.Equal(b1, b2) || ler(t, arq) != antes || inv.Contas[0].Exportado != 0 {
		t.Fatalf("o mês corrente sem mensagem nova foi regravado (subiria de novo): exportados %d", inv.Contas[0].Exportado)
	}
	if !fi.ModTime().Equal(o.Agora) {
		t.Fatal("a data do mês corrente não avançou: ele nunca ficaria fechado")
	}
}

func TestPastaDeCimaAbertaRecusada(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	// Uma pasta do caminho, sem o bit sticky, que outros usuários alteram:
	// trocariam a pasta da conta pela sua depois da conferência.
	aberta := filepath.Join(t.TempDir(), "aberta")
	os.MkdirAll(filepath.Join(aberta, "r"), 0o700)
	os.Chmod(aberta, 0o777)
	destino := filepath.Join(aberta, "r", "ana@exemplo.com.br")
	if err := os.Rename(dir, destino); err != nil {
		t.Fatal(err)
	}
	if err := Restaurar(context.Background(), z.cliente(t), OpcoesDeRestauracao{De: destino}, io.Discard); err == nil || !strings.Contains(err.Error(), "outros usuários escrevem em "+aberta) {
		t.Fatalf("restaurou com uma pasta de cima aberta: %v", err)
	}
}

func TestFiltroComRespostaAutomaticaFicaDeFora(t *testing.T) {
	for _, sv := range []string{`require "vacation"; vacation "Fora";`, `require "reply"; reply "ok";`} {
		_, _, fora := atributosParaOZimbra(atributos{"zimbraMailSieveScript": {sv}}, false)
		if len(fora) != 1 {
			t.Errorf("%q voltaria sem --with-forwarding", sv)
		}
	}
}

func TestLinkNoCaminhoDaRestauracao(t *testing.T) {
	z := servidorDeExemplo(t)
	dir := exportarAna(t, z)
	base := t.TempDir()
	// Link de sistema numa pasta fechada (/home → var/home): passa.
	link := filepath.Join(base, "home")
	if err := os.Symlink(filepath.Dir(dir), link); err != nil {
		t.Fatal(err)
	}
	if _, err := conferirPastaDaRestauracao(filepath.Join(link, "ana@exemplo.com.br")); err != nil {
		t.Fatalf("recusou um link numa pasta fechada: %v", err)
	}
	// A própria pasta da conta como link: o nome dela decide a conta de
	// destino, e o que ela aponta pode mudar depois da conferência.
	os.Symlink(dir, filepath.Join(filepath.Dir(dir), "bruno@exemplo.com.br"))
	if _, err := conferirPastaDaRestauracao(filepath.Join(filepath.Dir(dir), "bruno@exemplo.com.br")); err == nil {
		t.Fatal("aceitou a pasta da conta como link")
	}
	// O mesmo link numa pasta sticky aberta a todos: quem o criou o refaz.
	aberta := filepath.Join(base, "tmp")
	os.Mkdir(aberta, 0o700)
	os.Chmod(aberta, 0o1777)
	os.Symlink(filepath.Dir(dir), filepath.Join(aberta, "x"))
	if _, err := conferirPastaDaRestauracao(filepath.Join(aberta, "x", "ana@exemplo.com.br")); err == nil {
		t.Fatal("aceitou um link numa pasta sticky aberta a todos")
	}
	// Link apontando para uma pasta aberta: o caminho resolvido é conferido.
	solta := filepath.Join(base, "solta")
	os.MkdirAll(filepath.Join(solta, "ana@exemplo.com.br"), 0o700)
	os.Chmod(solta, 0o777)
	os.Symlink(solta, filepath.Join(base, "atalho"))
	if _, err := conferirPastaDaRestauracao(filepath.Join(base, "atalho", "ana@exemplo.com.br")); err == nil {
		t.Fatal("aceitou um link para uma pasta que outros alteram")
	}
}

func TestMesQueFicouParaTrasDaJanelaAindaSai(t *testing.T) {
	z := servidorDeExemplo(t)
	// Novembro de 2024 falha enquanto está na janela.
	z.contas["ana@exemplo.com.br"].mesQuebrado = "before:" + ms(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "ana@exemplo.com.br")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	// A janela anda dois meses (2024-11 fica para trás) e o Zimbra volta.
	z.contas["ana@exemplo.com.br"].mesQuebrado = ""
	o.Agora = agora.AddDate(0, 2, 0)
	if _, err := Exportar(context.Background(), z.cliente(t), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !existe(dir, "caixa-2024-11.vazio") && !existe(dir, "caixa-2024-11.tgz") {
		t.Fatal("o mês que falhou e saiu da janela sumiu do backup")
	}
	if strings.Contains(ler(t, filepath.Join(dir, "anteriores.tgz")), "2024-11") {
		t.Fatal("o mês saiu duas vezes")
	}
}

func TestContaDeForaNaoDuplicaNoInventario(t *testing.T) {
	z := servidorDeExemplo(t)
	z.conta("filial@exemplo.com.br", &contaFalsa{host: "mail2.exemplo.com.br"})
	pasta := filepath.Join(t.TempDir(), "zimbra")
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard)
	inv, _ := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Hour), Contas: []string{"ana@exemplo.com.br", "filial@exemplo.com.br"}}, io.Discard)
	n := 0
	for _, r := range inv.Contas {
		if r.Conta == "filial@exemplo.com.br" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a conta de outro servidor aparece %d vezes no inventário", n)
	}
}

func TestEstruturaLentaNaoTiraOMesCorrente(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	arq := filepath.Join(pasta, "contas", "ana@exemplo.com.br", "caixa-2026-10.tgz")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	// Os documentos da conta passam a demorar mais que o prazo.
	z.lento(time.Hour, "contact,")
	defer z.lento(0, "")
	z.contas["ana@exemplo.com.br"].mensagens = append(z.contas["ana@exemplo.com.br"].mensagens, data("2026-10-15T10:00:00Z"))
	o.Agora, o.Prazo = agora.Add(time.Hour), time.Now().Add(folgaDoPrazo+3*time.Second)
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if !strings.Contains(ler(t, arq), "2026-10-15") {
		t.Fatal("a estrutura lenta tirou o lugar do e-mail de hoje")
	}
}

func TestRodizioCobreAsAnterioresVazias(t *testing.T) {
	z := servidorDeExemplo(t)
	z.conta("nova@exemplo.com.br", &contaFalsa{mensagens: []time.Time{data("2026-10-02T10:00:00Z")}})
	pasta := filepath.Join(t.TempDir(), "zimbra")
	dir := filepath.Join(pasta, "contas", "nova@exemplo.com.br")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"nova@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if !existe(dir, "anteriores.vazio") {
		t.Fatal("a conta nova não tem anteriores.vazio")
	}
	// Uma caixa de anos migrada para a conta que começou vazia.
	z.contas["nova@exemplo.com.br"].mensagens = append(z.contas["nova@exemplo.com.br"].mensagens, data("2015-01-10T10:00:00Z"))
	for i := 1; i <= MesesDeTodas+3; i++ {
		o.Agora = agora.Add(time.Duration(i) * time.Hour)
		Exportar(context.Background(), z.cliente(t), o, io.Discard)
		if existe(dir, "anteriores.tgz") {
			if !strings.Contains(ler(t, filepath.Join(dir, "anteriores.tgz")), "2015-01-10") || existe(dir, "anteriores.vazio") {
				t.Fatal("as anteriores conferidas ficaram erradas")
			}
			return
		}
	}
	t.Fatal("o rodízio nunca conferiu as anteriores que tinham saído vazias")
}

func TestRodizioQueFalhaNaoDerrubaOBackup(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	o := Opcoes{Pasta: pasta, Agora: agora, Contas: []string{"ana@exemplo.com.br"}}
	Exportar(context.Background(), z.cliente(t), o, io.Discard)
	// Nada muda na caixa, e a conferência do mês mais antigo falha.
	z.contas["ana@exemplo.com.br"].mesQuebrado = "before:" + ms(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))
	o.Agora = agora.Add(time.Hour)
	inv, err := Exportar(context.Background(), z.cliente(t), o, io.Discard)
	if err != nil {
		t.Fatalf("a conferência (opcional) que falhou derrubou o backup: %v", err)
	}
	if inv.Falhas != 1 || !existe(filepath.Join(pasta, "contas", "ana@exemplo.com.br"), MarcadorDeFalha) {
		t.Fatalf("a falha da conferência sumiu: %+v", inv)
	}
}

func TestContaDeForaGuardaOQueSeSabia(t *testing.T) {
	z := servidorDeExemplo(t)
	pasta := filepath.Join(t.TempDir(), "zimbra")
	Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora}, io.Discard)
	z.contas["ana@exemplo.com.br"].host = "mail2.exemplo.com.br" // a caixa mudou de servidor
	inv, _ := Exportar(context.Background(), z.cliente(t), Opcoes{Pasta: pasta, Agora: agora.Add(time.Hour)}, io.Discard)
	for _, r := range inv.Contas {
		if r.Conta == "ana@exemplo.com.br" && (r.Corte == "" || !r.UltimoSucesso.Equal(agora)) {
			t.Fatalf("a conta que foi para outro servidor perdeu o corte e o último sucesso: %+v", r)
		}
	}
}
