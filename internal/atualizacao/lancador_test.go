package atualizacao

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noVolume monta a troca pelo lançador do Docker: o programa da imagem é a
// 0.4.31, o volume está vazio, e a troca para a 0.5.0 põe lá o programa novo.
func noVolume(t *testing.T) *bancada {
	t.Helper()
	b := novaBancada(t)
	// No Docker o programa a trocar é o do volume, que ainda não existe.
	volume := filepath.Join(t.TempDir(), "bin", "arkame-agent")
	if err := os.MkdirAll(filepath.Dir(volume), 0o755); err != nil {
		t.Fatal(err)
	}
	b.lugar = LugarDe(volume)
	b.opcoes.Programa = volume
	b.opcoes.PeloLancador = true
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a troca no volume falhou: %v", err)
	}
	return b
}

func (b *bancada) escolher(versaoDaImagem string) (string, string) {
	return EscolherDoVolume(b.lugar.Atual, versaoDaImagem, []ed25519.PublicKey{b.pub}, NomeNoRelease())
}

// O lançador executa o programa do volume quando ele é válido e mais novo.
func TestLancadorEscolheOProgramaValidoDoVolume(t *testing.T) {
	b := noVolume(t)
	escolhido, porque := b.escolher("0.4.31")
	if escolhido != b.lugar.Atual {
		t.Fatalf("não escolheu o programa do volume: %s", porque)
	}
	if e := b.estado(); !e.PeloLancador || e.Arranques[chaveDoLancador] != 1 {
		t.Fatalf("registro = %+v", e)
	}
}

// Programa do volume adulterado depois da troca (um byte a mais): o lançador
// o ignora, segue com o da imagem e o abandona.
func TestLancadorIgnoraProgramaAdulterado(t *testing.T) {
	b := noVolume(t)
	f, err := os.OpenFile(b.lugar.Atual, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("# intruso\n")
	_ = f.Close()

	escolhido, porque := b.escolher("0.4.31")
	if escolhido != "" {
		t.Fatal("o lançador escolheu um programa adulterado")
	}
	if !strings.Contains(porque, "não confere") {
		t.Fatalf("motivo = %q", porque)
	}
	if e := b.estado(); e.Situacao != SituacaoAbandonada || e.Motivo != MotivoInvalido {
		t.Fatalf("registro = %+v", e)
	}
	// Abandonado, não é conferido de novo nem volta sozinho.
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador voltou a um programa abandonado")
	}
	if estado, _, motivo := Relato(b.lugar.Atual, "0.4.31"); estado != "rolled_back" || motivo != MotivoInvalido {
		t.Fatalf("relato = %q %q", estado, motivo)
	}
}

// Programa e registro trocados juntos por quem não tem a chave de release (o
// SHA-256 do registro bate com o arquivo): a assinatura guardada não cobre.
func TestLancadorIgnoraProgramaComRegistroForjado(t *testing.T) {
	b := noVolume(t)
	intruso := programaFalso("9.9.9", 0)
	if err := os.WriteFile(b.lugar.Atual, intruso, 0o755); err != nil {
		t.Fatal(err)
	}
	e := b.estado()
	e.Sha256 = resumoDe(intruso)
	if err := b.lugar.gravarEstado(e); err != nil {
		t.Fatal(err)
	}
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador escolheu um programa que a assinatura não cobre")
	}

	// E com o checksums.txt reescrito para o intruso: a assinatura não confere.
	b = noVolume(t)
	if err := os.WriteFile(b.lugar.Atual, intruso, 0o755); err != nil {
		t.Fatal(err)
	}
	e = b.estado()
	e.Sha256 = resumoDe(intruso)
	_ = b.lugar.gravarEstado(e)
	_ = os.WriteFile(b.lugar.Checksums, []byte(resumoDe(intruso)+"  "+NomeNoRelease()+"\n"), 0o644)
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador escolheu um programa com checksums.txt reescrito")
	}

	// Registro que diz ser de versão maior do que a assinada: a assinatura é
	// da 0.5.0 e não cobre.
	b = noVolume(t)
	e = b.estado()
	e.Versao = "0.9.0"
	_ = b.lugar.gravarEstado(e)
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador aceitou um registro com a versão trocada")
	}
}

// Chave que o programa da imagem não conhece: o programa do volume não vale.
func TestLancadorIgnoraAssinaturaDeChaveDesconhecida(t *testing.T) {
	b := noVolume(t)
	outra, _ := chavesDeTesteGeradas(t)
	if escolhido, _ := EscolherDoVolume(b.lugar.Atual, "0.4.31", []ed25519.PublicKey{outra}, NomeNoRelease()); escolhido != "" {
		t.Fatal("o lançador aceitou assinatura de chave desconhecida")
	}
}

// Quem puxou uma imagem mais nova (ou igual) quer a imagem: o programa do
// volume fica de lado, sem ser abandonado.
func TestLancadorPrefereImagemMaisNova(t *testing.T) {
	for _, imagem := range []string{"0.5.0", "0.6.0"} {
		b := noVolume(t)
		if escolhido, _ := b.escolher(imagem); escolhido != "" {
			t.Fatalf("imagem %s: escolheu o programa do volume (0.5.0)", imagem)
		}
		if e := b.estado(); e.Situacao != SituacaoEmProva {
			t.Fatalf("imagem %s: registro = %+v", imagem, e)
		}
	}
	b := noVolume(t)
	if escolhido, _ := b.escolher("dev"); escolhido != "" {
		t.Fatal("imagem de desenvolvimento lançou o programa do volume")
	}
}

// A prova no Docker: sem heartbeat aceito, o programa do volume é abandonado
// e o lançador volta ao da imagem.
func TestLancadorAbandonaQuemNaoProva(t *testing.T) {
	b := noVolume(t)
	if escolhido, _ := b.escolher("0.4.31"); escolhido == "" {
		t.Fatal("não lançou")
	}
	if !AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("a prova vencida não mandou voltar ao lançador")
	}
	if e := b.estado(); e.Situacao != SituacaoAbandonada || e.Motivo != MotivoSemHeartbeat {
		t.Fatalf("registro = %+v", e)
	}
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatal("o lançador voltou ao programa que não provou")
	}
}

// O programa do volume que cai antes do prazo: na terceira partida o lançador
// deixa de usá-lo.
func TestLancadorAbandonaNaTerceiraPartida(t *testing.T) {
	b := noVolume(t)
	for i := 1; i < MaximoDeArranques; i++ {
		if escolhido, porque := b.escolher("0.4.31"); escolhido == "" {
			t.Fatalf("partida %d: não lançou (%s)", i, porque)
		}
	}
	if escolhido, _ := b.escolher("0.4.31"); escolhido != "" {
		t.Fatalf("lançou pela %dª vez sem heartbeat aceito", MaximoDeArranques)
	}
	if e := b.estado(); e.Situacao != SituacaoAbandonada || e.Motivo != MotivoArranques {
		t.Fatalf("registro = %+v", e)
	}
}

// Confirmado, o programa do volume é lançado sempre, sem gastar partidas.
func TestLancadorSegueComOConfirmado(t *testing.T) {
	b := noVolume(t)
	AbrirProva(b.lugar.Atual, "0.5.0").Confirmar()
	for i := 0; i < MaximoDeArranques+2; i++ {
		if escolhido, porque := b.escolher("0.4.31"); escolhido == "" {
			t.Fatalf("partida %d: não lançou o confirmado (%s)", i, porque)
		}
	}
}

// Volume sem programa baixado, ou com um programa posto à mão (sem registro
// do lançador): vale o da imagem.
func TestLancadorSemRegistroNaoLanca(t *testing.T) {
	dir := t.TempDir()
	programa := filepath.Join(dir, "arkame-agent")
	pub, _ := chavesDeTesteGeradas(t)
	if escolhido, _ := EscolherDoVolume(programa, "0.4.31", []ed25519.PublicKey{pub}, NomeNoRelease()); escolhido != "" {
		t.Fatal("lançou de um volume vazio")
	}
	_ = os.WriteFile(programa, programaFalso("9.9.9", 0), 0o755)
	if escolhido, _ := EscolherDoVolume(programa, "0.4.31", []ed25519.PublicKey{pub}, NomeNoRelease()); escolhido != "" {
		t.Fatal("lançou um programa posto à mão no volume")
	}
}

func TestAmbienteDoLancador(t *testing.T) {
	amb := AmbienteDoLancado([]string{"A=1", EnvLancador + "=/velho", "B=2"}, "/usr/local/bin/arkame-agent", "0.4.31")
	if strings.Join(amb, " ") != "A=1 B=2 "+EnvLancador+"=/usr/local/bin/arkame-agent "+EnvVersaoDoLancador+"=0.4.31" {
		t.Fatalf("ambiente do lançado = %v", amb)
	}
	if got := strings.Join(AmbienteSemLancador(amb), " "); got != "A=1 B=2" {
		t.Fatalf("ambiente de volta = %q", got)
	}
	for _, v := range []string{"off", "OFF", " false ", "0", "no"} {
		if !Desligada(v) {
			t.Errorf("%q não desliga", v)
		}
	}
	for _, v := range []string{"", "on", "true", "1"} {
		if Desligada(v) {
			t.Errorf("%q desliga", v)
		}
	}
	if p := ProgramaDoVolume("/etc/arkame/agent.env"); p != "/etc/arkame/bin/arkame-agent" {
		t.Fatalf("programa do volume = %q", p)
	}
}
