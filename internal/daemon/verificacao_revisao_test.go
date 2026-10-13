package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/verificacao"
)

// Um link plantado em qualquer nível do caminho da pasta da rodada — por quem
// conseguiu escrever na pasta do estado — não é seguido: nada é apagado nem
// gravado através dele, nenhum objeto é baixado, e a rodada vai ao painel como
// `local_error` (sem pasta onde restaurar).
func TestLinkPlantadoNaPastaDaRodadaNaoESeguido(t *testing.T) {
	for _, nivel := range []string{"verificacao", "agente_armazenamento", "rodada"} {
		t.Run(nivel, func(t *testing.T) {
			m := mesaComBackup(t, 2)
			p := m.servirVerificacao(m.amostraDoCatalogo())
			niveis := niveisDaRodada(m.cfg, m.cfg.StateDir, runDeTeste)
			alvo := map[string]string{"verificacao": niveis[1], "agente_armazenamento": niveis[2], "rodada": niveis[3]}[nivel]

			alheio := t.TempDir()
			sentinela := filepath.Join(alheio, "nao-apague.txt")
			if err := os.WriteFile(sentinela, []byte("arquivo de outro lugar"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(alvo), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(alheio, alvo); err != nil {
				t.Skip("sem link simbólico neste sistema")
			}

			if _, err := abrirPastaDaRodada(m.cfg, runDeTeste); err == nil {
				t.Fatal("a pasta da rodada foi aberta através de um link")
			}
			rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

			if _, err := os.Lstat(sentinela); err != nil {
				t.Fatalf("o que estava atrás do link foi apagado: %v", err)
			}
			itens, err := os.ReadDir(alheio)
			if err != nil || len(itens) != 1 {
				t.Fatalf("gravou através do link: %v (err=%v)", itens, err)
			}
			if fi, err := os.Lstat(alvo); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("o link foi mexido (err=%v)", err)
			}
			if n := m.s3.N("GetObject"); n != 0 {
				t.Fatalf("baixou %d objeto(s) sem pasta confiável onde gravar", n)
			}
			for _, it := range p.relato(t).Items {
				if it.Status != verificacao.StatusPulado || it.Error != verificacao.PuladoErroLocal {
					t.Fatalf("com link na pasta o item vai como skipped/local_error; veio %+v", it)
				}
			}
		})
	}
}

// Pasta de outro dono no caminho também é recusada na abertura (não só na
// limpeza do arranque).
func TestPastaDeOutroDonoNaAberturaERecusada(t *testing.T) {
	m := mesaComBackup(t, 1)
	niveis := niveisDaRodada(m.cfg, m.cfg.StateDir, runDeTeste)
	if err := os.MkdirAll(niveis[2], 0o700); err != nil {
		t.Fatal(err)
	}
	antes := conferirDono
	conferirDono = func(p string) error {
		if p == niveis[2] {
			return errors.New("pasta de outro usuário")
		}
		return antes(p)
	}
	t.Cleanup(func() { conferirDono = antes })

	if rodada, err := abrirPastaDaRodada(m.cfg, runDeTeste); err == nil {
		t.Fatalf("abriu a rodada dentro de uma pasta de outro dono: %s", rodada)
	}
	if _, err := os.Lstat(niveis[3]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("criou a pasta da rodada dentro da pasta de outro dono (err=%v)", err)
	}
}

// A pergunta ao painel é feita sem a trava do bucket. Se um backup pega o
// bucket enquanto o painel responde, o plano fica guardado: a passada seguinte
// o executa SEM perguntar de novo (para o painel, cada plano entregue é uma
// amostra baixada).
func TestPlanoPendenteRodaQuandoOBucketSolta(t *testing.T) {
	m := mesaComBackup(t, 3)
	p := m.servirVerificacao(m.amostraDoCatalogo())
	trava := &exclusaoDoBucket{}
	servir := m.painel.Responder
	var travaLivreNaPergunta, pegou bool
	m.painel.Responder = func(rota string, r *http.Request, corpo []byte) (int, string) {
		if rota == "verify-plan" && !pegou {
			// O agente não segura a trava enquanto espera o plano…
			travaLivreNaPergunta = trava.mu.TryLock()
			// …e um backup a pega antes de o plano chegar.
			pegou = travaLivreNaPergunta
		}
		return servir(rota, r, corpo)
	}

	rodou, _ := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)

	if !travaLivreNaPergunta {
		t.Fatal("o agente segurou a trava do bucket durante o GET verify-plan")
	}
	if rodou {
		t.Fatal("a verificação rodou com o bucket ocupado")
	}
	if planos, relatos := p.contagem(); planos != 1 || relatos != 0 {
		t.Fatalf("com o bucket ocupado depois da pergunta: %d plano(s), %d relato(s)", planos, relatos)
	}
	if n := m.s3.N("GetObject"); n != 0 {
		t.Fatalf("baixou %d objeto(s) com o bucket ocupado", n)
	}
	// Ainda ocupado na passada seguinte: nem pergunta, nem roda, nem perde o plano.
	if rodou, _ := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava); rodou {
		t.Fatal("rodou com o bucket ainda ocupado")
	}

	trava.mu.Unlock()
	rodou, _ = rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)

	if !rodou {
		t.Fatal("com o bucket livre o plano pendente não rodou")
	}
	if planos, relatos := p.contagem(); planos != 1 || relatos != 1 {
		t.Fatalf("o plano pendente roda sem nova pergunta: %d pedido(s) de plano, %d relato(s)", planos, relatos)
	}
	if r := p.relato(t); r.RunID != runDeTeste || len(r.Items) != 3 {
		t.Fatalf("o relato não é o do plano guardado: %+v", r)
	}
	// Executado, o plano não fica guardado: a passada seguinte pergunta de novo.
	rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)
	if planos, _ := p.contagem(); planos != 2 {
		t.Fatalf("depois de executar, a passada seguinte volta a perguntar: %d pedido(s)", planos)
	}
}

// O plano que não pôde rodar em 6 horas é descartado (o painel já deu a rodada
// por perdida), sem baixar nada; a passada seguinte volta a perguntar.
func TestPlanoPendenteVencidoEDescartado(t *testing.T) {
	m := mesaComBackup(t, 2)
	p := m.servirVerificacao(m.amostraDoCatalogo())
	trava := &exclusaoDoBucket{}
	agora := time.Now()
	antes := agoraDaVerificacao
	agoraDaVerificacao = func() time.Time { return agora }
	t.Cleanup(func() { agoraDaVerificacao = antes })
	plano := m.amostraDoCatalogo()
	trava.pendente.guardar(&plano)

	agora = agora.Add(validadeDoPlanoPendente - time.Minute)
	if pl, vencido := trava.pendente.pegar(); pl == nil || vencido {
		t.Fatal("dentro do prazo o plano pendente continua valendo")
	}
	agora = agora.Add(2 * time.Minute)
	rodou, proxima := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)

	if !rodou || proxima != intervaloDaVerificacao {
		t.Fatalf("plano vencido: a passada termina e a próxima pergunta é no intervalo normal; veio rodou=%v proxima=%v", rodou, proxima)
	}
	if planos, relatos := p.contagem(); planos != 0 || relatos != 0 {
		t.Fatalf("o plano vencido não é executado nem relatado: %d plano(s), %d relato(s)", planos, relatos)
	}
	if n := m.s3.N("GetObject"); n != 0 {
		t.Fatalf("baixou %d objeto(s) de um plano vencido", n)
	}
	rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)
	if planos, relatos := p.contagem(); planos != 1 || relatos != 1 {
		t.Fatalf("descartado o vencido, a passada seguinte pergunta e executa: %d plano(s), %d relato(s)", planos, relatos)
	}
}

// relatosCrus guarda o corpo de cada POST verify-result e responde o que o
// teste mandar.
type relatosCrus struct {
	mu     gosync.Mutex
	corpos [][]byte
}

func (m *mesaDoDaemon) servirRelatos(responder func(n int, corpo []byte) (int, string)) *relatosCrus {
	rc := &relatosCrus{}
	m.painel.Responder = func(rota string, _ *http.Request, corpo []byte) (int, string) {
		if rota != "verify-result" {
			return 0, ""
		}
		rc.mu.Lock()
		rc.corpos = append(rc.corpos, append([]byte(nil), corpo...))
		n := len(rc.corpos)
		rc.mu.Unlock()
		return responder(n, corpo)
	}
	return rc
}

func relatoDeTeste(itens int, detalhe string) api.VerifyResult {
	r := api.VerifyResult{RunID: runDeTeste, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	for i := 0; i < itens; i++ {
		r.Items = append(r.Items, api.VerifyResultItem{Key: "arkame/data/ag1/arquivo", VersionID: "v", Status: verificacao.StatusFalhou, Error: verificacao.ErroDeDownload, Detail: detalhe})
	}
	return r
}

func detalhesDe(t *testing.T, corpo []byte) (itens, comDetalhe int) {
	t.Helper()
	var r api.VerifyResult
	if err := json.Unmarshal(corpo, &r); err != nil {
		t.Fatalf("relato que não é JSON: %v", err)
	}
	for _, it := range r.Items {
		if it.Detail != "" {
			comDetalhe++
		}
	}
	return len(r.Items), comDetalhe
}

// O relato tem de caber no 1 MiB que o painel aceita: grande demais, vai sem
// os detalhes — e com todos os itens.
func TestRelatoGrandeVaiSemDetalhe(t *testing.T) {
	m := mesaComBackup(t, 1)
	rc := m.servirRelatos(func(int, []byte) (int, string) { return 200, `{"status":"failed"}` })

	_, ok := enviarRelatoDeVerificacao(context.Background(), m.c, m.cfg, relatoDeTeste(verificacao.MaxItens, strings.Repeat("ç", 500)))

	if !ok || len(rc.corpos) != 1 {
		t.Fatalf("o relato grande tem de chegar, num pedido só: ok=%v pedidos=%d", ok, len(rc.corpos))
	}
	if len(rc.corpos[0]) > 1024*1024 {
		t.Fatalf("o relato saiu com %d bytes; o painel aceita 1 MiB", len(rc.corpos[0]))
	}
	if itens, comDetalhe := detalhesDe(t, rc.corpos[0]); itens != verificacao.MaxItens || comDetalhe != 0 {
		t.Fatalf("relato grande: todos os itens, nenhum detalhe; veio %d itens, %d com detalhe", itens, comDetalhe)
	}

	// O relato pequeno segue com os detalhes.
	rc.corpos = nil
	enviarRelatoDeVerificacao(context.Background(), m.c, m.cfg, relatoDeTeste(3, "NoSuchVersion"))
	if itens, comDetalhe := detalhesDe(t, rc.corpos[0]); itens != 3 || comDetalhe != 3 {
		t.Fatalf("relato pequeno leva os detalhes; veio %d itens, %d com detalhe", itens, comDetalhe)
	}
}

// O painel recusou o corpo (413, ou outro 4xx do corpo): o agente reenvia uma
// vez, sem os detalhes. Rodada que o painel não aceita mais (404, 409, 410) e
// erro do servidor não são reenviados.
func TestRelatoRecusadoEReenviadoSemDetalhe(t *testing.T) {
	m := mesaComBackup(t, 1)
	for _, caso := range []struct {
		nome    string
		status  int
		pedidos int
		ok      bool
	}{
		{"413 corpo grande", http.StatusRequestEntityTooLarge, 2, true},
		{"400 corpo inválido", http.StatusBadRequest, 2, true},
		{"404 rodada desconhecida", http.StatusNotFound, 1, false},
		{"409 rodada encerrada", http.StatusConflict, 1, false},
		{"410 rodada expirada", http.StatusGone, 1, false},
		{"403 credencial", http.StatusForbidden, 1, false},
		{"500 painel fora", http.StatusInternalServerError, 1, false},
	} {
		t.Run(caso.nome, func(t *testing.T) {
			rc := m.servirRelatos(func(n int, _ []byte) (int, string) {
				if n == 1 {
					return caso.status, `{"error":"x"}`
				}
				return 200, `{"status":"failed","failed":2}`
			})

			resposta, ok := enviarRelatoDeVerificacao(context.Background(), m.c, m.cfg, relatoDeTeste(2, "detalhe com \x7f lixo"))

			if len(rc.corpos) != caso.pedidos || ok != caso.ok {
				t.Fatalf("status %d: %d pedido(s) e ok=%v; queria %d e %v", caso.status, len(rc.corpos), ok, caso.pedidos, caso.ok)
			}
			if _, comDetalhe := detalhesDe(t, rc.corpos[0]); comDetalhe != 2 {
				t.Fatalf("o primeiro envio leva os detalhes; veio %d", comDetalhe)
			}
			if caso.pedidos == 2 {
				if itens, comDetalhe := detalhesDe(t, rc.corpos[1]); itens != 2 || comDetalhe != 0 {
					t.Fatalf("o reenvio vai com todos os itens e sem detalhe; veio %d itens, %d com detalhe", itens, comDetalhe)
				}
				if resposta.Failed != 2 {
					t.Fatalf("a resposta é a do reenvio aceito: %+v", resposta)
				}
			}
		})
	}

	// Sem detalhe nenhum não há o que tirar: um pedido só.
	rc := m.servirRelatos(func(int, []byte) (int, string) { return 413, `{}` })
	if _, ok := enviarRelatoDeVerificacao(context.Background(), m.c, m.cfg, relatoDeTeste(2, "")); ok || len(rc.corpos) != 1 {
		t.Fatalf("relato sem detalhe recusado não é reenviado: ok=%v pedidos=%d", ok, len(rc.corpos))
	}
}
