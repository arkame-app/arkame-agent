package atualizacao

import (
	"os"
	"testing"
)

// trocada devolve uma bancada com a troca para a 0.5.0 já feita (em prova).
func trocada(t *testing.T) *bancada {
	t.Helper()
	b := novaBancada(t)
	if _, err := b.aplicar(); err != nil {
		t.Fatal(err)
	}
	return b
}

// O programa novo sobe e não tem heartbeat aceito no prazo: o anterior volta
// ao lugar, o registro diz por quê e o heartbeat do anterior conta ao painel.
func TestProvaVencidaVoltaAoAnterior(t *testing.T) {
	b := trocada(t)
	p := AbrirProva(b.lugar.Atual, "0.5.0")
	if !p.Aberta() {
		t.Fatal("a troca recém-feita não abriu prova")
	}
	if estado, versao, _ := Relato(b.lugar.Atual, "0.5.0"); estado != "proving" || versao != "0.5.0" {
		t.Fatalf("relato em prova = %q %q", estado, versao)
	}
	if !p.Vencer() {
		t.Fatal("a prova vencida não mandou reiniciar")
	}
	if got := b.noLugar(); got != string(b.atual) {
		t.Fatalf("o anterior não voltou ao lugar:\n%s", got)
	}
	if _, err := os.Lstat(b.lugar.Anterior); err == nil {
		t.Fatal("o anterior continua ao lado depois de voltar ao lugar")
	}
	e := b.estado()
	if e.Situacao != SituacaoRevertida || e.Motivo != MotivoSemHeartbeat || e.Versao != "0.5.0" {
		t.Fatalf("registro = %+v", e)
	}
	// Quem sobe agora é o anterior (0.4.31), e é ele que relata.
	if estado, versao, motivo := Relato(b.lugar.Atual, "0.4.31"); estado != "rolled_back" || versao != "0.5.0" || motivo != MotivoSemHeartbeat {
		t.Fatalf("relato do anterior = %q %q %q", estado, versao, motivo)
	}
	// Vencer de novo não desfaz nada.
	if p.Vencer() {
		t.Fatal("a prova venceu duas vezes")
	}
}

// Com o heartbeat aceito, a troca fica: o prazo que vencer depois não reverte.
func TestProvaConfirmadaFica(t *testing.T) {
	b := trocada(t)
	p := AbrirProva(b.lugar.Atual, "0.5.0")
	p.Confirmar()
	if p.Aberta() {
		t.Fatal("a prova continua aberta depois de confirmada")
	}
	if p.Vencer() {
		t.Fatal("a prova confirmada venceu")
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa novo saiu do lugar:\n%s", got)
	}
	if e := b.estado(); e.Situacao != SituacaoConfirmada || len(e.Arranques) != 0 {
		t.Fatalf("registro = %+v", e)
	}
	if AbrirProva(b.lugar.Atual, "0.5.0") != nil {
		t.Fatal("a troca confirmada abriu prova de novo")
	}
	if estado, _, _ := Relato(b.lugar.Atual, "0.5.0"); estado != "" {
		t.Fatalf("relato depois de confirmada = %q", estado)
	}
}

// O programa novo cai antes de completar o prazo, toda vez: na terceira
// partida sem sucesso o anterior volta, sem esperar os cinco minutos.
func TestTerceiroArranqueSemSucessoVoltaAoAnterior(t *testing.T) {
	b := trocada(t)
	for i := 1; i < MaximoDeArranques; i++ {
		if RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/agent.env") {
			t.Fatalf("reverteu na partida %d, antes da hora", i)
		}
		if got := b.noLugar(); got != string(b.rel.programa) {
			t.Fatalf("partida %d: o programa novo saiu do lugar", i)
		}
	}
	if !RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/agent.env") {
		t.Fatalf("a partida %d sem sucesso não reverteu", MaximoDeArranques)
	}
	if got := b.noLugar(); got != string(b.atual) {
		t.Fatalf("o anterior não voltou ao lugar:\n%s", got)
	}
	if e := b.estado(); e.Situacao != SituacaoRevertida || e.Motivo != MotivoArranques {
		t.Fatalf("registro = %+v", e)
	}
	// O anterior sobe e não conta nem reverte mais nada.
	if RegistrarArranque(b.lugar.Atual, "0.4.31", "/etc/arkame/agent.env") {
		t.Fatal("o anterior, ao subir, reverteu de novo")
	}
}

// Dois agentes dividem o programa: as partidas de um não contam para o outro,
// e o heartbeat aceito de qualquer um zera a conta.
func TestArranquesSaoPorAgente(t *testing.T) {
	b := trocada(t)
	for _, cfg := range []string{"/etc/arkame/a.env", "/etc/arkame/b.env", "/etc/arkame/a.env", "/etc/arkame/b.env"} {
		if RegistrarArranque(b.lugar.Atual, "0.5.0", cfg) {
			t.Fatalf("reverteu com duas partidas de cada agente (%s)", cfg)
		}
	}
	AbrirProva(b.lugar.Atual, "0.5.0").Confirmar()
	for i := 0; i < 5; i++ {
		if RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/a.env") {
			t.Fatal("reverteu uma troca já confirmada")
		}
	}
}

// Quem sobe com uma prova aberta de outra versão é o programa antigo: a troca
// não chegou ao fim. A prova é encerrada e o painel fica sabendo.
func TestProvaDeOutraVersaoEncerra(t *testing.T) {
	b := trocada(t)
	if RegistrarArranque(b.lugar.Atual, "0.4.31", "/etc/arkame/agent.env") {
		t.Fatal("reverteu sem ser o programa em prova")
	}
	if e := b.estado(); e.Situacao != SituacaoRevertida || e.Motivo != MotivoTrocaIncomplet {
		t.Fatalf("registro = %+v", e)
	}
	if AbrirProva(b.lugar.Atual, "0.4.31") != nil {
		t.Fatal("o programa antigo abriu a prova do novo")
	}
}

// Sem o anterior ao lado não há para onde voltar: o programa novo segue.
func TestSemAnteriorONovoSegue(t *testing.T) {
	b := trocada(t)
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	if AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("mandou reiniciar sem ter para onde voltar")
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa novo saiu do lugar sem haver anterior:\n%s", got)
	}
}

// Sem registro, ou com a versão em dia, não há prova nem contagem.
func TestSemTrocaNaoHaProva(t *testing.T) {
	b := novaBancada(t)
	if AbrirProva(b.lugar.Atual, "0.4.31") != nil {
		t.Fatal("abriu prova sem troca")
	}
	if RegistrarArranque(b.lugar.Atual, "0.4.31", "x") {
		t.Fatal("reverteu sem troca")
	}
	var p *Prova
	p.Confirmar()
	if p.Vencer() || p.Aberta() {
		t.Fatal("a prova nula fez alguma coisa")
	}
}
