package atualizacao

import (
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A troca certa: o programa novo entra, o atual fica ao lado como anterior e
// o registro diz que a versão nova está em prova.
func TestTrocaPoeONovoNoLugarEGuardaOAnterior(t *testing.T) {
	b := novaBancada(t)
	res, err := b.aplicar()
	if err != nil {
		t.Fatalf("a troca falhou: %v", err)
	}
	if res.Versao != "0.5.0" || res.JaNoLugar {
		t.Fatalf("resultado = %+v", res)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("no lugar ficou:\n%s", got)
	}
	anterior, err := os.ReadFile(b.lugar.Anterior)
	if err != nil || string(anterior) != string(b.atual) {
		t.Fatalf("o anterior não é o programa que estava no lugar: %v\n%s", err, anterior)
	}
	if fi, _ := os.Stat(b.lugar.Atual); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("o programa novo não é executável: %v", fi.Mode())
	}
	e := b.estado()
	if e.Situacao != SituacaoEmProva || e.Versao != "0.5.0" || e.VersaoAnterior != "0.4.31" || e.Sha256 != b.pedido.Sha256 {
		t.Fatalf("registro = %+v", e)
	}
	for _, resto := range []string{b.lugar.Novo, b.lugar.Trava} {
		if _, err := os.Lstat(resto); err == nil {
			t.Fatalf("sobrou %s depois da troca", resto)
		}
	}
}

// Canal 1 × canal 2: o SHA-256 que o painel mandou não é o do checksums.txt
// assinado. Quem comprometeu só o painel não empurra programa — e a troca
// para antes de baixar o programa.
func TestShaDoPainelDiferenteDoAssinadoRecusa(t *testing.T) {
	b := novaBancada(t)
	b.pedido.Sha256 = strings.Repeat("ab", 32)
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaResumoDoPainel)
	for _, p := range b.rel.pedidos {
		if strings.HasSuffix(p, NomeNoRelease()) {
			t.Fatalf("o programa foi baixado mesmo com o SHA-256 do painel diferente: %v", b.rel.pedidos)
		}
	}
}

// O arquivo servido não é o do checksums.txt assinado (download adulterado):
// o painel e a assinatura estão certos, o arquivo não.
func TestArquivoBaixadoComShaErradoRecusa(t *testing.T) {
	b := novaBancada(t)
	b.rel.servir[NomeNoRelease()] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(programaFalso("0.5.0-adulterado", 0))
	}
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaResumoDoArquivo)
}

// SHA-256 certo nos dois canais, assinatura estragada: recusa.
func TestShaCertoComAssinaturaInvalidaRecusa(t *testing.T) {
	b := novaBancada(t)
	ruim := []byte(string(b.rel.assinatura))
	// Troca um caractere do base64 (continua base64 válido de 64 bytes).
	if ruim[10] == 'A' {
		ruim[10] = 'B'
	} else {
		ruim[10] = 'A'
	}
	b.rel.assinatura = ruim
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)

	// E o checksums.txt alterado depois de assinado (uma linha a mais).
	b = novaBancada(t)
	b.rel.checksums = append(b.rel.checksums, []byte("ffff  intruso\n")...)
	_, err = b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)

	// Arquivo de assinatura que nem é uma assinatura.
	b = novaBancada(t)
	b.rel.assinatura = []byte("não sou base64\n")
	_, err = b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)
}

// Assinatura perfeita, de uma chave que este programa não conhece: quem
// tomou o release (e o painel) mas não tem a chave de release não passa.
func TestAssinaturaDeChaveDesconhecidaRecusa(t *testing.T) {
	b := novaBancada(t)
	_, intrusa := chavesDeTesteGeradas(t)
	b.rel.assinatura = Assinar(intrusa, b.versaoV, b.rel.checksums)
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)
}

// As duas posições de chave valem: a atual e a próxima (rotação).
func TestAssinaturaDaChaveProximaVale(t *testing.T) {
	b := novaBancada(t)
	outra, _ := chavesDeTesteGeradas(t)
	b.opcoes.Chaves = []ed25519.PublicKey{outra, b.pub}
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a chave da segunda posição não valeu: %v", err)
	}
}

// Sem chave embutida, nada é confiável.
func TestSemChaveEmbutidaRecusa(t *testing.T) {
	b := novaBancada(t)
	b.opcoes.Chaves = nil
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaSemChaves)
}

// O checksums.txt assinado de um release antigo, servido como se fosse a
// versão nova: a assinatura é de verdade, mas de outra versão. Sem a versão na
// mensagem assinada, isto voltaria o agente para um programa antigo.
func TestAssinaturaDeOutraVersaoRecusa(t *testing.T) {
	b := novaBancada(t)
	antiga, _ := LerVersao("0.4.0")
	b.rel.assinatura = Assinar(b.priv, antiga, b.rel.checksums)
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAssinatura)
}

// Versão menor, e versão igual: recusa. Só o comando manual, com a opção
// explícita, volta de versão.
func TestDowngradeRecusa(t *testing.T) {
	for _, atual := range []string{"0.5.1", "0.5.0", "1.0.0"} {
		b := novaBancada(t)
		b.opcoes.VersaoAtual = atual
		_, err := b.aplicar()
		b.exigirFalha(err, FalhaNaoEMaior)
		if len(b.rel.pedidos) != 0 {
			t.Fatalf("atual %s: baixou algo antes de recusar a versão: %v", atual, b.rel.pedidos)
		}
	}
	b := novaBancada(t)
	b.opcoes.VersaoAtual = "0.6.0"
	b.pedido.PermitirReversao = true
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a reversão pedida à mão falhou: %v", err)
	}
}

// Build de desenvolvimento não tem versão para comparar: não se atualiza.
func TestVersaoDeDesenvolvimentoNaoSeAtualiza(t *testing.T) {
	for _, v := range []string{"dev", "v0.4.31-3-gabc1234", "0.4", ""} {
		b := novaBancada(t)
		b.opcoes.VersaoAtual = v
		_, err := b.aplicar()
		b.exigirFalha(err, FalhaVersaoDeDev)
	}
}

// O programa novo sai com 1 no self-test: o atual fica.
func TestProgramaNovoQueFalhaNoAutoTesteNaoEntra(t *testing.T) {
	b := novaBancada(t)
	b.trocarPrograma(programaFalso("0.5.0", 1))
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAutoTeste)
}

// O programa novo que nem executa (arquivo que não é programa; no Windows, o
// .exe sem assinatura barrado pelo Smart App Control): o atual fica.
func TestProgramaNovoQueNaoExecutaNaoEntra(t *testing.T) {
	b := novaBancada(t)
	b.trocarPrograma([]byte("isto não é um programa\n"))
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaAutoTeste)
}

// O programa novo passa no self-test mas diz ser de outra versão: recusa.
func TestProgramaNovoDeOutraVersaoNaoEntra(t *testing.T) {
	b := novaBancada(t)
	b.trocarPrograma(programaFalso("0.4.0", 0))
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaVersaoDoPrograma)
}

// escritaQueEnche aceita alguns bytes e depois devolve "disco cheio".
type escritaQueEnche struct {
	f     *os.File
	resta int
}

func (e *escritaQueEnche) Write(p []byte) (int, error) {
	if len(p) > e.resta {
		n, _ := e.f.Write(p[:e.resta])
		e.resta = 0
		return n, syscall.ENOSPC
	}
	e.resta -= len(p)
	return e.f.Write(p)
}
func (e *escritaQueEnche) Sync() error  { return e.f.Sync() }
func (e *escritaQueEnche) Close() error { return e.f.Close() }

// Disco cheio no meio do download: falha, e o pedaço baixado sai do disco.
func TestDiscoCheioNoDownloadNaoDeixaArquivoParcial(t *testing.T) {
	b := novaBancada(t)
	antes := criarNovo
	t.Cleanup(func() { criarNovo = antes })
	criarNovo = func(caminho string) (arquivoNovo, error) {
		f, err := os.OpenFile(caminho, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if err != nil {
			return nil, err
		}
		return &escritaQueEnche{f: f, resta: 10}, nil
	}
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaDownload)
	if !errors.Is(err, syscall.ENOSPC) && !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("a causa não é o disco cheio: %v", err)
	}
}

// Download cortado no meio. Com o tamanho anunciado, o corte é erro de
// download; sem ele, o arquivo chega menor e quem acusa é o SHA-256. Nos dois
// casos nada fica no disco e o atual segue.
func TestDownloadCortadoNaoEntra(t *testing.T) {
	t.Run("com Content-Length", func(t *testing.T) {
		b := novaBancada(t)
		b.rel.servir[NomeNoRelease()] = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write(b.rel.programa[:20])
		}
		_, err := b.aplicar()
		b.exigirFalha(err, FalhaDownload)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("a causa não é o corte: %v", err)
		}
	})
	t.Run("sem Content-Length", func(t *testing.T) {
		b := novaBancada(t)
		b.rel.servir[NomeNoRelease()] = func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush() // resposta em pedaços, sem tamanho
			_, _ = w.Write(b.rel.programa[:20])
		}
		_, err := b.aplicar()
		b.exigirFalha(err, FalhaResumoDoArquivo)
	})
}

// O endereço do painel falha e a reserva responde: a troca acontece.
func TestReservaDoDownload(t *testing.T) {
	b := novaBancada(t)
	b.opcoes.Reserva = b.srv.URL
	b.pedido.BaseURL = "http://127.0.0.1:1" // ninguém escuta
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a reserva não serviu: %v", err)
	}
}

// Endereço http de outra máquina não é usado; sem endereço aceito, falha.
func TestEnderecoHttpDeForaNaoEUsado(t *testing.T) {
	b := novaBancada(t)
	b.pedido.BaseURL = "http://releases.exemplo.test"
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaDownload)
	for _, e := range []string{"http://10.0.0.1/x", "ftp://a/b", "https://u:s@host/x", "arquivo", ""} {
		if enderecoAceito(e) {
			t.Errorf("endereço %q aceito", e)
		}
	}
	for _, e := range []string{"https://get.arkame.app/releases", "http://127.0.0.1:8080/r", "http://localhost/r", "http://[::1]:9/r"} {
		if !enderecoAceito(e) {
			t.Errorf("endereço %q recusado", e)
		}
	}
}

// Pedido sem SHA-256, com versão torta ou com nome de arquivo que sai da
// pasta: recusa antes de qualquer download.
func TestPedidoInvalidoRecusa(t *testing.T) {
	for nome, estragar := range map[string]func(*Pedido){
		"sem sha":        func(p *Pedido) { p.Sha256 = "" },
		"sha curto":      func(p *Pedido) { p.Sha256 = "abcd" },
		"versão torta":   func(p *Pedido) { p.Versao = "nova" },
		"arquivo com ..": func(p *Pedido) { p.Arquivo = "../../etc/passwd" },
	} {
		b := novaBancada(t)
		estragar(&b.pedido)
		_, err := b.aplicar()
		var f *Falha
		if !errors.As(err, &f) || f.Codigo != FalhaPedidoInvalido {
			t.Errorf("%s: veio %v", nome, err)
		}
		b.exigirIntacto()
	}
}

// Outra troca em curso nesta máquina (o agente vizinho, que divide o
// programa): adia, sem tocar em nada.
func TestOutraTrocaEmCursoAdia(t *testing.T) {
	b := novaBancada(t)
	destravar, ok, err := b.lugar.travar()
	if err != nil || !ok {
		t.Fatalf("travar: ok=%v err=%v", ok, err)
	}
	_, err = b.aplicar()
	var adiada *Adiada
	if !errors.As(err, &adiada) || adiada.Motivo != AdiadaOutraTroca {
		t.Fatalf("queria adiada por outra troca, veio %v", err)
	}
	if len(b.rel.pedidos) != 0 {
		t.Fatalf("baixou algo com a trava presa: %v", b.rel.pedidos)
	}
	destravar()
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("com a trava solta a troca falhou: %v", err)
	}
}

// O vizinho já fez a troca: enquanto ela está em prova, este agente espera;
// confirmada, só reinicia — sem baixar de novo.
func TestTrocaJaFeitaPeloVizinho(t *testing.T) {
	b := novaBancada(t)
	if _, err := b.aplicar(); err != nil {
		t.Fatal(err)
	}
	b.rel.pedidos = nil
	_, err := b.aplicar()
	var adiada *Adiada
	if !errors.As(err, &adiada) || adiada.Motivo != AdiadaVizinhoProva {
		t.Fatalf("em prova: queria adiada, veio %v", err)
	}
	AbrirProva(b.lugar.Atual, "0.5.0").Confirmar()
	res, err := b.aplicar()
	if err != nil || !res.JaNoLugar {
		t.Fatalf("confirmada: res=%+v err=%v", res, err)
	}
	if len(b.rel.pedidos) != 0 {
		t.Fatalf("baixou de novo o que já estava no lugar: %v", b.rel.pedidos)
	}
}

// A versão que esta máquina já pôs e reverteu não entra de novo sozinha; com
// `force` (o painel mandou tentar de novo), entra.
func TestVersaoRevertidaSoVoltaForcada(t *testing.T) {
	b := novaBancada(t)
	if _, err := b.aplicar(); err != nil {
		t.Fatal(err)
	}
	if !AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("a prova vencida não reverteu")
	}
	if got := b.noLugar(); got != string(b.atual) {
		t.Fatalf("depois da reversão, no lugar:\n%s", got)
	}
	_, err := b.aplicar()
	var f *Falha
	if !errors.As(err, &f) || f.Codigo != FalhaJaRevertida {
		t.Fatalf("queria a recusa da versão revertida, veio %v", err)
	}
	b.pedido.Forcar = true
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("forçada, a troca falhou: %v", err)
	}
	if e := b.estado(); e.Situacao != SituacaoEmProva {
		t.Fatalf("registro = %+v", e)
	}
}

// A troca pelo caminho do Windows (renomeia o atual, depois o novo) dá no
// mesmo, e a volta também.
func TestTrocaPorRenome(t *testing.T) {
	antes := trocaPorRenome
	t.Cleanup(func() { trocaPorRenome = antes })
	trocaPorRenome = true
	b := novaBancada(t)
	if _, err := b.aplicar(); err != nil {
		t.Fatal(err)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("no lugar:\n%s", got)
	}
	if !AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("não reverteu")
	}
	if got := b.noLugar(); got != string(b.atual) {
		t.Fatalf("depois da volta, no lugar:\n%s", got)
	}
}

// Falha ao gravar o registro da troca (disco cheio na hora do registro): o
// programa atual fica, e o novo sai.
func TestFalhaAoRegistrarATrocaNaoTroca(t *testing.T) {
	b := novaBancada(t)
	antes := gravarArquivo
	t.Cleanup(func() { gravarArquivo = antes })
	gravarArquivo = func(string, []byte) error { return syscall.ENOSPC }
	_, err := b.aplicar()
	b.exigirFalha(err, FalhaTroca)
}
