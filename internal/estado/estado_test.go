package estado

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

var idDeTeste = Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "b"}

func abrir(t *testing.T, dir string) *Base {
	t.Helper()
	b, err := Abrir(dir, idDeTeste)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Fechar() })
	return b
}

func entrada(i int) Entrada {
	e := Entrada{Tamanho: int64(i), MtimeSeg: 1_700_000_000 + int64(i), MtimeNano: 7, CtimeSeg: 5, VerificarEm: 20000, Versao: fmt.Sprintf("v%d", i), NaBase: true}
	e.SHA256[0] = byte(i)
	return e
}

// A entrada só vale depois de a geração que a gravou ser confirmada: é o que
// faz um `kill -9` no meio não deixar afirmação que o painel não aceitou.
func TestEntradaSoValeDepoisDeConfirmada(t *testing.T) {
	dir := t.TempDir()
	b := abrir(t, dir)
	p, err := b.Plano("p1", "data/ag1/")
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := p.Comecar("s1")
	if err := ex.Gravar([]Gravacao{{Chave: "a.txt", Ent: entrada(1)}}); err != nil {
		t.Fatal(err)
	}
	// Morreu aqui: sem Confirmar.
	_ = b.Fechar()

	b = abrir(t, dir)
	if b.Motivo != "" {
		t.Fatalf("o arquivo existia e vale; motivo = %q", b.Motivo)
	}
	p, _ = b.Plano("p1", "data/ag1/")
	if g, s := p.Pendente(); g != 1 || s != "s1" {
		t.Fatalf("pendente = %d %q", g, s)
	}
	ex, _ = p.Comecar("s2")
	ent, achou, err := ex.Buscar("a.txt")
	if err != nil || !achou {
		t.Fatalf("achou=%v err=%v", achou, err)
	}
	if ex.Vale(ent) {
		t.Fatal("entrada de geração não confirmada não pode valer")
	}
	if ent.Versao != "v1" || ent.Tamanho != 1 || ent.MtimeNano != 7 || !ent.NaBase || ent.SHA256[0] != 1 {
		t.Fatalf("entrada voltou diferente: %+v", ent)
	}
	ent.Versao = "v2"
	if err := ex.Gravar([]Gravacao{{Chave: "a.txt", Ent: ent}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Confirmar("s2", 1, ex.Ger); err != nil {
		t.Fatal(err)
	}
	ex3, _ := p.Comecar("s3")
	ent, _, _ = ex3.Buscar("a.txt")
	if !ex3.Vale(ent) || ent.Versao != "v2" || ent.ID != 1 {
		t.Fatalf("depois de confirmada tem de valer, com o mesmo ID: %+v", ent)
	}
	if p.UltimaSessao() != "s2" || p.Arquivos() != 1 {
		t.Fatalf("última = %q arquivos = %d", p.UltimaSessao(), p.Arquivos())
	}
	// Geração adotada vale dentro da execução que a adotou.
	ex4, _ := p.Comecar("s4", ex3.Ger)
	_ = ex3.Gravar([]Gravacao{{Chave: "b.txt", Ent: entrada(2)}})
	ent, _, _ = ex4.Buscar("b.txt")
	if !ex4.Vale(ent) {
		t.Fatal("geração adotada tem de valer")
	}
}

// Arquivo cortado, adulterado ou de outro armazenamento: começa vazio, com o
// motivo, em vez de afirmar o que não sabe.
func TestEstadoInvalidoRecomecaVazio(t *testing.T) {
	semear := func(t *testing.T, dir string) string {
		b := abrir(t, dir)
		p, _ := b.Plano("p1", "data/ag1/")
		ex, _ := p.Comecar("s1")
		var itens []Gravacao
		for i := 0; i < 5000; i++ {
			itens = append(itens, Gravacao{Chave: fmt.Sprintf("pasta/arquivo-%05d.txt", i), Ent: entrada(i)})
		}
		if err := ex.Gravar(itens); err != nil {
			t.Fatal(err)
		}
		_ = p.Confirmar("s1", 5000, ex.Ger)
		_ = b.Fechar()
		return NomeDoArquivo(dir, idDeTeste)
	}

	t.Run("cortado", func(t *testing.T) {
		dir := t.TempDir()
		arq := semear(t, dir)
		st, _ := os.Stat(arq)
		if err := os.Truncate(arq, st.Size()/8); err != nil {
			t.Fatal(err)
		}
		b := abrir(t, dir)
		if b.Motivo != "corrompido" {
			t.Fatalf("motivo = %q", b.Motivo)
		}
		p, _ := b.Plano("p1", "data/ag1/")
		if !p.Vazio() {
			t.Fatal("estado cortado tem de recomeçar vazio")
		}
	})

	t.Run("outro armazenamento", func(t *testing.T) {
		dir := t.TempDir()
		arq := semear(t, dir)
		outro := Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "outro-bucket"}
		b, err := Abrir(dir, outro)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Fechar()
		if b.Motivo != "outro_armazenamento" || b.Caminho() != arq {
			t.Fatalf("motivo = %q", b.Motivo)
		}
	})

	t.Run("adulterado", func(t *testing.T) {
		dir := t.TempDir()
		arq := semear(t, dir)
		bruto, _ := os.ReadFile(arq)
		// Estraga o miolo das páginas de dados, sem tocar nas duas páginas
		// de meta do começo (o bbolt abriria assim mesmo).
		for i := len(bruto) / 3; i < len(bruto)*2/3; i += 97 {
			bruto[i] ^= 0x5a
		}
		if err := os.WriteFile(arq, bruto, 0o600); err != nil {
			t.Fatal(err)
		}
		b, err := Abrir(dir, idDeTeste)
		if err != nil {
			t.Fatal(err)
		}
		falhou := b.Motivo == "corrompido"
		if !falhou {
			p, perr := b.Plano("p1", "data/ag1/")
			if perr != nil {
				falhou = true
			} else {
				ex, cerr := p.Comecar("s2")
				falhou = cerr != nil
				for i := 0; i < 5000 && !falhou; i++ {
					_, _, err := ex.Buscar(fmt.Sprintf("pasta/arquivo-%05d.txt", i))
					if err != nil {
						if !errors.Is(err, ErrCorrompido) {
							t.Fatalf("erro que não é de corrupção: %v", err)
						}
						falhou = true
					}
				}
			}
		}
		if !falhou {
			t.Fatal("nenhuma leitura acusou o arquivo adulterado")
		}
		if b.Motivo != "corrompido" && !b.Corrompida() {
			t.Fatal("a base tinha de ficar marcada como corrompida")
		}
		_ = b.Fechar()
		if b.Corrompida() {
			if _, err := os.Stat(arq); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("estado corrompido tem de ser apagado ao fechar")
			}
		}
	})
}

// Prefixo de chave trocado: as entradas eram de outro lugar do bucket.
func TestPrefixoTrocadoReiniciaOPlano(t *testing.T) {
	b := abrir(t, t.TempDir())
	p, _ := b.Plano("p1", "A/data/ag1/")
	ex, _ := p.Comecar("s1")
	_ = ex.Gravar([]Gravacao{{Chave: "a", Ent: entrada(1)}})
	_ = p.Confirmar("s1", 1, ex.Ger)
	p, _ = b.Plano("p1", "B/data/ag1/")
	if !p.Reiniciado || !p.Vazio() || p.UltimaSessao() != "" {
		t.Fatalf("plano com outro prefixo tinha de recomeçar: %+v", p.meta)
	}
}

// A varredura final vê quem não foi visto, e as mudanças valem.
func TestVarrerEEsquecer(t *testing.T) {
	b := abrir(t, t.TempDir())
	p, _ := b.Plano("p1", "data/ag1/")
	ex, _ := p.Comecar("s1")
	var itens []Gravacao
	for i := 1; i <= 10000; i++ {
		itens = append(itens, Gravacao{Chave: fmt.Sprintf("f%05d", i), Ent: entrada(i)})
	}
	_ = ex.Gravar(itens)
	_ = p.Confirmar("s1", 10000, ex.Ger)

	ex, _ = p.Comecar("s2")
	for i := 1; i <= 10000; i++ {
		if i%100 != 0 {
			ex.MarcarVisto(uint64(i))
		}
	}
	naoVistos := 0
	err := ex.Varrer(func(_ string, ent Entrada, visto bool) (Acao, Entrada) {
		if visto {
			return Manter, ent
		}
		naoVistos++
		ent.Removida = true
		return Regravar, ent
	})
	if err != nil || naoVistos != 100 {
		t.Fatalf("não vistos = %d err = %v", naoVistos, err)
	}
	ent, _, _ := ex.Buscar("f00100")
	if !ent.Removida || ent.Ger != ex.Ger {
		t.Fatalf("lápide não gravada: %+v", ent)
	}
	apagadas := 0
	_ = ex.Varrer(func(_ string, ent Entrada, _ bool) (Acao, Entrada) {
		if ent.Removida {
			apagadas++
			return Apagar, ent
		}
		return Manter, ent
	})
	if _, achou, _ := ex.Buscar("f00100"); achou || apagadas != 100 {
		t.Fatalf("lápides apagadas = %d", apagadas)
	}

	n, err := b.Esquecer([]VersaoApagada{
		{Chave: "data/ag1/f00001", Versao: "v1"},     // a versão registrada: sai
		{Chave: "data/ag1/f00002", Versao: "antiga"}, // versão antiga: a entrada fica
		{Chave: "outro/f00003", Versao: "v3"},        // fora do prefixo
	})
	if err != nil || n != 1 {
		t.Fatalf("esquecidas = %d err = %v", n, err)
	}
	if _, achou, _ := ex.Buscar("f00001"); achou {
		t.Fatal("entrada da versão expurgada tinha de sair")
	}
	if _, achou, _ := ex.Buscar("f00002"); !achou {
		t.Fatal("entrada de outra versão tinha de ficar")
	}
}
