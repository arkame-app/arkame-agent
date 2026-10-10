package atualizacao

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Os testes deste arquivo nasceram das sondas da revisão de segurança da
// atualização automática: cada um reproduz um desvio que a sonda achou e cobra
// a conferência que o fechou.

// O redirecionamento passa pela mesma regra do endereço de partida. A base é
// aceita (http da própria máquina) e responde 302 para um endereço que ela
// mesma não poderia ser — aqui, com usuário e senha; a regra é a mesma que
// barra o https que manda para um http de fora. O download não segue.
func TestRedirecionamentoPassaPelaRegraDoEndereco(t *testing.T) {
	b := novaBancada(t)
	u, _ := url.Parse(b.srv.URL)
	alvoProibido := "http://u:p@" + u.Host
	if enderecoAceito(alvoProibido) {
		t.Fatal("premissa: o alvo deveria ser recusado como base")
	}
	porta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, alvoProibido+r.URL.Path, http.StatusFound)
	}))
	defer porta.Close()
	b.pedido.BaseURL = porta.URL
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaDownload)
	b.rel.mu.Lock()
	defer b.rel.mu.Unlock()
	if len(b.rel.pedidos) != 0 {
		t.Fatalf("o alvo do redirecionamento recusado foi pedido: %v", b.rel.pedidos)
	}
}

// A regra de cada salto: https, ou http só da própria máquina; sem usuário e
// senha; consulta aceita (o endereço assinado do armazenamento do GitHub é
// uma); e um teto de saltos.
func TestConferirSalto(t *testing.T) {
	salto := func(endereco string, anteriores int) error {
		u, err := url.Parse(endereco)
		if err != nil {
			t.Fatal(err)
		}
		return conferirSalto(&http.Request{URL: u}, make([]*http.Request, anteriores))
	}
	for _, bom := range []string{
		"https://objects.githubusercontent.com/x/arkame-agent_linux_amd64?X-Amz-Signature=abc",
		"http://127.0.0.1:8080/v0.5.0/checksums.txt",
		"http://localhost/v0.5.0/checksums.txt",
	} {
		if err := salto(bom, 1); err != nil {
			t.Errorf("%s recusado: %v", bom, err)
		}
	}
	for _, ruim := range []string{
		"http://exemplo.invalid/v0.5.0/arkame-agent_linux_amd64",
		"http://169.254.169.254/latest/meta-data/",
		"https://u:p@exemplo.invalid/x",
		"ftp://exemplo.invalid/x",
		"file:///etc/passwd",
	} {
		if err := salto(ruim, 1); err == nil {
			t.Errorf("%s aceito como alvo de redirecionamento", ruim)
		}
	}
	if err := salto("https://exemplo.invalid/x", maximoDeSaltos); err == nil {
		t.Error("seguiu além do teto de saltos")
	}
	if ClientePadrao().CheckRedirect == nil {
		t.Error("o ClientePadrao não confere os redirecionamentos")
	}
}

// Base com consulta, fragmento ou usuário: o /v<versão>/<arquivo> colado no
// fim viraria valor de parâmetro, e o pedido iria a outro endereço.
func TestBaseComConsultaFragmentoOuUsuarioERecusada(t *testing.T) {
	v, _ := LerVersao("1.2.3")
	for _, base := range []string{
		"https://interno.exemplo/qualquer/caminho?x=",
		"https://interno.exemplo/caminho?",
		"https://interno.exemplo/caminho#frag",
		"https://usuario:senha@interno.exemplo/caminho",
		"https://usuario@interno.exemplo/caminho",
	} {
		if bases := basesDaVersao(base, "", v); len(bases) != 0 {
			t.Errorf("%s aceita: o pedido iria a %s", base, bases[0]+"/"+ArquivoDeChecksums)
		}
	}
	if bases := basesDaVersao("https://interno.exemplo/releases/", "", v); len(bases) != 1 || bases[0] != "https://interno.exemplo/releases/v1.2.3" {
		t.Errorf("a base comum deixou de valer: %v", bases)
	}
}

// O `.novo` que já existe como link simbólico não é seguido: o alvo do link
// fica como estava, e o que vai para o lugar do programa é um arquivo comum.
func TestNovoComoLinkSimbolicoNaoESeguido(t *testing.T) {
	b := novaBancada(t)
	vitima := filepath.Join(t.TempDir(), "vitima")
	if err := os.WriteFile(vitima, []byte("conteudo importante\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vitima, b.lugar.Novo); err != nil {
		t.Fatal(err)
	}
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a troca devia tirar o link do caminho e seguir: %v", err)
	}
	if c, _ := os.ReadFile(vitima); string(c) != "conteudo importante\n" {
		t.Fatalf("o download seguiu o link e sobrescreveu o alvo: %q", c)
	}
	if fi, err := os.Stat(vitima); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("o alvo do link mudou de permissão: %v %v", fi, err)
	}
	fi, err := os.Lstat(b.lugar.Atual)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("o que está no lugar do programa não é um arquivo comum: %v %v", fi, err)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa no lugar não é o novo:\n%s", got)
	}
}

// O `.novo` trocado por outra coisa entre o download e a execução (aqui, uma
// pasta) não ganha permissão de execução nem é executado.
func TestNovoQueNaoEArquivoComumNaoEExecutado(t *testing.T) {
	b := novaBancada(t)
	antes := criarNovo
	t.Cleanup(func() { criarNovo = antes })
	alvo := filepath.Join(t.TempDir(), "outro-programa")
	criarNovo = func(caminho string) (arquivoNovo, error) {
		// O download vai para outro arquivo e o `.novo` vira um link para
		// ele: o SHA-256 lido pelo link conferiria.
		if err := os.Symlink(alvo, caminho); err != nil {
			return nil, err
		}
		return os.OpenFile(alvo, os.O_CREATE|os.O_WRONLY, 0o700)
	}
	executou := false
	b.opcoes.Executar = executarQueMarca(&executou)
	_, err := b.aplicar()
	var f *Falha
	if !errors.As(err, &f) || f.Codigo != FalhaPermissoes {
		t.Fatalf("queria a recusa do arquivo que não é comum, veio %v", err)
	}
	if executou {
		t.Fatal("o link foi executado")
	}
	if got := b.noLugar(); got != string(b.atual) {
		t.Fatal("o programa atual foi trocado")
	}
}

// O `.anterior` que virou link não volta ao lugar do programa: a prova que
// vence fica sem volta, e o programa novo segue.
func TestAnteriorComoLinkNaoVoltaAoLugar(t *testing.T) {
	b := trocada(t)
	// O alvo do link tem o conteúdo do programa que saiu: o SHA-256 lido pelo
	// link confere, e quem barra é só a exigência de arquivo comum.
	outro := filepath.Join(t.TempDir(), "outro")
	if err := os.WriteFile(outro, b.atual, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outro, b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	if AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("voltou para um `.anterior` que é link")
	}
	if fi, err := os.Lstat(b.lugar.Atual); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("o link foi para o lugar do programa: %v %v", fi, err)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa novo saiu do lugar:\n%s", got)
	}
	if e := b.estado(); e.Situacao != SituacaoSemVolta {
		t.Fatalf("registro = %+v", e)
	}
}

// O `.anterior` trocado por outro arquivo (comum, mas que não é o programa que
// saiu na troca) também não volta.
func TestAnteriorAdulteradoNaoVoltaAoLugar(t *testing.T) {
	b := trocada(t)
	if b.estado().Sha256Anterior != resumoDe(b.atual) {
		t.Fatalf("a troca não registrou o SHA-256 do programa que saiu: %+v", b.estado())
	}
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.lugar.Anterior, []byte("#!/bin/sh\necho invasor\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("voltou para um `.anterior` que não é o programa que saiu")
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o arquivo adulterado foi para o lugar do programa:\n%s", got)
	}
}

// O checksums.txt e a assinatura são uma unidade por base: a primeira base
// responde 200 com lixo, e a reserva, boa, é tentada antes de falhar.
func TestAssinaturaRuimNaPrimeiraBaseCaiNaReserva(t *testing.T) {
	b := novaBancada(t)
	ruim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("lixo\n"))
	}))
	defer ruim.Close()
	b.pedido.BaseURL = ruim.URL
	b.opcoes.Reserva = b.srv.URL
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a reserva tinha o release certo e não foi usada: %v", err)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa no lugar não é o do release:\n%s", got)
	}
}

// O par não se mistura: o checksums.txt de uma base com a assinatura da outra
// não vale. A primeira base tem o checksums.txt certo e não tem assinatura; a
// reserva tem assinatura (de outro conteúdo) e outro checksums.txt.
func TestChecksumsEAssinaturaNaoSeMisturamEntreBases(t *testing.T) {
	b := novaBancada(t)
	b.rel.mu.Lock()
	certo := b.rel.checksums
	b.rel.mu.Unlock()
	primeira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+ArquivoDeChecksums) {
			_, _ = w.Write(certo)
			return
		}
		http.NotFound(w, r)
	}))
	defer primeira.Close()
	b.rel.mu.Lock()
	b.rel.checksums = []byte("lixo\n") // a assinatura da reserva é do `certo`
	b.rel.mu.Unlock()
	b.pedido.BaseURL = primeira.URL
	b.opcoes.Reserva = b.srv.URL
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)
}

// Com as duas bases ruins, o erro é o de assinatura — o mais grave — mesmo
// que a última a falhar tenha sido só um download.
func TestAssinaturaRuimEReservaForaDoArFalhaPelaAssinatura(t *testing.T) {
	b := novaBancada(t)
	ruim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("lixo\n"))
	}))
	defer ruim.Close()
	fora := httptest.NewServer(http.NotFoundHandler())
	defer fora.Close()
	b.pedido.BaseURL = ruim.URL
	b.opcoes.Reserva = fora.URL
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)
}

// O nome de arquivo do pedido é só conferência: o programa baixado é sempre o
// deste sistema. Um nome diferente — outra linha do mesmo checksums.txt
// assinado, com o SHA-256 dela — recusa o pedido.
func TestArquivoDoPedidoESoConferencia(t *testing.T) {
	b := novaBancada(t)
	outro := append(programaFalso("0.5.0", 0), []byte("# outro artefato do mesmo release\n")...)
	b.rel.mu.Lock()
	b.rel.checksums = []byte(resumoDe(b.rel.programa) + "  " + NomeNoRelease() + "\n" + resumoDe(outro) + "  arkame-agent_outra_plataforma\n")
	b.rel.assinatura = Assinar(b.priv, b.versaoV, b.rel.checksums)
	b.rel.servir["arkame-agent_outra_plataforma"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(outro) }
	b.rel.mu.Unlock()

	b.pedido.Arquivo = "arkame-agent_outra_plataforma"
	b.pedido.Sha256 = resumoDe(outro)
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaArquivoDoPedido)
	b.rel.mu.Lock()
	pedidos := len(b.rel.pedidos)
	b.rel.mu.Unlock()
	if pedidos != 0 {
		t.Fatalf("o pedido recusado chegou a baixar alguma coisa: %d pedidos", pedidos)
	}

	// Nome que sai da pasta: a mesma recusa, antes de qualquer download.
	b.pedido.Arquivo = "../../etc/passwd"
	_, err = b.aplicar()
	b.exigirFalha(err, FalhaArquivoDoPedido)

	// O nome certo, ou nenhum, segue valendo.
	b.pedido.Arquivo = NomeNoRelease()
	b.pedido.Sha256 = resumoDe(b.rel.programa)
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("o pedido com o nome deste sistema foi recusado: %v", err)
	}
}

// Formas da assinatura e da chave que não podem valer.
func TestAssinaturaEChaveMalformadasNaoValem(t *testing.T) {
	v, _ := LerVersao("0.5.0")
	pub, priv := chavesDeTesteGeradas(t)
	cs := []byte("x\n")
	for nome, sig := range map[string][]byte{
		"vazia":          nil,
		"só espaços":     []byte("  \n"),
		"64 zeros":       []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="),
		"duas linhas":    append(Assinar(priv, v, cs), Assinar(priv, v, cs)...),
		"outra mensagem": Assinar(priv, v, []byte("y\n")),
	} {
		if err := VerificarAssinatura([]ed25519.PublicKey{pub}, v, cs, sig); err == nil {
			t.Errorf("assinatura %s: aceita", nome)
		}
	}
	if _, err := LerChavePublica(""); err == nil {
		t.Error("chave vazia aceita")
	}
	// A versão e o conteúdo não se confundem na mensagem assinada: "0.5.0" +
	// "1\n…" não é "0.5.01" + "\n…".
	v2, _ := LerVersao("0.5.1")
	if string(MensagemAssinada(v, []byte("1\nresto"))) == string(MensagemAssinada(v2, []byte("\nresto"))) {
		t.Error("a mensagem assinada de duas versões colide")
	}
}
