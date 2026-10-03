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

// deleted_file: o arquivo saiu da origem e o cliente desligou "manter a última
// versão de arquivos apagados". O painel manda a versão atual para apagar, e
// o agent obedece como no hard_delete.
func TestArquivoApagadoApagaVersaoAtual(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/removido.txt"
	f.versoes[key] = []string{"v1", "v2"} // v2 é a atual

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{{Key: key, VersionID: "v2", Reason: "deleted_file"}})

	if len(failed) != 0 || len(deleted) != 1 || f.existe(key, "v2") {
		t.Fatalf("deleted_file deveria apagar a atual: deleted=%+v failed=%+v", deleted, failed)
	}
	if !f.existe(key, "v1") {
		t.Fatal("a versão fora do plano sumiu")
	}
}

// deleted_file continua passando pelas travas de VersionId e prefixo.
func TestArquivoApagadoExigeVersaoEPrefixo(t *testing.T) {
	f, c := novoS3Falso(t)
	f.versoes["arkame/data/a.txt"] = []string{"v1"}
	f.versoes["outro/b.txt"] = []string{"v1"}

	deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
		[]Version{
			{Key: "arkame/data/a.txt", VersionID: "", Reason: "deleted_file"},
			{Key: "outro/b.txt", VersionID: "v1", Reason: "deleted_file"},
		})

	if len(deleted) != 0 || len(failed) != 2 {
		t.Fatalf("esperava as duas recusadas: deleted=%+v failed=%+v", deleted, failed)
	}
	if !f.existe("arkame/data/a.txt", "v1") || !f.existe("outro/b.txt", "v1") {
		t.Fatal("um item recusado mexeu no bucket")
	}
}

// Motivo que o agent não conhece é recusado: um painel mais novo não pode
// apagar a versão atual por uma regra que este agent nunca viu.
func TestMotivoDesconhecidoRecusado(t *testing.T) {
	f, c := novoS3Falso(t)
	key := "arkame/data/agente/c.txt"
	f.versoes[key] = []string{"v1"}

	for _, motivo := range []string{"", "purge_all", "Thinning"} {
		deleted, failed := Run(context.Background(), Options{S3: c, Bucket: "b", PrefixRoot: "arkame/"},
			[]Version{{Key: key, VersionID: "v1", Reason: motivo}})
		if len(deleted) != 0 || !f.existe(key, "v1") {
			t.Fatalf("motivo %q apagou a versão atual", motivo)
		}
		if len(failed) != 1 || !strings.Contains(failed[0].Error, "motivo desconhecido") {
			t.Fatalf("motivo %q: esperava recusa clara, veio %+v", motivo, failed)
		}
	}
}
