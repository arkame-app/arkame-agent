package sync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
)

// A data que vai ao painel fica sempre nos anos 0001–9999 em UTC (o que o
// painel aceita: contrato, item 19) e vira JSON em qualquer fuso do servidor.
// O instante era preso à faixa, mas o texto levava o fuso do arquivo: a menos
// de um dia das pontas, o ano escrito saía 10000 — que o json.Marshal recusa,
// e a sessão inteira não fechava — ou 0000.
func TestDataParaOPainelEmQualquerFuso(t *testing.T) {
	leste := time.FixedZone("leste", 14*3600)
	oeste := time.FixedZone("oeste", -12*3600)
	for _, c := range []struct {
		nome    string
		arquivo time.Time
		indice  time.Time // o instante que tem de ir
	}{
		{"comum", time.Date(2026, 10, 12, 9, 0, 0, 0, oeste), time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)},
		{"data zero do Windows", time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"ano 12000", time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC), maiorDataDoPainel},
		{"antes do ano 1", time.Date(-300, 1, 1, 0, 0, 0, 0, time.UTC), menorDataDoPainel},
		{"ano 0", time.Date(0, 6, 1, 0, 0, 0, 0, time.UTC), menorDataDoPainel},
		// Dentro da faixa em UTC, fora dela no relógio do servidor.
		{"último dia, fuso a leste", time.Date(10000, 1, 1, 5, 0, 0, 0, leste), time.Date(9999, 12, 31, 15, 0, 0, 0, time.UTC)},
		{"primeiro dia, fuso a oeste", time.Date(0, 12, 31, 20, 0, 0, 0, oeste), time.Date(1, 1, 1, 8, 0, 0, 0, time.UTC)},
		// Fora da faixa em UTC, dentro dela no relógio do servidor.
		{"além do fim, fuso a oeste", time.Date(9999, 12, 31, 20, 0, 0, 0, oeste), maiorDataDoPainel},
		{"antes do começo, fuso a leste", time.Date(1, 1, 1, 5, 0, 0, 0, leste), menorDataDoPainel},
	} {
		d := dataParaOPainel(c.arquivo)
		if !d.Equal(c.indice) {
			t.Errorf("%s: foi %v, queria %v", c.nome, d, c.indice)
		}
		if a := d.UTC().Year(); a < 1 || a > 9999 {
			t.Errorf("%s: ano %d em UTC, fora de 0001–9999", c.nome, a)
		}
		tam := int64(1)
		b, err := json.Marshal(api.IndexEntry{Op: api.OpUpsert, Key: "k", Size: &tam, ModifiedAt: &d})
		if err != nil {
			t.Errorf("%s: a entrada não vira JSON: %v", c.nome, err)
			continue
		}
		// O ano escrito, como o painel o lê (sem sinal, quatro dígitos, de 0001 a 9999).
		var lida struct {
			ModifiedAt string `json:"modified_at"`
		}
		_ = json.Unmarshal(b, &lida)
		if ano := lida.ModifiedAt[:4]; ano < "0001" || strings.ContainsAny(ano, "+-") {
			t.Errorf("%s: o texto leva o ano %q: %s", c.nome, ano, lida.ModifiedAt)
		}
	}
}

// Arquivo cujo caminho dá uma chave maior que 1.024 bytes (o teto do S3 e do
// painel: contrato, item 19) é falha daquele arquivo, com o motivo — não vai
// ao bucket nem ao índice, e os outros seguem. Antes, o bucket falso o
// aceitava e a entrada ia ao painel, que recusa o lote inteiro
// (`invalid_batch`).
func TestChaveAcimaDe1024BytesEFalhaDoArquivo(t *testing.T) {
	m := novaMesa(t)
	m.semear(5)
	// Cinco níveis de 250 bytes: cada nome cabe no sistema (255), o caminho
	// inteiro passa do teto da chave.
	fundo := filepath.Join(m.raiz, strings.Repeat("a", 250), strings.Repeat("b", 250), strings.Repeat("c", 250), strings.Repeat("d", 250), strings.Repeat("e", 250))
	if err := os.MkdirAll(fundo, 0o755); err != nil {
		t.Skipf("o sistema não cria um caminho deste tamanho: %v", err)
	}
	longo := filepath.Join(fundo, "longo.txt")
	if err := os.WriteFile(longo, []byte("x"), 0o644); err != nil {
		t.Skipf("o sistema não cria um caminho deste tamanho: %v", err)
	}
	// E um logo abaixo do teto, que tem de subir.
	raso := filepath.Join(m.raiz, strings.Repeat("a", 250), "raso.txt")
	if err := os.WriteFile(raso, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, em := m.rodar()
	if r.FilesFailed != 1 || r.Stats.FilesTotal != 6 || r.InventarioCompleto {
		t.Fatalf("falhas=%d arquivos=%d inventário completo=%v; queria 1 falha e 6 arquivos, sessão parcial", r.FilesFailed, r.Stats.FilesTotal, r.InventarioCompleto)
	}
	if !strings.Contains(r.PrimeiraFalha, "longo.txt") || !strings.Contains(r.PrimeiraFalha, "1.024 bytes") {
		t.Fatalf("o motivo tem de citar o arquivo e o limite: %q", r.PrimeiraFalha)
	}
	for _, e := range em {
		if len(e.Key) > MaxBytesDaChave {
			t.Fatalf("entrada com chave de %d bytes foi ao índice", len(e.Key))
		}
	}
	if len(em) != 6 {
		t.Fatalf("%d entradas no índice, queria 6", len(em))
	}
	for _, k := range m.s3.Chaves() {
		if len(k) > MaxBytesDaChave {
			t.Fatalf("objeto com chave de %d bytes foi ao bucket", len(k))
		}
	}
	if !errors.Is(erroDaChave(strings.Repeat("k", MaxBytesDaChave+1)), ErrChaveLonga) || erroDaChave(strings.Repeat("k", MaxBytesDaChave)) != nil {
		t.Fatal("o teto é 1.024 bytes: 1.024 cabe, 1.025 não")
	}
	// O teto é em bytes, não em letras.
	if erroDaChave(strings.Repeat("é", 512)) != nil || erroDaChave(strings.Repeat("é", 513)) == nil {
		t.Fatal("o teto conta bytes UTF-8")
	}
}
