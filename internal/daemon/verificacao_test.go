package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/verificacao"
)

const runDeTeste = "0b6f3c1e-5a1d-4c7e-9d2a-3f4b5c6d7e8f"

// amostraDoCatalogo monta um plano de verificação com tudo o que o painel
// falso tem catalogado, em ordem de chave.
func (m *mesaDoDaemon) amostraDoCatalogo() api.VerifyPlanResponse {
	m.t.Helper()
	cat := m.painel.Catalogo("ag1", "p1")
	chaves := make([]string, 0, len(cat))
	for k := range cat {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	plano := api.VerifyPlanResponse{RunID: runDeTeste, StorageID: "st1", Bucket: "b", MaxBytes: 1 << 30, DeadlineSeconds: 600}
	for _, k := range chaves {
		e := cat[k]
		plano.Items = append(plano.Items, api.VerifyItem{Key: k, VersionID: e.VersionID, Size: e.Size, SHA256: e.SHA256})
	}
	return plano
}

// painelDaVerificacao faz o painel falso entregar o plano e guardar o relato.
type painelDaVerificacao struct {
	mu      gosync.Mutex
	planos  int
	relatos []api.VerifyResult
}

func (m *mesaDoDaemon) servirVerificacao(plano api.VerifyPlanResponse) *painelDaVerificacao {
	m.t.Helper()
	p := &painelDaVerificacao{}
	corpoDoPlano, _ := json.Marshal(plano)
	m.painel.Responder = func(rota string, r *http.Request, corpo []byte) (int, string) {
		switch rota {
		case "verify-plan":
			p.mu.Lock()
			p.planos++
			p.mu.Unlock()
			return 200, string(corpoDoPlano)
		case "verify-result":
			var relato api.VerifyResult
			if err := json.Unmarshal(corpo, &relato); err != nil {
				return 400, `{}`
			}
			p.mu.Lock()
			p.relatos = append(p.relatos, relato)
			p.mu.Unlock()
			return 200, `{"status":"completed"}`
		}
		return 0, ""
	}
	return p
}

func (p *painelDaVerificacao) contagem() (planos, relatos int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.planos, len(p.relatos)
}

func (p *painelDaVerificacao) relato(t *testing.T) api.VerifyResult {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.relatos) != 1 {
		t.Fatalf("esperava um relato no painel, chegaram %d", len(p.relatos))
	}
	return p.relatos[0]
}

// mesaComBackup: um servidor com n arquivos já guardados (com o conteúdo no
// bucket, para o GetObject).
func mesaComBackup(t *testing.T, n int) *mesaDoDaemon {
	m := novaMesaDoDaemon(t)
	m.s3.GuardarDados = true
	m.semear(n)
	m.rodarOK()
	m.s3.Zerar()
	return m
}

func statusDe(r api.VerifyResult, chave string) string {
	for _, it := range r.Items {
		if it.Key == chave {
			return it.Status + "/" + it.Error
		}
	}
	return "ausente"
}

// A rodada inteira, como o daemon a faz: restaura a amostra, acusa a versão
// apagada do bucket e a de sha256 divergente, relata, tira as duas do estado
// local (o próximo backup as reenvia) e não deixa nada no disco.
func TestVerificacaoRelataEDaBaixaNoEstado(t *testing.T) {
	m := mesaComBackup(t, 12)
	plano := m.amostraDoCatalogo()
	apagada, adulterada, sa := plano.Items[2], &plano.Items[5], plano.Items[7]
	m.s3.ApagarVersao(apagada.Key, apagada.VersionID)
	adulterada.SHA256 = strings.Repeat("ab", 32) // o catálogo diz outro conteúdo
	p := m.servirVerificacao(plano)

	rodou, proxima := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

	if !rodou || proxima != intervaloDaVerificacao {
		t.Fatalf("rodou=%v proxima=%v", rodou, proxima)
	}
	r := p.relato(t)
	if r.RunID != runDeTeste || len(r.Items) != 12 || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("relato: run=%q itens=%d início=%v fim=%v", r.RunID, len(r.Items), r.StartedAt, r.FinishedAt)
	}
	if got := statusDe(r, apagada.Key); got != "failed/not_found" {
		t.Fatalf("versão apagada do bucket: %s", got)
	}
	if got := statusDe(r, adulterada.Key); got != "failed/hash_mismatch" {
		t.Fatalf("sha256 divergente: %s", got)
	}
	oks := 0
	for i, it := range r.Items {
		if it.Status != verificacao.StatusOK {
			continue
		}
		oks++
		if it.SHA256 != plano.Items[i].SHA256 || it.Bytes != plano.Items[i].Size {
			t.Fatalf("item ok sem o sha256/tamanho relido do disco: %+v", it)
		}
	}
	if oks != 10 {
		t.Fatalf("esperava 10 itens ok, vieram %d", oks)
	}
	if n := m.s3.N("RestoreObject"); n != 0 {
		t.Fatalf("%d RestoreObject", n)
	}

	estado := arquivoDoEstado(m.cfg, m.cfg.StateDir)
	for _, falha := range []api.VerifyItem{apagada, *adulterada} {
		// A entrada invalidada fica sem versão: o backup seguinte lê o arquivo.
		if v, achou := m.versaoNoEstado(estado, falha.Key); achou && v != "" {
			t.Fatalf("o estado local segue afirmando a versão %s de %s, que a verificação reprovou", v, falha.Key)
		}
	}
	if v, achou := m.versaoNoEstado(estado, sa.Key); !achou || v != sa.VersionID {
		t.Fatal("a versão sã saiu do estado local")
	}
	if _, err := os.Lstat(filepath.Join(m.cfg.StateDir, pastaDaVerificacao)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a pasta de verificação ficou no disco depois da rodada (err=%v)", err)
	}
}

// Com o bucket ocupado (backup ou limpeza em curso) a verificação nem
// pergunta ao painel — perguntar é o que emite a rodada.
func TestVerificacaoNaoPerguntaComBucketOcupado(t *testing.T) {
	m := mesaComBackup(t, 3)
	p := m.servirVerificacao(m.amostraDoCatalogo())
	trava := &exclusaoDoBucket{}
	trava.mu.Lock()

	rodou, _ := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)

	trava.mu.Unlock()
	if rodou {
		t.Fatal("a verificação rodou com o bucket ocupado")
	}
	if planos, relatos := p.contagem(); planos != 0 || relatos != 0 {
		t.Fatalf("falou com o painel com o bucket ocupado: %d pedido(s) de plano, %d relato(s)", planos, relatos)
	}
	if n := m.s3.N("GetObject"); n != 0 {
		t.Fatalf("baixou %d objeto(s) com o bucket ocupado", n)
	}

	if rodou, _ := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava); !rodou {
		t.Fatal("com o bucket livre a verificação segue adiada")
	}
	if planos, relatos := p.contagem(); planos != 1 || relatos != 1 {
		t.Fatalf("com o bucket livre: %d plano(s), %d relato(s)", planos, relatos)
	}
}

// Um backup que chega no meio da verificação não espera a amostra inteira: a
// rodada encerra no item em curso, relata o resto como busy e solta o bucket.
func TestBackupEsperandoEncerraAVerificacao(t *testing.T) {
	m := mesaComBackup(t, 6)
	p := m.servirVerificacao(m.amostraDoCatalogo())
	trava := &exclusaoDoBucket{}
	backupRodou := make(chan struct{})
	var uma gosync.Once
	m.s3.Antes(func(op string, _ *http.Request) {
		if op != "GetObject" {
			return
		}
		uma.Do(func() {
			go func() {
				_ = trava.backup(func() error { close(backupRodou); return nil })
			}()
			for i := 0; i < 500 && !trava.backupEsperando(); i++ {
				time.Sleep(10 * time.Millisecond)
			}
		})
	})

	rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, trava)

	select {
	case <-backupRodou:
	case <-time.After(5 * time.Second):
		t.Fatal("o backup não rodou depois da verificação")
	}
	r := p.relato(t)
	if r.Items[0].Status != verificacao.StatusOK {
		t.Fatalf("o item em curso termina: %+v", r.Items[0])
	}
	for _, it := range r.Items[1:] {
		if it.Status != verificacao.StatusPulado || it.Error != verificacao.PuladoOcupado {
			t.Fatalf("com backup esperando o resto vai como busy; veio %+v", it)
		}
	}
	if n := m.s3.N("GetObject"); n != 1 {
		t.Fatalf("seguiu baixando com o backup esperando: %d GET", n)
	}
}

// Serviço parando no meio da rodada: nenhum relato sai (o painel reentrega a
// rodada) e a pasta da rodada não fica no disco.
func TestServicoParandoNaoRelataAVerificacao(t *testing.T) {
	m := mesaComBackup(t, 4)
	p := m.servirVerificacao(m.amostraDoCatalogo())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.s3.Antes(func(op string, _ *http.Request) {
		if op == "GetObject" {
			cancel()
		}
	})

	rodadaDeVerificacao(ctx, m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

	if _, relatos := p.contagem(); relatos != 0 {
		t.Fatalf("a rodada interrompida foi relatada (%d relato): o painel a daria por feita", relatos)
	}
	if n := m.s3.N("GetObject"); n > 1 {
		t.Fatalf("seguiu baixando depois da parada: %d GET", n)
	}
	if _, err := os.Lstat(filepath.Join(m.cfg.StateDir, pastaDaVerificacao)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a pasta de verificação ficou no disco (err=%v)", err)
	}
}

// O processo morreu no meio de uma rodada e deixou arquivos restaurados: o
// arranque seguinte apaga a pasta deste agente + armazenamento — e só ela (a
// de um processo irmão, de outro bucket, pode estar em uso).
func TestArranqueLimpaAVerificacaoInterrompida(t *testing.T) {
	cfg := &config.Config{AgentID: "ag1", HostRoot: "/", StorageID: "st1", StorageBucket: "b", StateDir: t.TempDir()}
	meu := filepath.Join(cfg.StateDir, pastaDaVerificacao, "ag1_st1", runDeTeste, "item-0003")
	irmao := filepath.Join(cfg.StateDir, pastaDaVerificacao, "ag1_st2", runDeTeste, "item-0000")
	estado := filepath.Join(cfg.StateDir, "ag1_st1.db")
	for _, p := range []string{meu, irmao, estado} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("dado do servidor"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	limparVerificacoes(cfg)

	if _, err := os.Lstat(filepath.Join(cfg.StateDir, pastaDaVerificacao, "ag1_st1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("o resto da rodada interrompida ficou no disco (err=%v)", err)
	}
	for _, p := range []string{irmao, estado} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("a limpeza do arranque apagou o que não era dela: %s (%v)", p, err)
		}
	}

	// Um link no lugar da pasta de verificação não é seguido.
	alheio := t.TempDir()
	dentro := filepath.Join(alheio, "ag1_st1", "arquivo")
	_ = os.MkdirAll(filepath.Dir(dentro), 0o700)
	_ = os.WriteFile(dentro, []byte("x"), 0o600)
	cfg2 := &config.Config{AgentID: "ag1", HostRoot: "/", StorageID: "st1", StorageBucket: "b", StateDir: t.TempDir()}
	if err := os.Symlink(alheio, filepath.Join(cfg2.StateDir, pastaDaVerificacao)); err != nil {
		t.Skip("sem link simbólico neste sistema")
	}
	limparVerificacoes(cfg2)
	if _, err := os.Lstat(dentro); err != nil {
		t.Fatalf("a limpeza seguiu um link e apagou fora da pasta do estado: %v", err)
	}
}

// Painel de antes do teste de restauração (404 na rota): o agente pergunta de
// novo só em um dia e registra uma linha, uma vez — não uma por pergunta.
func TestPainelSemARotaDeVerificacaoFicaQuieto(t *testing.T) {
	m := mesaComBackup(t, 2)
	semRotaDeVerificacao = gosync.Once{}
	t.Cleanup(func() { semRotaDeVerificacao = gosync.Once{} })
	perguntas := 0
	m.painel.Responder = func(rota string, _ *http.Request, _ []byte) (int, string) {
		if rota == "verify-plan" {
			perguntas++
			return 404, `{"error":"not_found"}`
		}
		return 0, ""
	}
	log := capturarLog(t)

	for i := 0; i < 3; i++ {
		rodou, proxima := rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})
		if !rodou || proxima != esperaSemRotaDeVerificacao {
			t.Fatalf("com 404 a próxima pergunta é em %v; veio rodou=%v proxima=%v", esperaSemRotaDeVerificacao, rodou, proxima)
		}
	}

	if perguntas != 3 {
		t.Fatalf("perguntas = %d", perguntas)
	}
	if n := strings.Count(log.String(), "ainda não tem o teste de restauração"); n != 1 {
		t.Fatalf("esperava uma linha de log por processo, saíram %d:\n%s", n, log.String())
	}
	if strings.Contains(log.String(), "level=WARN") || strings.Contains(log.String(), "level=ERROR") {
		t.Fatalf("painel antigo não é aviso nem erro:\n%s", log.String())
	}
}

// O run_id vira nome de pasta: o que não tem forma de UUID é recusado antes
// de qualquer pasta, download ou relato.
func TestRunIDMaliciosoERecusado(t *testing.T) {
	for _, runID := range []string{"../../../fora", "/etc/cron.d/x", `..\..\fora`, "a/b", strings.Repeat("a", 36), runDeTeste + "/..", " " + runDeTeste} {
		m := mesaComBackup(t, 2)
		plano := m.amostraDoCatalogo()
		plano.RunID = runID
		p := m.servirVerificacao(plano)
		acima := filepath.Dir(m.cfg.StateDir)
		antes, _ := os.ReadDir(acima)

		rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

		if _, relatos := p.contagem(); relatos != 0 {
			t.Fatalf("run_id %q: a rodada foi executada e relatada", runID)
		}
		if n := m.s3.N("GetObject") + m.s3.N("HeadObjectVersion") + m.s3.N("HeadObject"); n != 0 {
			t.Fatalf("run_id %q: %d requisição(ões) ao bucket", runID, n)
		}
		if _, err := os.Lstat(filepath.Join(m.cfg.StateDir, pastaDaVerificacao)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("run_id %q: criou pasta de verificação (err=%v)", runID, err)
		}
		if depois, _ := os.ReadDir(acima); len(depois) != len(antes) {
			t.Fatalf("run_id %q: apareceu algo fora da pasta do estado", runID)
		}
	}
}

// Plano de um armazenamento (ou bucket) que este processo não atende: silêncio
// — sem download e sem relato; o processo irmão pega.
func TestPlanoDeVerificacaoDeOutroArmazenamento(t *testing.T) {
	for _, mudar := range []func(*api.VerifyPlanResponse){
		func(p *api.VerifyPlanResponse) { p.Bucket = "outro" },
		func(p *api.VerifyPlanResponse) { p.StorageID = "st9" },
	} {
		m := mesaComBackup(t, 2)
		plano := m.amostraDoCatalogo()
		mudar(&plano)
		p := m.servirVerificacao(plano)

		rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

		if _, relatos := p.contagem(); relatos != 0 || m.s3.N("GetObject") != 0 {
			t.Fatalf("executou o plano de outro armazenamento: %d relato(s), %d GET", relatos, m.s3.N("GetObject"))
		}
	}
}

// O plano acima do teto do agente é cortado (1.000 itens), e sem pasta onde
// restaurar a rodada é relatada como local_error — não como falha do backup.
func TestTetoDeItensESemPasta(t *testing.T) {
	m := mesaComBackup(t, 1)
	plano := m.amostraDoCatalogo()
	um := plano.Items[0]
	for i := 0; i < verificacao.MaxItens+50; i++ {
		plano.Items = append(plano.Items, api.VerifyItem{Key: fmt.Sprintf("%s.%d", um.Key, i), VersionID: "v", Size: 1, SHA256: um.SHA256})
	}
	p := m.servirVerificacao(plano)
	antes := pastaPrivada
	pastaPrivada = func(string) error { return errors.New("pasta de outro dono") }
	t.Cleanup(func() { pastaPrivada = antes })

	rodadaDeVerificacao(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})

	r := p.relato(t)
	if len(r.Items) != verificacao.MaxItens {
		t.Fatalf("o relato tem %d itens; o teto do agente é %d", len(r.Items), verificacao.MaxItens)
	}
	for _, it := range r.Items {
		if it.Status != verificacao.StatusPulado || it.Error != verificacao.PuladoErroLocal {
			t.Fatalf("sem pasta o item vai como skipped/local_error; veio %+v", it)
		}
	}
	if n := m.s3.N("GetObject"); n != 0 {
		t.Fatalf("baixou %d objeto(s) sem ter onde gravar", n)
	}
}

// A capacidade só é declarada por quem tem como fazer o teste: armazenamento
// configurado e pasta de estado.
func TestCapacidadeDeVerificarRestauracao(t *testing.T) {
	completo := config.Config{AgentID: "ag1", StorageID: "st1", StorageBucket: "b", StateDir: "/var/lib/arkame/estado"}
	casos := []struct {
		nome  string
		mudar func(*config.Config)
		quer  bool
	}{
		{"completo", func(*config.Config) {}, true},
		{"sem armazenamento", func(c *config.Config) { c.StorageID = "" }, false},
		{"sem bucket", func(c *config.Config) { c.StorageBucket = "" }, false},
		{"estado desligado", func(c *config.Config) { c.StateDir = "" }, false},
	}
	for _, c := range casos {
		cfg := completo
		c.mudar(&cfg)
		// Com impedimento de atualização: a capacidade não depende dela.
		caps := (&atualizador{cfg: &cfg, impedimento: "desligada"}).capacidades()
		if tem := slices.Contains(caps, api.CapacidadeVerificarRestauracao); tem != c.quer {
			t.Errorf("%s: verify_restore declarada=%v, queria %v (%v)", c.nome, tem, c.quer, caps)
		}
		if !slices.Contains(caps, api.CapacidadeIndiceEmLotes) {
			t.Errorf("%s: as capacidades do índice sumiram: %v", c.nome, caps)
		}
	}
	if caps := (*atualizador)(nil).capacidades(); slices.Contains(caps, api.CapacidadeVerificarRestauracao) {
		t.Errorf("sem configuração não há o que declarar: %v", caps)
	}
}

// A pasta de verificação nunca entra no backup: está na lista do que a
// varredura não abre, e um arquivo restaurado que fique nela (rodada
// interrompida) não vai para o catálogo — mesmo com o plano cobrindo a pasta.
func TestPastaDeVerificacaoForaDoBackup(t *testing.T) {
	m := novaMesaDoDaemon(t)
	m.cfg.StateDir = filepath.Join(m.raiz, "estado") // dentro do que o plano copia
	resto := filepath.Join(m.cfg.StateDir, pastaDaVerificacao, "ag1_st1", runDeTeste, "item-0000")
	if err := os.MkdirAll(filepath.Dir(resto), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resto, []byte("arquivo restaurado de outro lugar"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.semear(3)

	if fora := pastasForaDoBackup(m.cfg); !slices.Contains(fora, filepath.Join(m.cfg.StateDir, pastaDaVerificacao)) {
		t.Fatalf("a pasta de verificação não está entre as que a varredura não abre: %v", fora)
	}
	m.rodarOK()
	for k := range m.painel.Catalogo("ag1", "p1") {
		if strings.Contains(k, "/"+pastaDaVerificacao+"/") || strings.HasSuffix(k, "item-0000") {
			t.Fatalf("arquivo da pasta de verificação entrou no backup: %s", k)
		}
	}
}

// O uninstall apaga as pastas de verificação deste agente (de qualquer
// armazenamento) e deixa as de outro agente da máquina.
func TestPastasDeVerificacaoDoAgente(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"ag1_st1", "ag1_st2", "ag10_st1", "outro_st1"} {
		if err := os.MkdirAll(filepath.Join(dir, pastaDaVerificacao, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	achadas := PastasDeVerificacao(&config.Config{AgentID: "ag1"}, []string{dir, filepath.Join(dir, "nao-existe")})
	var nomes []string
	for _, p := range achadas {
		nomes = append(nomes, filepath.Base(p))
	}
	sort.Strings(nomes)
	if !slices.Equal(nomes, []string{"ag1_st1", "ag1_st2"}) {
		t.Fatalf("pastas deste agente: %v", nomes)
	}
}

// O intervalo configurado só encurta a espera — e nunca a de um dia do painel
// que não tem a rota: é ela que mantém o agente quieto diante de um painel antigo.
func TestEsperaDaVerificacaoConfigurada(t *testing.T) {
	padrao := &config.Config{}
	curto := &config.Config{RestoreTestIntervalSec: 5}
	longo := &config.Config{RestoreTestIntervalSec: 7200}
	casos := []struct {
		nome   string
		cfg    *config.Config
		espera time.Duration
		quer   time.Duration
	}{
		{"sem configuração, a primeira pergunta", padrao, primeiraVerificacao, primeiraVerificacao},
		{"sem configuração, o intervalo", padrao, intervaloDaVerificacao, intervaloDaVerificacao},
		{"configurado, a primeira pergunta", curto, primeiraVerificacao, 5 * time.Second},
		{"configurado, o intervalo", curto, intervaloDaVerificacao, 5 * time.Second},
		{"configurado maior que o padrão não alonga", longo, intervaloDaVerificacao, intervaloDaVerificacao},
		{"painel sem a rota: um dia, com ou sem configuração", curto, esperaSemRotaDeVerificacao, esperaSemRotaDeVerificacao},
		{"sem config nenhuma", nil, intervaloDaVerificacao, intervaloDaVerificacao},
	}
	for _, c := range casos {
		if got := esperaDaVerificacao(c.cfg, c.espera); got != c.quer {
			t.Errorf("%s: espera %v, quero %v", c.nome, got, c.quer)
		}
	}
}
