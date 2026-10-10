package daemon

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/bancada"
)

// picoDeRSS lê o pico de memória residente deste processo (VmHWM), em MiB.
// Zero onde não há /proc.
func picoDeRSS() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmHWM:") {
			f := strings.Fields(l)
			kb, _ := strconv.ParseFloat(f[1], 64)
			return kb / 1024
		}
	}
	return 0
}

// zerarPicoDeRSS faz o VmHWM recomeçar do RSS de agora (Linux ≥ 4.0).
func zerarPicoDeRSS() bool {
	debug.FreeOSMemory()
	return os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) == nil
}

// Escala: muitos arquivos pequenos, do começo ao fim pelo caminho do daemon
// (estado local, índice em lotes, delta), com o bucket e o painel falsos sem
// memória própria — o que este processo ocupa é o agente.
//
// Por padrão roda com 20 mil arquivos, para caber em qualquer máquina e na
// CI. ARKAME_TESTE_ESCALA=300000 (ou 1000000, numa máquina com disco e
// memória para a árvore) roda o cenário de verdade; o teto de RSS
// (ARKAME_TESTE_RSS_MB, padrão 300) vale para qualquer tamanho.
//
// O que ele prova: sem mudança, o bucket não recebe requisição e o índice não
// leva entrada; com 0,1% alterado, sobem só os alterados e nenhum POST passa
// de 2 MiB; e a memória do agente não cresce com o número de arquivos.
func TestEscalaMuitosArquivosPequenos(t *testing.T) {
	if testing.Short() {
		t.Skip("teste de escala")
	}
	n := 20000
	if v, err := strconv.Atoi(os.Getenv("ARKAME_TESTE_ESCALA")); err == nil && v > 0 {
		n = v
	}
	tetoMB := 300.0
	if v, err := strconv.ParseFloat(os.Getenv("ARKAME_TESTE_RSS_MB"), 64); err == nil && v > 0 {
		tetoMB = v
	}
	m := novaMesaDoDaemon(t)
	m.s3.SemMemoria = true
	m.painel.SemCatalogo = true
	arvore := bancada.Arvore{Raiz: m.raiz, Arquivos: n, Semente: 7, Mediana: 48, Sigma: 0.5, Teto: 256}
	if _, err := arvore.Gerar(); err != nil {
		t.Fatalf("gerando %d arquivos (a máquina comporta? ver ARKAME_TESTE_ESCALA): %v", n, err)
	}

	medir := func(nome string) (*bancada.SessaoFalsa, time.Duration, float64) {
		t.Helper()
		m.s3.Zerar()
		m.painel.Zerar()
		runtime.GC()
		medeRSS := zerarPicoDeRSS()
		inicio := time.Now()
		s := m.rodarOK()
		d := time.Since(inicio)
		rss := 0.0
		if medeRSS {
			rss = picoDeRSS()
		}
		maiorPOST := int64(0)
		for _, r := range m.painel.Rotas() {
			maiorPOST = max(maiorPOST, r.MaiorCorpo)
		}
		t.Logf("%-12s %d arquivos: %v, pico de RSS %.0f MiB, índice %s, lotes=%d entradas=%d, maior POST=%d B, s3=%v",
			nome, n, d.Round(time.Millisecond), rss, s.Modo, s.Lotes, s.Entradas, maiorPOST, m.s3.Requisicoes())
		if maiorPOST > 2<<20 {
			t.Errorf("%s: POST de %d bytes ao painel (o teto do contrato é 2 MiB)", nome, maiorPOST)
		}
		if rss > 0 && !comDetectorDeCorrida && rss > tetoMB {
			t.Errorf("%s: pico de RSS de %.0f MiB com %d arquivos; o teto é %.0f MiB", nome, rss, n, tetoMB)
		}
		return s, d, rss
	}

	s, _, _ := medir("primeira")
	if s.Status != "complete" || s.Modo != "full" || s.Entradas != int64(n) || m.s3.N("PutObject") != int64(n) {
		t.Fatalf("primeira: %+v s3=%v", s, m.s3.Requisicoes())
	}
	if lotes := (n + 4999) / 5000; s.Lotes < lotes {
		t.Fatalf("primeira: %d lotes para %d entradas", s.Lotes, n)
	}

	s, d, _ := medir("sem mudança")
	if s.Modo != "delta" || s.Entradas != 0 || s.Lotes != 0 || s.Status != "complete" {
		t.Fatalf("sem mudança: %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)
	// 1 milhão em menos de 10 min são 0,6 ms por arquivo; aqui, com folga
	// para máquina carregada e detector de corrida.
	if porArquivo := d / time.Duration(n); porArquivo > 600*time.Microsecond && !comDetectorDeCorrida {
		t.Errorf("sem mudança: %v por arquivo (%v para %d); a meta é 0,6 ms", porArquivo, d, n)
	}

	alterados, err := bancada.Mutar(m.raiz, bancada.MutAlterar, 0.001, 11)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ = medir("0,1% alterado")
	if s.Modo != "delta" || s.Entradas != int64(alterados) || m.s3.N("PutObject") != int64(alterados) || m.s3.N("HeadObject") != int64(alterados) {
		t.Fatalf("0,1%% alterado (%d arquivos): %+v s3=%v", alterados, s, m.s3.Requisicoes())
	}
	if fmt.Sprint(s.Complete["stats"].(map[string]any)["files_changed"]) != fmt.Sprint(alterados) {
		t.Fatalf("stats: %v", s.Complete["stats"])
	}
}
