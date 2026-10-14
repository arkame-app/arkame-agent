package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/inventario"
)

// painelDoInventario faz o painel falso entregar o catálogo em páginas (o
// cursor é o índice do primeiro item da página) e guardar o relato.
type painelDoInventario struct {
	mu        gosync.Mutex
	perguntas int
	paginas   []string
	relatos   []api.InventoryResult
	// comTrava conta as chamadas ao painel feitas com a trava do bucket segurada.
	comTrava int
}

func (m *mesaDoDaemon) catalogoParaInventario() []api.InventoryItem {
	m.t.Helper()
	cat := m.painel.Catalogo("ag1", "p1")
	chaves := make([]string, 0, len(cat))
	for k := range cat {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	itens := make([]api.InventoryItem, 0, len(chaves))
	for _, k := range chaves {
		itens = append(itens, api.InventoryItem{Key: k, VersionID: cat[k].VersionID, Size: cat[k].Size})
	}
	return itens
}

func (m *mesaDoDaemon) servirInventario(itens []api.InventoryItem, porPagina int, trava *exclusaoDoBucket, mudar func(*api.InventoryPlanResponse)) *painelDoInventario {
	m.t.Helper()
	p := &painelDoInventario{}
	pagina := func(ini int) string {
		fim := min(ini+porPagina, len(itens))
		r := api.InventoryPlanResponse{RunID: runDeTeste, StorageID: "st1", Bucket: "b", PrefixRoot: "", Prefix: "data/ag1/", Page: ini/porPagina + 1, DeadlineSeconds: 600, SampleSize: 4, Items: itens[ini:fim]}
		if fim < len(itens) {
			r.Next = strconv.Itoa(fim)
		}
		if mudar != nil {
			mudar(&r)
		}
		corpo, _ := json.Marshal(r)
		return string(corpo)
	}
	m.painel.Responder = func(rota string, r *http.Request, corpo []byte) (int, string) {
		if rota != "inventory-plan" && rota != "inventory-result" {
			return 0, ""
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if trava != nil && !trava.livre() {
			p.comTrava++
		}
		if rota == "inventory-result" {
			var relato api.InventoryResult
			if err := json.Unmarshal(corpo, &relato); err != nil {
				return 400, `{}`
			}
			p.relatos = append(p.relatos, relato)
			return 200, `{"status":"completed"}`
		}
		q := r.URL.Query()
		if q.Get("cursor") == "" {
			p.perguntas++
			if q.Get("storage_id") != "st1" {
				return 400, `{}`
			}
			p.paginas = append(p.paginas, "1")
			return 200, pagina(0)
		}
		if q.Get("run_id") != runDeTeste {
			return 404, `{}`
		}
		ini, _ := strconv.Atoi(q.Get("cursor"))
		p.paginas = append(p.paginas, q.Get("cursor"))
		return 200, pagina(ini)
	}
	return p
}

func (p *painelDoInventario) relato(t *testing.T) api.InventoryResult {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.relatos) != 1 {
		t.Fatalf("esperava um relato de inventário no painel, chegaram %d", len(p.relatos))
	}
	return p.relatos[0]
}

// A rodada inteira, como o daemon a faz: pede o plano página a página, confere
// contra o bucket, acusa a versão apagada, relata, tira-a do estado local — e
// não segura a trava do bucket enquanto fala com o painel.
func TestInventarioRelataEDaBaixaNoEstado(t *testing.T) {
	m := mesaComBackup(t, 23)
	itens := m.catalogoParaInventario()
	apagada := itens[9]
	m.s3.ApagarVersao(apagada.Key, apagada.VersionID)
	// Um envio de sessão que falhou antes do índice: objeto a mais, não é erro.
	m.s3.Semear("data/ag1/"+strings.TrimPrefix(m.raiz, "/")+"/p00/sobra-de-sessao-que-falhou.tmp", bancada.VersaoFalsa{Tamanho: 3})
	trava := &exclusaoDoBucket{}
	p := m.servirInventario(itens, 5, trava, nil)

	proxima := rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, trava)

	if proxima != intervaloDoInventario {
		t.Fatalf("próxima pergunta em %v", proxima)
	}
	r := p.relato(t)
	if r.RunID != runDeTeste || !r.Complete || r.Interrupted != "" || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("relato: %+v", r)
	}
	if r.PlanVersions != 23 || r.Found != 22 || r.MissingTotal != 1 || len(r.Missing) != 1 || r.Missing[0].Key != apagada.Key || r.Missing[0].VersionID != apagada.VersionID {
		t.Fatalf("23 no plano, 22 encontradas, a apagada faltando: plano=%d found=%d faltantes=%+v", r.PlanVersions, r.Found, r.Missing)
	}
	if r.ExtraVersions != 1 || r.Versioning != "Enabled" || r.Sample.Checked != 4 || len(r.Sample.Items) != 4 {
		t.Fatalf("a mais=%d versionamento=%q amostra=%d", r.ExtraVersions, r.Versioning, r.Sample.Checked)
	}
	for _, it := range r.Sample.Items {
		if len(it.SHA256) != 64 {
			t.Fatalf("o agente grava o metadado sha256 no envio; a amostra tem de trazê-lo: %+v", it)
		}
	}
	if p.perguntas != 1 || strings.Join(p.paginas, ",") != "1,5,10,15,20" {
		t.Fatalf("o plano é pedido uma vez e percorrido em ordem: perguntas=%d páginas=%v", p.perguntas, p.paginas)
	}
	if p.comTrava != 0 {
		t.Fatalf("o painel foi chamado %d vez(es) com a trava do bucket segurada", p.comTrava)
	}
	if n := m.s3.N("GetObject"); n != 0 {
		t.Fatalf("o inventário não baixa arquivo: %d GetObject", n)
	}
	estado := arquivoDoEstado(m.cfg, m.cfg.StateDir)
	if v, achou := m.versaoNoEstado(estado, apagada.Key); achou && v != "" {
		t.Fatalf("o estado local segue afirmando a versão %s, que o inventário não achou no bucket", v)
	}
	if v, achou := m.versaoNoEstado(estado, itens[3].Key); !achou || v != itens[3].VersionID {
		t.Fatal("a versão sã saiu do estado local")
	}
	if !trava.livre() {
		t.Fatal("a trava do bucket ficou segurada depois da rodada")
	}
}

// Com o bucket ocupado o inventário nem pergunta ao painel — perguntar é o que
// emite a rodada.
func TestInventarioNaoPerguntaComBucketOcupado(t *testing.T) {
	m := mesaComBackup(t, 3)
	p := m.servirInventario(m.catalogoParaInventario(), 100, nil, nil)
	trava := &exclusaoDoBucket{}
	trava.mu.Lock()
	// Com prazo: se o inventário perguntasse e ficasse esperando a vez, o teste
	// acusaria pela pergunta, não por travar.
	ctx, cancelar := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelar()
	proxima := rodadaDeInventario(ctx, m.c, m.s3c, m.cfg, trava)
	trava.mu.Unlock()
	if proxima != reprovaDaVerificacao {
		t.Fatalf("com o bucket ocupado tenta de novo em %v; veio %v", reprovaDaVerificacao, proxima)
	}
	if p.perguntas != 0 || len(p.relatos) != 0 || m.s3.N("ListObjectVersions") != 0 {
		t.Fatalf("falou com o painel ou listou com o bucket ocupado: %d pergunta(s), %d listagem(ns)", p.perguntas, m.s3.N("ListObjectVersions"))
	}
}

// A janela do inventário não entra na fila: com a trava ocupada espera sem
// travar ninguém, dá a vez ao backup que estiver esperando, e desiste quando o
// contexto vence.
func TestJanelaDoInventarioCedeAoBackup(t *testing.T) {
	antes := esperaPelaVezDoInventario
	esperaPelaVezDoInventario = 5 * time.Millisecond
	t.Cleanup(func() { esperaPelaVezDoInventario = antes })

	trava := &exclusaoDoBucket{}
	rodou := false
	if !trava.inventario(context.Background(), func() { rodou = true }) || !rodou {
		t.Fatal("com o bucket livre a janela roda")
	}
	if q, _ := trava.quem.Load().(string); q != atividadeInventario {
		t.Fatalf("a trava diz quem a segura: %q", q)
	}

	// Há backup esperando: a janela não pega a trava, mesmo livre.
	trava.esperando.Add(1)
	ctx, cancelar := context.WithTimeout(context.Background(), 40*time.Millisecond)
	rodou = false
	if trava.inventario(ctx, func() { rodou = true }) || rodou {
		t.Fatal("a janela passou na frente do backup que estava esperando")
	}
	cancelar()
	trava.esperando.Add(-1)

	// Ocupado e depois livre: a janela espera e roda.
	trava.mu.Lock()
	go func() {
		time.Sleep(30 * time.Millisecond)
		trava.mu.Unlock()
	}()
	rodou = false
	if !trava.inventario(context.Background(), func() { rodou = true }) || !rodou {
		t.Fatal("a janela não rodou depois que o bucket ficou livre")
	}

	// Ocupado até o prazo: desiste sem rodar.
	trava.mu.Lock()
	ctx, cancelar = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelar()
	if trava.inventario(ctx, func() { t.Error("rodou com o bucket ocupado") }) {
		t.Fatal("a janela disse que rodou com o bucket ocupado")
	}
	trava.mu.Unlock()
}

// O backup que chega no meio do inventário não espera a listagem inteira: a
// janela corrente é curta, e a seguinte só vem depois dele.
func TestBackupPassaNaFrenteDoInventario(t *testing.T) {
	antes := esperaPelaVezDoInventario
	esperaPelaVezDoInventario = 2 * time.Millisecond
	t.Cleanup(func() { esperaPelaVezDoInventario = antes })

	m := mesaComBackup(t, 40)
	itens := m.catalogoParaInventario()
	trava := &exclusaoDoBucket{}
	p := m.servirInventario(itens, 4, trava, nil)
	var ordem []string
	var mu gosync.Mutex
	anotar := func(s string) {
		mu.Lock()
		ordem = append(ordem, s)
		mu.Unlock()
	}
	var umaVez gosync.Once
	feito := make(chan struct{})
	m.s3.Antes(func(op string, _ *http.Request) {
		if op != "ListObjectVersions" {
			return
		}
		anotar("lista")
		// Na primeira listagem, um backup chega e fica esperando o bucket.
		umaVez.Do(func() {
			go func() {
				_ = trava.backup(func() error {
					anotar("backup")
					return nil
				})
				close(feito)
			}()
			time.Sleep(20 * time.Millisecond)
		})
	})
	rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, trava)
	m.s3.Antes(nil)
	<-feito
	r := p.relato(t)
	if !r.Complete || r.Found != 40 {
		t.Fatalf("o inventário segue depois do backup e termina: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if i := slices.Index(ordem, "backup"); i != 1 {
		t.Fatalf("o backup tinha de rodar logo depois da primeira janela do inventário, não no fim: %v", ordem)
	}
}

func TestServicoParandoNaoRelataOInventario(t *testing.T) {
	m := mesaComBackup(t, 30)
	p := m.servirInventario(m.catalogoParaInventario(), 5, nil, nil)
	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()
	m.s3.Antes(func(op string, _ *http.Request) {
		if op == "ListObjectVersions" {
			cancelar()
		}
	})
	rodadaDeInventario(ctx, m.c, m.s3c, m.cfg, &exclusaoDoBucket{})
	m.s3.Antes(nil)
	if len(p.relatos) != 0 {
		t.Fatalf("o serviço parando no meio não relata (o painel reentrega): %+v", p.relatos)
	}
}

func TestPainelSemARotaDeInventarioFicaQuieto(t *testing.T) {
	m := mesaComBackup(t, 2)
	semRotaDeInventario = gosync.Once{}
	t.Cleanup(func() { semRotaDeInventario = gosync.Once{} })
	perguntas := 0
	m.painel.Responder = func(rota string, _ *http.Request, _ []byte) (int, string) {
		if rota == "inventory-plan" {
			perguntas++
			return 404, `{"error":"not_found"}`
		}
		return 0, ""
	}
	log := capturarLog(t)
	for range 3 {
		if proxima := rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{}); proxima != esperaSemRotaDeInventario {
			t.Fatalf("com 404 a próxima pergunta é em %v; veio %v", esperaSemRotaDeInventario, proxima)
		}
	}
	if perguntas != 3 || strings.Count(log.String(), "ainda não tem o inventário do bucket") != 1 {
		t.Fatalf("uma linha de log por processo: perguntas=%d\n%s", perguntas, log.String())
	}
	if strings.Contains(log.String(), "level=WARN") || strings.Contains(log.String(), "level=ERROR") {
		t.Fatalf("painel antigo não é aviso nem erro:\n%s", log.String())
	}
}

// O plano tem de ser deste armazenamento, com run_id de verdade, e o prefixo
// tem de ser a pasta DESTE servidor: senão nada é listado nem relatado.
func TestPlanoDeInventarioQueNaoEDesteServidor(t *testing.T) {
	casos := map[string]func(*api.InventoryPlanResponse){
		"prefixo de outro servidor": func(r *api.InventoryPlanResponse) { r.Prefix = "data/ag2/" },
		"prefixo do bucket inteiro": func(r *api.InventoryPlanResponse) { r.Prefix = "" },
		"prefixo acima da pasta":    func(r *api.InventoryPlanResponse) { r.Prefix = "data/" },
		"outro bucket":              func(r *api.InventoryPlanResponse) { r.Bucket = "outro" },
		"outro armazenamento":       func(r *api.InventoryPlanResponse) { r.StorageID = "st9" },
		"run_id que não é UUID":     func(r *api.InventoryPlanResponse) { r.RunID = "../../etc" },
	}
	for nome, mudar := range casos {
		t.Run(nome, func(t *testing.T) {
			m := mesaComBackup(t, 3)
			p := m.servirInventario(m.catalogoParaInventario(), 100, nil, mudar)
			rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{})
			if n := m.s3.N("ListObjectVersions"); n != 0 || len(p.relatos) != 0 {
				t.Fatalf("plano que não é deste servidor: %d listagem(ns), %d relato(s)", n, len(p.relatos))
			}
		})
	}
}

// Sem nada a fazer (o caso de quase toda hora): nenhuma requisição ao bucket.
func TestSemInventarioAFazerNaoTocaOBucket(t *testing.T) {
	m := mesaComBackup(t, 3)
	m.painel.Responder = func(rota string, _ *http.Request, _ []byte) (int, string) {
		if rota == "inventory-plan" {
			return 200, `{"run_id":null,"items":[],"next":null}`
		}
		return 0, ""
	}
	if proxima := rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, &exclusaoDoBucket{}); proxima != intervaloDoInventario {
		t.Fatalf("próxima em %v", proxima)
	}
	if n := m.s3.N("ListObjectVersions") + m.s3.N("GetBucketVersioning"); n != 0 {
		t.Fatalf("sem rodada, %d requisição(ões) ao bucket", n)
	}
}

func TestCapacidadeDeInventariarOBucket(t *testing.T) {
	completo := config.Config{AgentID: "ag1", StorageID: "st1", StorageBucket: "b"}
	casos := []struct {
		nome  string
		mudar func(*config.Config)
		quer  bool
	}{
		{"completo, sem pasta de estado", func(*config.Config) {}, true},
		{"sem armazenamento", func(c *config.Config) { c.StorageID = "" }, false},
		{"sem bucket", func(c *config.Config) { c.StorageBucket = "" }, false},
	}
	for _, c := range casos {
		cfg := completo
		c.mudar(&cfg)
		caps := (&atualizador{cfg: &cfg, impedimento: "desligada"}).capacidades()
		if tem := slices.Contains(caps, api.CapacidadeInventariarBucket); tem != c.quer {
			t.Errorf("%s: bucket_inventory declarada=%v, queria %v (%v)", c.nome, tem, c.quer, caps)
		}
	}
	if caps := (*atualizador)(nil).capacidades(); slices.Contains(caps, api.CapacidadeInventariarBucket) {
		t.Errorf("sem configuração não há o que declarar: %v", caps)
	}
}

func TestEsperaDoInventarioConfigurada(t *testing.T) {
	cfg := &config.Config{RestoreTestIntervalSec: 30}
	if d := esperaDoInventario(cfg, intervaloDoInventario); d != 30*time.Second {
		t.Fatalf("o intervalo configurado encurta a pergunta: %v", d)
	}
	if d := esperaDoInventario(cfg, esperaSemRotaDeInventario); d != esperaSemRotaDeInventario {
		t.Fatalf("e não encurta a espera do painel sem a rota: %v", d)
	}
	if d := esperaDoInventario(&config.Config{}, primeiroInventario); d != primeiroInventario {
		t.Fatalf("sem configuração vale o padrão: %v", d)
	}
	if _, a := inventario.Limites(600, 4); a != 4 {
		t.Fatalf("a amostra pedida pelo painel vale até o teto: %d", a)
	}
}

// Revisão do S9 (M2): o backup que chega no MEIO de uma janela do inventário
// não espera a janela inteira — entre uma requisição e outra o inventário vê
// que há backup esperando e solta a trava.
func TestBackupPassaNoMeioDaJanelaDoInventario(t *testing.T) {
	antes := esperaPelaVezDoInventario
	esperaPelaVezDoInventario = 2 * time.Millisecond
	t.Cleanup(func() { esperaPelaVezDoInventario = antes })

	m := mesaComBackup(t, 40)
	itens := m.catalogoParaInventario()
	trava := &exclusaoDoBucket{}
	// Uma página só e uma listagem só: a janela que interessa é a da amostra,
	// com quatro HeadObject.
	p := m.servirInventario(itens, 1000, trava, nil)
	var ordem []string
	var mu gosync.Mutex
	anotar := func(s string) {
		mu.Lock()
		ordem = append(ordem, s)
		mu.Unlock()
	}
	var umaVez gosync.Once
	feito := make(chan struct{})
	m.s3.Antes(func(op string, _ *http.Request) {
		if op != "HeadObjectVersion" {
			return
		}
		anotar("head")
		// No primeiro HEAD da janela, um backup chega e fica esperando o bucket.
		umaVez.Do(func() {
			go func() {
				_ = trava.backup(func() error {
					anotar("backup")
					return nil
				})
				close(feito)
			}()
			time.Sleep(20 * time.Millisecond)
		})
	})
	rodadaDeInventario(context.Background(), m.c, m.s3c, m.cfg, trava)
	m.s3.Antes(nil)
	<-feito
	r := p.relato(t)
	if !r.Complete || r.Found != 40 || r.Sample.Checked != 4 {
		t.Fatalf("o inventário segue depois do backup e termina, com a amostra inteira: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ordem) != 5 || ordem[0] != "head" || ordem[1] != "backup" {
		t.Fatalf("o backup tinha de rodar logo depois do HEAD em curso, não no fim da janela de quatro: %v", ordem)
	}
}
