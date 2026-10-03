package purge

import (
	"context"
	"strings"
	"testing"
)

// O plano do painel chegou a listar, como desbaste, a versão atual de
// arquivos que não mudavam. O agent é o último ponto antes do dado sumir:
// ele confere e recusa.
func TestDesbasteRecusaVersaoAtual(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/banco.dump"
	f.versoes[key] = []string{"v1", "v2", "v3"} // v3 é a atual

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{
			{Key: key, VersionID: "v3", Reason: "thinning"},
			{Key: key, VersionID: "v1", Reason: "thinning"},
		})

	if !f.existe(key, "v3") {
		t.Fatal("a versão atual foi apagada por um item de desbaste")
	}
	if f.existe(key, "v1") {
		t.Fatal("a versão antiga autorizada não saiu")
	}
	if len(deleted) != 1 || deleted[0].VersionID != "v1" {
		t.Fatalf("esperava só v1 apagada, veio %+v", deleted)
	}
	if len(failed) != 1 || failed[0].VersionID != "v3" || !strings.Contains(failed[0].Error, "versão atual") {
		t.Fatalf("esperava v3 recusada com motivo claro, veio %+v", failed)
	}
	if f.heads != 1 {
		t.Fatalf("esperava um HeadObject por chave, foram %d", f.heads)
	}
}

// hard_delete apaga a atual de propósito (arquivo saiu da origem e passou do
// prazo): não passa pela conferência.
func TestHardDeleteApagaVersaoAtual(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/sumiu.txt"
	f.versoes[key] = []string{"v1"}

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "v1", Reason: "hard_delete"}})

	if len(failed) != 0 || len(deleted) != 1 || f.existe(key, "v1") {
		t.Fatalf("hard_delete deveria apagar a atual: deleted=%+v failed=%+v", deleted, failed)
	}
	if f.heads != 0 {
		t.Fatalf("hard_delete não deveria consultar a versão atual (%d HEAD)", f.heads)
	}
}

// Sem conseguir conferir, recusa: apagar no escuro é o que se quer evitar.
func TestDesbasteRecusaQuandoNaoConsegueConferir(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/a.txt"
	f.versoes[key] = []string{"v1", "v2"}
	f.headErro = 403

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "v1", Reason: "thinning"}})

	if len(deleted) != 0 || !f.existe(key, "v1") {
		t.Fatal("apagou sem conseguir conferir a versão atual")
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Error, "conferir") {
		t.Fatalf("esperava recusa por não conferir, veio %+v", failed)
	}
}

// Chave sem versão atual (só antigas, ou delete marker no topo): nada a
// proteger, o desbaste segue.
func TestDesbasteChaveSemVersaoAtual(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/b.txt"
	f.headErro = 404
	f.versoes[key] = []string{"v1"}

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "v1", Reason: "thinning"}})
	if len(failed) != 0 || len(deleted) != 1 {
		t.Fatalf("deleted=%+v failed=%+v", deleted, failed)
	}
}
