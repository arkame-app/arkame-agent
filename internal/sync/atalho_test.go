package sync

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/estado"
)

func semCtime(t *testing.T) {
	antes := lerCtime
	lerCtime = func(fs.FileInfo) (int64, uint32) { return 0, 0 }
	t.Cleanup(func() { lerCtime = antes })
}

func contar(es []api.IndexEntry, op string) int {
	n := 0
	for _, e := range es {
		if e.Op == op {
			n++
		}
	}
	return n
}

// Backup sem mudança: nenhum arquivo é relido e o bucket não recebe
// requisição nenhuma; o delta sai vazio.
func TestSemMudancaNaoLeNemConsultaOBucket(t *testing.T) {
	m := novaMesa(t)
	m.semear(200)
	r, em := m.rodar()
	if r.Stats.FilesUploaded != 200 || len(em) != 200 || m.s3.N("PutObject") != 200 || m.s3.N("HeadObject") != 200 {
		t.Fatalf("primeira: enviados=%d emitidas=%d s3=%v", r.Stats.FilesUploaded, len(em), m.s3.Requisicoes())
	}
	m.conferir()

	m.s3.Zerar()
	r, em = m.rodar()
	if len(m.s3.Requisicoes()) != 0 {
		t.Fatalf("sem mudança, o bucket não devia ser consultado: %v", m.s3.Requisicoes())
	}
	if r.Atalhos != 200 || r.Stats.FilesTotal != 200 || r.Stats.FilesUploaded != 0 || len(em) != 0 || !r.InventarioCompleto {
		t.Fatalf("atalhos=%d total=%d enviados=%d emitidas=%d", r.Atalhos, r.Stats.FilesTotal, r.Stats.FilesUploaded, len(em))
	}
	m.conferir()

	// Em modo full (painel sem delta) o inventário inteiro sai do estado,
	// ainda sem ler arquivo nem consultar o bucket.
	m.delta = false
	r, em = m.rodar()
	if len(m.s3.Requisicoes()) != 0 || contar(em, api.OpUpsert) != 200 || r.Atalhos != 200 {
		t.Fatalf("full com estado: s3=%v emitidas=%d", m.s3.Requisicoes(), len(em))
	}
	m.conferir()
}

// Alterado, novo, removido e fora da seleção: só o que mudou vai ao bucket e
// ao índice.
func TestDeltaLevaSoOQueMudou(t *testing.T) {
	m := novaMesa(t)
	m.semear(100)
	m.escrever("logs/a.log", "log a")
	m.escrever("logs/b.log", "log b")
	m.rodar()
	m.conferir()

	m.escrever("p00/arquivo-0000.txt", "conteúdo novo, maior")
	m.escrever("p01/arquivo-0001.txt", "outro conteúdo novo")
	m.escrever("novo/x.txt", "x")
	if err := os.Remove(filepath.Join(m.raiz, "p02/arquivo-0002.txt")); err != nil {
		t.Fatal(err)
	}
	m.exclusoes = []string{"*.log"}
	m.s3.Zerar()
	r, em := m.rodar()
	if contar(em, api.OpUpsert) != 3 || contar(em, api.OpRemoved) != 1 || contar(em, api.OpUnselected) != 2 {
		t.Fatalf("emitidas: %+v", em)
	}
	if m.s3.N("PutObject") != 3 || m.s3.N("HeadObject") != 3 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
	if r.FilesNew != 1 || r.FilesChanged != 2 || r.FilesRemoved != 1 || r.FilesUnselected != 2 || r.ChangedSameMtime != 0 {
		t.Fatalf("contagens: novo=%d alterado=%d removido=%d fora=%d", r.FilesNew, r.FilesChanged, r.FilesRemoved, r.FilesUnselected)
	}
	m.conferir(".log")

	// As remoções foram confirmadas: não saem de novo, e as lápides somem.
	_, em = m.rodar()
	if len(em) != 0 {
		t.Fatalf("remoção emitida duas vezes: %+v", em)
	}
	// Arquivo que volta depois de removido é novo.
	m.escrever("p02/arquivo-0002.txt", "voltou")
	r, em = m.rodar()
	if contar(em, api.OpUpsert) != 1 || r.FilesNew != 1 {
		t.Fatalf("arquivo que voltou: %+v novo=%d", em, r.FilesNew)
	}
	m.conferir(".log")
}

// Tocar sem mudar: a data muda, o conteúdo não. Relê e confere no bucket, não
// reenvia; o índice recebe a data nova na mesma versão.
func TestTocarSemMudarNaoReenvia(t *testing.T) {
	m := novaMesa(t)
	m.semear(20)
	m.rodar()
	antes := m.presenca[m.chave("p00/arquivo-0000.txt")]
	nova := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(m.raiz, "p00/arquivo-0000.txt"), nova, nova); err != nil {
		t.Fatal(err)
	}
	m.s3.Zerar()
	r, em := m.rodar()
	if m.s3.N("PutObject") != 0 || m.s3.N("HeadObject") != 1 || r.Stats.FilesUploaded != 0 {
		t.Fatalf("s3: %v", m.s3.Requisicoes())
	}
	if len(em) != 1 || em[0].VersionID != antes.VersionID || !em[0].ModifiedAt.Equal(nova) {
		t.Fatalf("emitidas: %+v", em)
	}
	if r.FilesNew != 0 || r.FilesChanged != 0 {
		t.Fatalf("tocar não é alterar: novo=%d alterado=%d", r.FilesNew, r.FilesChanged)
	}
	m.conferir()
}

// Mudar sem tocar (conteúdo diferente, tamanho e data iguais). Com o ctime
// (Linux, macOS) o atalho já recusa. Sem ele (Windows) o atalho não vê — e é
// para isso que existem a releitura total e a rotativa.
func TestMudarSemTocar(t *testing.T) {
	t.Run("com ctime é pego na hora", func(t *testing.T) {
		m := novaMesa(t)
		m.semear(30)
		m.rodar()
		alvo := filepath.Join(m.raiz, "p03/arquivo-0003.txt")
		st, _ := os.Stat(alvo)
		// O relógio do sistema de arquivos tem de andar para o ctime mudar.
		time.Sleep(20 * time.Millisecond)
		if err := bancada.MudarSemTocar(alvo); err != nil {
			t.Fatal(err)
		}
		if st2, _ := os.Stat(alvo); !st2.ModTime().Equal(st.ModTime()) || st2.Size() != st.Size() {
			t.Fatal("a mutação devia preservar data e tamanho")
		}
		if seg, nano := ctimeDe(st); seg == 0 && nano == 0 {
			t.Skip("sistema sem ctime")
		}
		r, em := m.rodar()
		if r.FilesChanged != 1 || contar(em, api.OpUpsert) != 1 || r.Stats.FilesUploaded != 1 {
			t.Fatalf("alterado=%d emitidas=%d enviados=%d", r.FilesChanged, len(em), r.Stats.FilesUploaded)
		}
		m.conferir()
	})

	t.Run("sem ctime só a releitura total pega", func(t *testing.T) {
		semCtime(t)
		m := novaMesa(t)
		m.semear(30)
		m.rodar()
		if err := bancada.MudarSemTocar(filepath.Join(m.raiz, "p03/arquivo-0003.txt")); err != nil {
			t.Fatal(err)
		}
		// O atalho não vê: é o limite conhecido dele.
		r, em := m.rodar()
		if r.Atalhos != 30 || len(em) != 0 {
			t.Fatalf("esperava o atalho cego aqui: atalhos=%d emitidas=%d", r.Atalhos, len(em))
		}
		m.rehash = true
		m.s3.Zerar()
		r, em = m.rodar()
		if r.ChangedSameMtime != 1 || r.Stats.FilesUploaded != 1 || contar(em, api.OpUpsert) != 1 {
			t.Fatalf("releitura total: mesma_data=%d enviados=%d emitidas=%+v", r.ChangedSameMtime, r.Stats.FilesUploaded, em)
		}
		if m.s3.N("HeadObject") != 30 || m.s3.N("PutObject") != 1 {
			t.Fatalf("releitura total confere todos no bucket e envia um: %v", m.s3.Requisicoes())
		}
		m.conferir()
	})

	t.Run("sem ctime a releitura rotativa pega dentro do ciclo", func(t *testing.T) {
		semCtime(t)
		m := novaMesa(t)
		m.semear(300)
		m.rodar()
		if err := bancada.MudarSemTocar(filepath.Join(m.raiz, "p03/arquivo-0003.txt")); err != nil {
			t.Fatal(err)
		}
		m.s3.Zerar()
		pegos, verificados, maiorDia := int64(0), int64(0), int64(0)
		for dia := 1; dia <= 31; dia++ {
			m.agora = m.agora.Add(24 * time.Hour)
			r, _ := m.rodar()
			pegos += r.ChangedSameMtime
			verificados += r.Verificados
			maiorDia = max(maiorDia, r.Verificados)
		}
		if pegos != 1 {
			t.Fatalf("a alteração devia ser pega uma vez no ciclo de 30 dias; pegos=%d", pegos)
		}
		// Cada arquivo é relido uma vez por ciclo, espalhado pelos dias.
		if verificados < 300 || verificados > 330 || maiorDia > 40 {
			t.Fatalf("reconferências no ciclo = %d (queria ~300), maior dia = %d", verificados, maiorDia)
		}
		if h := m.s3.N("HeadObject"); h != verificados {
			t.Fatalf("HeadObject = %d, reconferências = %d", h, verificados)
		}
		if m.s3.N("PutObject") != 1 {
			t.Fatalf("só o arquivo alterado sobe: %v", m.s3.Requisicoes())
		}
		m.conferir()
	})
}

// O teto por execução segura a reconferência: o que não coube fica para a
// execução seguinte.
func TestReconferenciaRespeitaOTeto(t *testing.T) {
	m := novaMesa(t)
	m.semear(120)
	m.rodar()
	m.teto = 10
	m.agora = m.agora.Add(40 * 24 * time.Hour) // todos vencidos
	total := int64(0)
	for i := 0; i < 12; i++ {
		m.s3.Zerar()
		r, _ := m.rodar()
		if r.Verificados > 10 || m.s3.N("HeadObject") > 10 {
			t.Fatalf("passou do teto: %d reconferências, %v", r.Verificados, m.s3.Requisicoes())
		}
		total += r.Verificados
	}
	if total != 120 {
		t.Fatalf("em 12 execuções de 10 todos os 120 deviam ser reconferidos; foram %d", total)
	}
}

// Versão apagada direto no bucket: a reconferência rotativa (HeadObject)
// percebe e o arquivo sobe de novo.
func TestVersaoApagadaNoBucketEPegaPelaReconferencia(t *testing.T) {
	m := novaMesa(t)
	m.semear(60)
	m.rodar()
	k := m.chave("p04/arquivo-0004.txt")
	if !m.s3.ApagarVersao(k, m.presenca[k].VersionID) {
		t.Fatal("não achei a versão para apagar")
	}
	if _, tem := m.s3.Atual(k); tem {
		t.Fatal("o objeto devia ter sumido")
	}
	m.s3.Zerar()
	reenvios := int64(0)
	for dia := 1; dia <= 31; dia++ {
		m.agora = m.agora.Add(24 * time.Hour)
		r, em := m.rodar()
		reenvios += r.Stats.FilesUploaded
		if r.Stats.FilesUploaded == 1 && (len(em) != 1 || em[0].Key != k) {
			t.Fatalf("o reenvio devia ir ao índice: %+v", em)
		}
	}
	if reenvios != 1 || m.s3.N("PutObject") != 1 {
		t.Fatalf("o objeto sumido devia subir uma vez no ciclo; reenvios=%d s3=%v", reenvios, m.s3.Requisicoes())
	}
	m.conferir()
}

// Estado perdido, cortado ou adulterado: a execução seguinte roda em modo
// completo (lê e confere tudo), acerta o índice e não reenvia arquivo igual.
func TestEstadoPerdidoNaoReenvia(t *testing.T) {
	estragos := map[string]func(t *testing.T, arq string){
		"ausente": func(t *testing.T, arq string) {
			if err := os.Remove(arq); err != nil {
				t.Fatal(err)
			}
		},
		"cortado": func(t *testing.T, arq string) {
			st, _ := os.Stat(arq)
			if err := os.Truncate(arq, st.Size()/8); err != nil {
				t.Fatal(err)
			}
		},
		"adulterado": func(t *testing.T, arq string) {
			b, _ := os.ReadFile(arq)
			for i := 3 * 4096; i < len(b); i += 61 {
				b[i] ^= 0xa5
			}
			if err := os.WriteFile(arq, b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for nome, estragar := range estragos {
		t.Run(nome, func(t *testing.T) {
			m := novaMesa(t)
			m.semear(400)
			m.rodar()
			m.escrever("p00/arquivo-0000.txt", "mudou depois")
			estragar(t, m.ultimaBase.Caminho())
			m.s3.Zerar()
			r, em := m.rodar()
			if m.s3.N("PutObject") != 1 || r.Stats.FilesUploaded != 1 {
				t.Fatalf("só o arquivo alterado devia subir: enviados=%d s3=%v", r.Stats.FilesUploaded, m.s3.Requisicoes())
			}
			if m.s3.N("HeadObject") != 400 {
				t.Fatalf("modo completo confere todos no bucket: %v", m.s3.Requisicoes())
			}
			if r.Stats.FilesTotal != 400 || contar(em, api.OpUpsert) != 400 {
				t.Fatalf("o inventário completo devia sair: total=%d emitidas=%d", r.Stats.FilesTotal, len(em))
			}
			m.conferir()
			// E o estado foi reconstruído: a seguinte volta ao atalho.
			m.s3.Zerar()
			r, _ = m.rodar()
			if r.Atalhos != 400 || len(m.s3.Requisicoes()) != 0 {
				t.Fatalf("estado não reconstruído: atalhos=%d s3=%v estadoFalhou=%v", r.Atalhos, m.s3.Requisicoes(), r.EstadoFalhou)
			}
		})
	}
}

// Sessão que o painel não confirmou: as entradas dela não valem. A execução
// seguinte relê e confere esses arquivos (sem reenviar) e os emite de novo.
func TestSessaoNaoConfirmadaNaoVale(t *testing.T) {
	m := novaMesa(t)
	m.semear(50)
	m.rodar()
	m.escrever("p00/arquivo-0000.txt", "versão 2")
	m.escrever("novo.txt", "novo")
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
		t.Fatal(err)
	}
	m.confirmar = false
	_, em := m.rodar() // o /complete desta nunca chegou
	if contar(em, api.OpUpsert) != 2 || contar(em, api.OpRemoved) != 1 {
		t.Fatalf("emitidas: %+v", em)
	}
	m.confirmar = true
	m.s3.Zerar()
	r, em := m.rodar()
	if contar(em, api.OpUpsert) != 2 || contar(em, api.OpRemoved) != 1 {
		t.Fatalf("o que a sessão perdida levou tem de sair de novo: %+v", em)
	}
	if m.s3.N("PutObject") != 0 || m.s3.N("HeadObject") != 2 || r.Atalhos != 48 {
		t.Fatalf("sem reenvio, e só os dois pendentes são conferidos: s3=%v atalhos=%d", m.s3.Requisicoes(), r.Atalhos)
	}
	m.conferir()
}

// Sessão parcial (um arquivo ilegível) não emite `removed`: a varredura não
// prova que o que faltou saiu da origem.
func TestSessaoParcialNaoEmiteRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lê qualquer arquivo")
	}
	m := novaMesa(t)
	m.semear(40)
	m.rodar()
	if err := os.Remove(filepath.Join(m.raiz, "p01/arquivo-0001.txt")); err != nil {
		t.Fatal(err)
	}
	trancado := filepath.Join(m.raiz, "p02/arquivo-0002.txt")
	if err := os.WriteFile(trancado, []byte("mudou e trancou"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(trancado, 0o000); err != nil {
		t.Fatal(err)
	}
	r, em := m.rodar()
	if r.FilesFailed != 1 || r.InventarioCompleto {
		t.Fatalf("falhas=%d inventário completo=%v", r.FilesFailed, r.InventarioCompleto)
	}
	if contar(em, api.OpRemoved)+contar(em, api.OpUnselected) != 0 {
		t.Fatalf("sessão parcial emitiu remoção: %+v", em)
	}
	// Destrancado, a sessão seguinte é completa e a remoção sai.
	if err := os.Chmod(trancado, 0o644); err != nil {
		t.Fatal(err)
	}
	r, em = m.rodar()
	if contar(em, api.OpRemoved) != 1 || contar(em, api.OpUpsert) != 1 || !r.InventarioCompleto {
		t.Fatalf("emitidas: %+v", em)
	}
	m.conferir()
}

// A pasta do estado nunca entra no backup, mesmo dentro de uma pasta do plano.
func TestPastaDoEstadoFicaForaDoBackup(t *testing.T) {
	m := novaMesa(t)
	m.dirEst = filepath.Join(m.raiz, "var", "lib", "arkame", "estado")
	m.semear(5)
	m.rodar()
	r, _ := m.rodar()
	if r.Stats.FilesTotal != 5 {
		t.Fatalf("o arquivo do estado entrou no backup: total=%d", r.Stats.FilesTotal)
	}
	for k := range m.presenca {
		if filepath.Ext(k) == ".db" {
			t.Fatalf("estado no índice: %s", k)
		}
	}
}

// O limite de banda é da execução, não de cada envio: com quatro
// trabalhadores, o total enviado por segundo segue dentro do limite.
func TestLimiteDeBandaEGlobal(t *testing.T) {
	m := novaMesa(t)
	bloco := make([]byte, 64<<10)
	for i := 0; i < 8; i++ { // 512 KiB
		m.escrever(filepath.Join("g", string(rune('a'+i))+".bin"), string(bloco)+string(rune('a'+i)))
	}
	m.semEstado = true
	inicio := time.Now()
	o := EngineOptions{
		S3: m.cli, Bucket: "b", PrefixRoot: prefixoDeTeste, HostRoot: "/",
		SourcePaths: []string{m.raiz}, Trabalhadores: 4, MaxMbps: 1, // 128 KiB/s, rajada de 1 s
	}
	r, err := Run(t.Context(), o)
	if err != nil || r.Stats.FilesUploaded != 8 {
		t.Fatalf("err=%v enviados=%d", err, r.Stats.FilesUploaded)
	}
	// 512 KiB a 128 KiB/s, menos 1 s de rajada: pelo menos 3 s. Com um
	// limitador por envio, os oito passariam na rajada de cada um.
	if d := time.Since(inicio); d < 2500*time.Millisecond {
		t.Fatalf("8 × 64 KiB a 1 Mbps levaram %v: o limite não é global", d)
	}
}

// No máximo dois multiparts ao mesmo tempo, com quatro trabalhadores.
func TestNoMaximoDoisMultiparts(t *testing.T) {
	antes := limiarDoMultipart
	limiarDoMultipart = 1024
	t.Cleanup(func() { limiarDoMultipart = antes })
	m := novaMesa(t)
	m.s3.Antes(func(op string, _ *http.Request) {
		if op == "UploadPart" {
			time.Sleep(30 * time.Millisecond)
		}
	})
	for i := 0; i < 8; i++ {
		m.escrever(string(rune('a'+i))+".bin", string(make([]byte, 2048))+string(rune('a'+i)))
	}
	r, _ := m.rodar()
	if r.Stats.FilesUploaded != 8 || m.s3.N("CompleteMultipartUpload") != 8 {
		t.Fatalf("enviados=%d s3=%v", r.Stats.FilesUploaded, m.s3.Requisicoes())
	}
	if p := m.s3.MaxMultiparts(); p != 2 {
		t.Fatalf("pico de multiparts simultâneos = %d, queria 2", p)
	}
	m.conferir()
}

// Sem Emitir (o /complete inline do painel antigo), o mapa de versões sai
// inteiro e na ordem da varredura, com um ou com quatro trabalhadores — e,
// com estado, sem reler o que não mudou.
func TestInlineSaiNaOrdemDaVarredura(t *testing.T) {
	m := novaMesa(t)
	m.semear(300)
	var ordem []string
	fileCh, errCh := walk(t.Context(), "/", []string{m.raiz}, nil, nil)
	for fi := range fileCh {
		ordem = append(ordem, prefixoDeTeste+fi.RelativePath)
	}
	<-errCh

	base, err := estado.Abrir(m.dirEst, estado.Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Fechar()
	plano, _ := base.Plano("p1", PrefixoDasChaves(prefixoDeTeste))
	for sessao, trab := range []int{4, 1, 4} {
		ex, _ := plano.Comecar("s")
		m.s3.Zerar()
		r, err := Run(t.Context(), EngineOptions{
			S3: m.cli, Bucket: "b", PrefixRoot: prefixoDeTeste, HostRoot: "/",
			SourcePaths: []string{m.raiz}, Trabalhadores: trab, Estado: ex, Delta: true,
			// Arquivo recém-criado é "data suspeita" e não serviria de atalho.
			Agora: func() time.Time { return m.agora },
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.VersionMap) != 300 {
			t.Fatalf("mapa com %d entradas", len(r.VersionMap))
		}
		for i, e := range r.VersionMap {
			atual, _ := m.s3.Atual(e.Key)
			if e.Key != ordem[i] || e.VersionID != atual.ID || e.SHA256 != atual.SHA256 {
				t.Fatalf("sessão %d: entrada %d = %+v; queria %s na versão %+v", sessao, i, e, ordem[i], atual)
			}
		}
		if sessao > 0 && len(m.s3.Requisicoes()) != 0 {
			t.Fatalf("inline com estado não devia consultar o bucket: %v", m.s3.Requisicoes())
		}
		_ = plano.Confirmar("s", r.Stats.FilesTotal, ex.Ger)
	}
}

// Versão trocada dentro do arquivo de estado, sem quebrar a estrutura dele: o
// atalho não pode levar ao índice uma versão que não é a do arquivo. O CRC da
// entrada acusa, o estado é largado e o resto da execução confere no bucket.
func TestVersaoAdulteradaNoEstadoNaoVaiAoIndice(t *testing.T) {
	m := novaMesa(t)
	m.trabalhad = 1
	m.semear(50)
	m.rodar()
	alvo := m.presenca[m.chave("p03/arquivo-0003.txt")].VersionID
	arq := m.ultimaBase.Caminho()
	bruto, _ := os.ReadFile(arq)
	n := 0
	for i := 0; i+len(alvo) <= len(bruto); i++ {
		if string(bruto[i:i+len(alvo)]) == alvo {
			copy(bruto[i:], "v00009999")
			n++
		}
	}
	if n == 0 || len(alvo) != len("v00009999") {
		t.Fatalf("não achei a versão %q no estado", alvo)
	}
	if err := os.WriteFile(arq, bruto, 0o600); err != nil {
		t.Fatal(err)
	}
	m.delta = false // inventário completo: toda entrada do atalho vai ao índice
	m.s3.Zerar()
	r, _ := m.rodar()
	if !r.EstadoFalhou {
		t.Fatal("o estado adulterado tinha de ser largado")
	}
	if m.s3.N("PutObject") != 0 {
		t.Fatalf("nada sobe de novo: %v", m.s3.Requisicoes())
	}
	m.conferir()
}
