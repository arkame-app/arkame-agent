package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	gosync "sync"
	"testing"

	"github.com/arkame-app/agent/internal/bancada"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// esvaziar apaga todas as versões do bucket, por fora do agente.
func esvaziar(f *bancada.S3Falso) {
	for _, k := range f.Chaves() {
		for _, v := range f.Versoes(k) {
			f.ApagarVersao(k, v.ID)
		}
	}
}

// O achado A1 da revisão: todas as versões apagadas no bucket por fora. O
// atalho afirmava as versões do estado sem consultar o bucket, e o backup
// seguinte fechava completo sem enviar nada. A amostra do começo da execução
// percebe, e a execução inteira lê, confere e reenvia.
func TestBucketEsvaziadoPorForaEPegoPelaAmostra(t *testing.T) {
	m := novaMesa(t)
	m.semear(20)
	m.rodar()
	m.conferir()
	esvaziar(m.s3)
	if m.s3.TotalDeVersoes() != 0 {
		t.Fatal("o bucket devia estar vazio")
	}
	m.s3.Zerar()
	r, em := m.rodar()
	if r.AmostraFalhou == "" || r.Atalhos != 0 {
		t.Fatalf("a amostra devia ter derrubado o atalho: falha=%q atalhos=%d", r.AmostraFalhou, r.Atalhos)
	}
	if m.s3.N("PutObject") != 20 || r.Stats.FilesUploaded != 20 || len(em) != 20 {
		t.Fatalf("os 20 arquivos deviam subir de novo e ir ao índice: enviados=%d emitidas=%d s3=%v", r.Stats.FilesUploaded, len(em), m.s3.Requisicoes())
	}
	m.conferir()
	// Com o bucket de novo em ordem, a execução seguinte volta ao atalho.
	m.s3.Zerar()
	r, _ = m.rodar()
	if r.AmostraFalhou != "" || r.Atalhos != 20 || len(alemDaAmostra(t, m.s3)) != 0 {
		t.Fatalf("seguinte: falha=%q atalhos=%d s3=%v", r.AmostraFalhou, r.Atalhos, m.s3.Requisicoes())
	}
}

// Bucket recriado com o mesmo nome (ou armazenamento reapontado para outro
// endereço): as versões do estado não existem lá.
func TestBucketTrocadoEPegoPelaAmostra(t *testing.T) {
	m := novaMesa(t)
	m.semear(50)
	m.rodar()
	outro := bancada.NovoS3Falso()
	srv := httptest.NewServer(outro)
	t.Cleanup(srv.Close)
	m.s3, m.cli = outro, bancada.ClienteS3(srv.URL)
	r, _ := m.rodar()
	if r.AmostraFalhou == "" || outro.N("PutObject") != 50 {
		t.Fatalf("no bucket novo tudo sobe de novo: falha=%q s3=%v", r.AmostraFalhou, outro.Requisicoes())
	}
	// Uma falha basta: a amostra para de consultar.
	if n := outro.N("HeadObjectVersion"); n < 1 || n > 8 {
		t.Fatalf("a amostra devia parar na primeira falha (até uma por trabalhador): %d", n)
	}
	m.conferir()
}

// Bucket recriado em que os ids de versão coincidem (provedor que os numera
// em sequência), mas o conteúdo é outro: a amostra confere o sha256 e o
// tamanho, não só a existência da versão.
func TestAmostraConfereOConteudoDaVersao(t *testing.T) {
	m := novaMesa(t)
	m.escrever("um.txt", "conteúdo original")
	m.rodar()
	k := m.chave("um.txt")
	id := m.presenca[k].VersionID

	outro := bancada.NovoS3Falso()
	srv := httptest.NewServer(outro)
	t.Cleanup(srv.Close)
	cli := bancada.ClienteS3(srv.URL)
	b := "b"
	if _, err := cli.PutObject(context.Background(), &s3.PutObjectInput{Bucket: &b, Key: &k,
		Body: strings.NewReader("outra coisa"), Metadata: map[string]string{"sha256": sha256Hex([]byte("outra coisa"))}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := outro.Atual(k); v.ID != id {
		t.Fatalf("o teste precisa do mesmo id de versão nos dois buckets: %q e %q", v.ID, id)
	}
	m.s3, m.cli = outro, cli
	r, _ := m.rodar()
	if !strings.Contains(r.AmostraFalhou, "sha256") || r.Stats.FilesUploaded != 1 {
		t.Fatalf("falha=%q enviados=%d", r.AmostraFalhou, r.Stats.FilesUploaded)
	}
	m.conferir()
}

// Perda parcial (regra de ciclo de vida, alguém que apagou uma pasta do
// bucket): se a amostra cai numa versão que sumiu, a execução INTEIRA
// reconfere — não só o arquivo amostrado.
func TestPerdaParcialFazAExecucaoInteiraReconferir(t *testing.T) {
	m := novaMesa(t)
	m.semear(120)
	m.rodar()
	// A amostra vira as AmostraPadrao primeiras chaves, em ordem: o sorteio
	// nunca troca uma entrada do reservatório.
	m.sortear = func(n int) int { return n - 1 }
	chaves := m.s3.Chaves()
	sort.Strings(chaves)
	naAmostra, fora := chaves[3], chaves[100]
	for _, k := range []string{naAmostra, fora} {
		if !m.s3.ApagarVersao(k, m.presenca[k].VersionID) {
			t.Fatal("não achei a versão para apagar")
		}
	}
	m.s3.Zerar()
	r, em := m.rodar()
	if r.AmostraFalhou == "" || r.Atalhos != 0 || m.s3.N("HeadObject") != 120 {
		t.Fatalf("todos os 120 deviam ser conferidos no bucket: falha=%q atalhos=%d s3=%v", r.AmostraFalhou, r.Atalhos, m.s3.Requisicoes())
	}
	// Os dois que sumiram sobem — também o que NÃO estava na amostra.
	if m.s3.N("PutObject") != 2 || len(em) != 2 {
		t.Fatalf("enviados=%v emitidas=%d", m.s3.Requisicoes(), len(em))
	}
	m.conferir()
}

// O custo da amostra é fixo: AmostraPadrao consultas por execução, qualquer
// que seja o tamanho do plano (e uma por entrada quando o plano é menor que
// ela). Sem estado, ou com a releitura total pedida, não há amostra.
func TestAmostraTemTamanhoFixo(t *testing.T) {
	for _, n := range []int{10, 700} {
		m := novaMesa(t)
		m.semear(n)
		m.rodar()
		if m.s3.N("HeadObjectVersion") != 0 {
			t.Fatalf("%d arquivos, primeira execução: sem estado confirmado não há o que amostrar: %v", n, m.s3.Requisicoes())
		}
		m.s3.Zerar()
		r, _ := m.rodar()
		quero := min(n, AmostraPadrao)
		if int(m.s3.N("HeadObjectVersion")) != quero || r.Amostrados != quero || len(alemDaAmostra(t, m.s3)) != 0 {
			t.Fatalf("%d arquivos: queria %d consultas da amostra e mais nada: amostrados=%d s3=%v", n, quero, r.Amostrados, m.s3.Requisicoes())
		}
		m.rehash = true
		m.s3.Zerar()
		m.rodar()
		if m.s3.N("HeadObjectVersion") != 0 {
			t.Fatalf("com a releitura total a amostra não acrescenta nada: %v", m.s3.Requisicoes())
		}
	}
}

// A amostra não se repete: de uma execução para outra o sorteio muda, e a
// perda parcial que escapou de uma é pega em outra.
func TestAmostraESorteadaACadaExecucao(t *testing.T) {
	m := novaMesa(t)
	m.semear(400)
	m.rodar()
	var mu gosync.Mutex
	rodadas := []map[string]bool{{}, {}}
	for i := range rodadas {
		m.s3.Antes(func(op string, r *http.Request) {
			if op == "HeadObjectVersion" {
				mu.Lock()
				rodadas[i][r.URL.Path] = true
				mu.Unlock()
			}
		})
		m.rodar()
	}
	iguais := 0
	for k := range rodadas[0] {
		if rodadas[1][k] {
			iguais++
		}
	}
	if len(rodadas[0]) != AmostraPadrao || len(rodadas[1]) != AmostraPadrao || iguais == AmostraPadrao {
		t.Fatalf("duas execuções com a mesma amostra: %d e %d chaves, %d em comum", len(rodadas[0]), len(rodadas[1]), iguais)
	}
}
