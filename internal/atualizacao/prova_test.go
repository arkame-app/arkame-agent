package atualizacao

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// Sem o anterior ao lado não há para onde voltar: o programa novo segue. A
// prova não fecha como "revertida" (seria mentira) nem fica presa em
// "em_prova": o registro e o relato dizem que ficou sem volta, e o heartbeat
// aceito que chegar depois ainda confirma a troca.
func TestProvaVencidaSemAnteriorFicaSemVoltaEAindaConfirma(t *testing.T) {
	b := trocada(t)
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	p := AbrirProva(b.lugar.Atual, "0.5.0")
	if p.Vencer() {
		t.Fatal("mandou reiniciar sem ter para onde voltar")
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa novo saiu do lugar sem haver anterior:\n%s", got)
	}
	if e := b.estado(); e.Situacao != SituacaoSemVolta || e.Motivo != MotivoSemVolta {
		t.Fatalf("registro depois de vencer sem anterior = %+v", e)
	}
	if estado, versao, motivo := Relato(b.lugar.Atual, "0.5.0"); estado != "proving" || versao != "0.5.0" || motivo != MotivoSemVolta {
		t.Fatalf("relato = %q %q %q", estado, versao, motivo)
	}
	if !p.Aberta() {
		t.Fatal("a prova sem volta fechou: o heartbeat que chegar não confirma mais nada")
	}
	// Vencer de novo, e as partidas seguintes, não mudam nada: não há volta.
	if p.Vencer() {
		t.Fatal("a prova sem volta mandou reiniciar")
	}
	for i := 0; i < MaximoDeArranques+1; i++ {
		if RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/agent.env") {
			t.Fatal("a partida de uma troca sem volta mandou reiniciar")
		}
	}
	// Outro processo (depois de um reinício) também abre a prova e confirma.
	AbrirProva(b.lugar.Atual, "0.5.0").Confirmar()
	if e := b.estado(); e.Situacao != SituacaoConfirmada || e.Motivo != "" {
		t.Fatalf("o heartbeat aceito não confirmou a troca sem volta: %+v", e)
	}
	if estado, _, _ := Relato(b.lugar.Atual, "0.5.0"); estado != "" {
		t.Fatalf("relato depois de confirmada = %q", estado)
	}
}

// O vizinho que divide o programa e ainda roda o antigo: com a troca sem
// volta, basta ele reiniciar — não baixa de novo nem fica adiando para sempre.
func TestTrocaSemVoltaContaComoNoLugarParaOVizinho(t *testing.T) {
	b := trocada(t)
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	AbrirProva(b.lugar.Atual, "0.5.0").Vencer()
	res, err := b.aplicar()
	if err != nil || !res.JaNoLugar {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A terceira partida sem anterior utilizável: sem volta, e não uma tentativa
// de reversão a cada partida dali em diante.
func TestTerceiroArranqueSemAnteriorFicaSemVolta(t *testing.T) {
	b := trocada(t)
	if err := os.Remove(b.lugar.Anterior); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaximoDeArranques; i++ {
		if RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/agent.env") {
			t.Fatal("mandou reiniciar sem ter para onde voltar")
		}
	}
	if e := b.estado(); e.Situacao != SituacaoSemVolta {
		t.Fatalf("registro = %+v", e)
	}
}

// A prova que vence com o painel fora do ar (rede, 5xx) é estendida, até o
// teto contado da troca. Quem ouviu um "não" (401/403), ou nem chegou a
// tentar um heartbeat, volta no prazo normal.
func TestProvaEstendeSoComOPainelForaDoArEAteOTeto(t *testing.T) {
	b := trocada(t)
	p := AbrirProva(b.lugar.Atual, "0.5.0")
	if !p.Estende(PainelIndisponivel, PrazoDaProva) {
		t.Fatal("painel fora do ar aos 5 minutos: a prova devia ser estendida")
	}
	if p.Estende(PainelRecusou, PrazoDaProva) {
		t.Fatal("o painel recusou (401/403) o prazo inteiro: não é para estender")
	}
	if p.Estende(PainelSemTentativa, PrazoDaProva) {
		t.Fatal("o processo nem tentou um heartbeat: não é para estender")
	}
	if p.Estende(PainelIndisponivel, TetoDaProva) {
		t.Fatal("estendeu além do teto contado do arranque")
	}
	// O teto conta da troca, e não de cada partida do processo.
	antes := agora
	t.Cleanup(func() { agora = antes })
	agora = func() time.Time { return antes().Add(TetoDaProva + time.Second) }
	if p.Estende(PainelIndisponivel, PrazoDaProva) {
		t.Fatal("estendeu além do teto contado da troca")
	}
	var nula *Prova
	if nula.Estende(PainelIndisponivel, 0) {
		t.Fatal("a prova nula estendeu")
	}
}

// O relato leva o instante do estado: a reversão é de quando aconteceu, e o
// registro devolvido por uma troca que falhou não ganha instante novo.
func TestRelatoLevaOInstanteDoEstado(t *testing.T) {
	antes := agora
	t.Cleanup(func() { agora = antes })
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	agora = func() time.Time { return t0 }
	b := trocada(t)
	if _, _, _, em := RelatoComInstante(b.lugar.Atual, "0.5.0"); !em.Equal(t0) {
		t.Fatalf("instante da prova = %v, queria %v", em, t0)
	}
	t1 := t0.Add(5 * time.Minute)
	agora = func() time.Time { return t1 }
	// A partida contada não muda a situação nem o instante dela.
	RegistrarArranque(b.lugar.Atual, "0.5.0", "/etc/arkame/agent.env")
	if _, _, _, em := RelatoComInstante(b.lugar.Atual, "0.5.0"); !em.Equal(t0) {
		t.Fatalf("contar a partida mudou o instante do estado: %v", em)
	}
	if !AbrirProva(b.lugar.Atual, "0.5.0").Vencer() {
		t.Fatal("não reverteu")
	}
	estado, _, _, em := RelatoComInstante(b.lugar.Atual, "0.4.31")
	if estado != "rolled_back" || !em.Equal(t1) {
		t.Fatalf("relato da reversão = %q em %v, queria rolled_back em %v", estado, em, t1)
	}
	// Uma troca forçada que falha no último passo devolve o registro de antes,
	// com o instante de antes.
	t2 := t1.Add(time.Hour)
	agora = func() time.Time { return t2 }
	gravar := gravarArquivo
	t.Cleanup(func() { gravarArquivo = gravar })
	renomeia := renomear
	t.Cleanup(func() { renomear = renomeia })
	trocaPorRenomeAntes, pausa := trocaPorRenome, pausaEntreRenames
	t.Cleanup(func() { trocaPorRenome, pausaEntreRenames = trocaPorRenomeAntes, pausa })
	trocaPorRenome, pausaEntreRenames = true, 0
	renomear = func(string, string) error { return errors.New("arquivo preso") }
	b.pedido.Forcar = true
	if _, err := b.aplicar(); CodigoDaFalha(err) != FalhaTroca {
		t.Fatalf("queria a troca falhando, veio %v", err)
	}
	if _, _, _, em := RelatoComInstante(b.lugar.Atual, "0.4.31"); !em.Equal(t1) {
		t.Fatalf("a troca que falhou mudou o instante da reversão antiga: %v", em)
	}
}

// No renomeia-e-renomeia (Windows), o segundo rename que falha uma vez (o
// antivírus ainda segura o arquivo) é tentado de novo antes de desfazer.
func TestSegundoRenameETentadoDeNovo(t *testing.T) {
	antesT, antesP, antesR := trocaPorRenome, pausaEntreRenames, renomear
	t.Cleanup(func() { trocaPorRenome, pausaEntreRenames, renomear = antesT, antesP, antesR })
	trocaPorRenome, pausaEntreRenames = true, 0
	falhas := 2
	renomear = func(de, para string) error {
		if falhas > 0 {
			falhas--
			return errors.New("arquivo em uso")
		}
		return os.Rename(de, para)
	}
	b := novaBancada(t)
	if _, err := b.aplicar(); err != nil {
		t.Fatalf("a troca desistiu no primeiro rename que falhou: %v", err)
	}
	if got := b.noLugar(); got != string(b.rel.programa) {
		t.Fatalf("o programa novo não entrou:\n%s", got)
	}
}

// A troca que parou entre os dois renames (queda de energia): o caminho do
// programa está vazio e o `.novo` e o `.anterior` estão ao lado. O `.novo`
// conferido entra; sem ele, volta o `.anterior` conferido; o que não confere
// com o registro não é posto no lugar.
func TestRecuperarATrocaInterrompida(t *testing.T) {
	interrompida := func(t *testing.T) *bancada {
		t.Helper()
		b := trocada(t)
		// Desfaz o segundo rename: o novo volta a ser `.novo`.
		if err := os.Rename(b.lugar.Atual, b.lugar.Novo); err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Run("o novo conferido entra", func(t *testing.T) {
		b := interrompida(t)
		if feito := Recuperar(b.lugar.Atual); feito != "novo" {
			t.Fatalf("Recuperar = %q", feito)
		}
		if got := b.noLugar(); got != string(b.rel.programa) {
			t.Fatalf("no lugar:\n%s", got)
		}
		if e := b.estado(); e.Situacao != SituacaoEmProva {
			t.Fatalf("registro = %+v", e)
		}
	})
	t.Run("sem o novo, volta o anterior conferido", func(t *testing.T) {
		b := interrompida(t)
		if err := os.Remove(b.lugar.Novo); err != nil {
			t.Fatal(err)
		}
		if feito := Recuperar(b.lugar.Atual); feito != "anterior" {
			t.Fatalf("Recuperar = %q", feito)
		}
		if got := b.noLugar(); got != string(b.atual) {
			t.Fatalf("no lugar:\n%s", got)
		}
		if e := b.estado(); e.Situacao != SituacaoRevertida || e.Motivo != MotivoTrocaIncomplet {
			t.Fatalf("registro = %+v", e)
		}
	})
	t.Run("o novo adulterado não entra; volta o anterior", func(t *testing.T) {
		b := interrompida(t)
		if err := os.WriteFile(b.lugar.Novo, []byte("#!/bin/sh\necho invasor\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if feito := Recuperar(b.lugar.Atual); feito != "anterior" {
			t.Fatalf("Recuperar = %q", feito)
		}
		if got := b.noLugar(); got != string(b.atual) {
			t.Fatalf("no lugar:\n%s", got)
		}
	})
	t.Run("nada que confira: não mexe", func(t *testing.T) {
		b := interrompida(t)
		for _, f := range []string{b.lugar.Novo, b.lugar.Anterior} {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte("#!/bin/sh\necho invasor\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if feito := Recuperar(b.lugar.Atual); feito != "" {
			t.Fatalf("Recuperar = %q", feito)
		}
		if _, err := os.Lstat(b.lugar.Atual); err == nil {
			t.Fatal("pôs no lugar um arquivo que não confere com o registro")
		}
	})
	t.Run("com o programa no lugar, não mexe", func(t *testing.T) {
		b := trocada(t)
		if feito := Recuperar(b.lugar.Atual); feito != "" {
			t.Fatalf("Recuperar = %q", feito)
		}
		if got := b.noLugar(); got != string(b.rel.programa) {
			t.Fatalf("no lugar:\n%s", got)
		}
	})
	t.Run("com outra troca em curso, não mexe", func(t *testing.T) {
		b := interrompida(t)
		destravar, ok, err := b.lugar.travar()
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		defer destravar()
		if feito := Recuperar(b.lugar.Atual); feito != "" {
			t.Fatalf("Recuperar = %q no meio da troca de outro processo", feito)
		}
	})
}

func TestProgramaDoAnterior(t *testing.T) {
	for caminho, quer := range map[string]string{
		filepath.Join("/usr/local/bin", "arkame-agent.anterior"):   filepath.Join("/usr/local/bin", "arkame-agent"),
		filepath.Join("C:", "Arkame", "arkame-agent.anterior.exe"): filepath.Join("C:", "Arkame", "arkame-agent.exe"),
		filepath.Join("/usr/local/bin", "arkame-agent"):            "",
		filepath.Join("/usr/local/bin", ".anterior"):               "",
	} {
		got, ok := ProgramaDoAnterior(caminho)
		if got != quer || ok != (quer != "") {
			t.Errorf("ProgramaDoAnterior(%q) = %q %v, queria %q", caminho, got, ok, quer)
		}
		if ok && LugarDe(got).Anterior != caminho {
			t.Errorf("LugarDe(%q).Anterior = %q, e não %q", got, LugarDe(got).Anterior, caminho)
		}
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
