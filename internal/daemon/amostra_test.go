package daemon

import (
	"testing"
)

// O cenário do achado A1 da revisão, pelo caminho inteiro do daemon: 20
// arquivos guardados, todas as versões apagadas no bucket por fora. O backup
// seguinte fechava `complete`, em delta, com zero entradas e só a consulta do
// versionamento — o painel com 20 arquivos presentes e o bucket vazio.
func TestBucketEsvaziadoNaoFechaCompletoSemOsArquivos(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(20)
	m.rodarOK()
	for _, k := range m.s3.Chaves() {
		for _, v := range m.s3.Versoes(k) {
			m.s3.ApagarVersao(k, v.ID)
		}
	}
	if m.s3.TotalDeVersoes() != 0 {
		t.Fatal("o bucket devia estar vazio")
	}
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Status != "complete" || s.Entradas != 20 || s.Presentes != 20 || m.s3.N("PutObject") != 20 {
		t.Fatalf("os 20 arquivos deviam subir de novo e ir ao índice: %+v s3=%v", s, m.s3.Requisicoes())
	}
	if m.s3.TotalDeVersoes() != 20 {
		t.Fatalf("bucket com %d versões", m.s3.TotalDeVersoes())
	}
	m.conferir()
}
