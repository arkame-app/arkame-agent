package daemon

import (
	"testing"
)

// O armazenamento reapontado para outro endereço, com o mesmo STORAGE_ID e o
// mesmo nome de bucket: o estado é de outro lugar. O backup confere todos os
// arquivos no bucket (e não só a amostra) e manda o inventário inteiro.
func TestEnderecoDoArmazenamentoTrocadoDescartaOEstado(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.semear(60)
	m.cfg.StorageEndpoint = "https://s3.provedor-a.example/"
	m.rodarOK()
	m.s3.Zerar()
	if s := m.rodarOK(); s.Modo != "delta" || s.Entradas != 0 {
		t.Fatalf("mesmo endereço (a barra do fim não conta): %+v", s)
	}
	semS3AlemDoVersionamento(t, m.s3)

	m.cfg.StorageEndpoint = "https://s3.provedor-b.example"
	m.s3.Zerar()
	s := m.rodarOK()
	if s.Modo != "full" || s.Entradas != 60 || m.s3.N("HeadObject") != 60 || m.s3.N("HeadObjectVersion") != 0 {
		t.Fatalf("endereço trocado: o estado não serve; queria full com 60 HeadObject: %+v s3=%v", s, m.s3.Requisicoes())
	}
	m.conferir()
}
