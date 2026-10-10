package sync

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/estado"
)

// filhoDoKill9 é o processo que vai morrer: roda uma sessão de backup de
// verdade (com o estado gravado em blocos pequenos) até o pai o matar.
func filhoDoKill9() {
	blocoDoEstado = 4
	base, err := estado.Abrir(os.Getenv("ARKAME_T_ESTADO"), estado.Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "b"})
	if err != nil {
		os.Exit(3)
	}
	plano, err := base.Plano("p1", PrefixoDasChaves(prefixoDeTeste))
	if err != nil {
		os.Exit(3)
	}
	ex, err := plano.Comecar("s2")
	if err != nil {
		os.Exit(3)
	}
	_, _ = Run(context.Background(), EngineOptions{
		S3: bancada.ClienteS3(os.Getenv("ARKAME_T_S3")), Bucket: "b", PrefixRoot: prefixoDeTeste, HostRoot: "/",
		SourcePaths: []string{os.Getenv("ARKAME_T_RAIZ")}, Trabalhadores: 2, Estado: ex, Delta: true,
		Emitir: func(api.IndexEntry) error { return nil },
		Agora:  func() time.Time { return time.Now().Add(time.Hour) },
	})
	os.Exit(4) // não devia chegar aqui: o pai mata antes
}

// `kill -9` no meio do backup: o que a sessão morta gravou no estado não foi
// confirmado pelo painel e não vale. A execução seguinte confere esses
// arquivos no bucket (sem reenviar o que já subiu) e os leva ao índice — se
// valessem, o atalho os pularia e o painel ficaria com a versão antiga.
func TestKill9NoMeioAsPendentesNaoValem(t *testing.T) {
	if os.Getenv("ARKAME_T_FILHO") == "kill9" {
		filhoDoKill9()
		return
	}
	m := novaMesa(t)
	m.semear(120)
	m.rodar() // s1, confirmada
	for i := 0; i < 60; i++ {
		m.escrever(filepath.Join("p0"+string(rune('0'+i%7)), "arquivo-"+pad4(i)+".txt"), "conteúdo novo "+pad4(i))
	}
	m.s3.Zerar()

	var puts atomic.Int64
	var presa atomic.Bool
	presa.Store(true)
	var uma gosync.Once
	chegou := make(chan struct{})
	solta := make(chan struct{})
	m.s3.Antes(func(op string, r *http.Request) {
		if !presa.Load() {
			return
		}
		if op == "PutObject" && puts.Add(1) > 20 {
			uma.Do(func() { close(chegou) })
			select {
			case <-solta:
			case <-r.Context().Done():
			}
		}
	})
	defer close(solta)
	srvURL := *m.cli.Options().BaseEndpoint
	cmd := exec.Command(os.Args[0], "-test.run=^TestKill9NoMeioAsPendentesNaoValem$")
	cmd.Env = append(os.Environ(), "ARKAME_T_FILHO=kill9", "ARKAME_T_ESTADO="+m.dirEst, "ARKAME_T_RAIZ="+m.raiz, "ARKAME_T_S3="+srvURL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-chegou:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("o processo filho não chegou ao meio do envio")
	}
	time.Sleep(100 * time.Millisecond)         // deixa o estado gravar alguns blocos
	if err := cmd.Process.Kill(); err != nil { // SIGKILL, pelo PID
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() == 4 {
		t.Fatal("o filho terminou o backup antes de morrer; o teste não testou nada")
	}
	enviadosPeloMorto := m.s3.N("PutObject") - 2 // os dois presos na porta não chegaram a gravar
	if enviadosPeloMorto < 15 {
		t.Fatalf("o morto devia ter enviado ~20 arquivos; enviou %d", enviadosPeloMorto)
	}

	// O estado tem entradas da geração morta, gravadas e não confirmadas.
	base, err := estado.Abrir(m.dirEst, estado.Identidade{Agente: "ag1", Armazenamento: "st1", Bucket: "b"})
	if err != nil || base.Motivo != "" {
		t.Fatalf("o estado devia abrir inteiro depois do kill -9: motivo=%q err=%v", base.Motivo, err)
	}
	plano, _ := base.Plano("p1", PrefixoDasChaves(prefixoDeTeste))
	if _, s := plano.Pendente(); s != "s2" || plano.UltimaSessao() != "s1" {
		t.Fatalf("pendente=%q última=%q", s, plano.UltimaSessao())
	}
	_ = base.Fechar()

	presa.Store(false)
	m.s3.Zerar()
	m.sessao = 2
	r, em := m.rodar() // s3
	if contar(em, api.OpUpsert) != 60 {
		t.Fatalf("os 60 alterados têm de ir ao índice, inclusive os que o morto já tinha enviado; foram %d", contar(em, api.OpUpsert))
	}
	if m.s3.N("HeadObject") != 60 || r.Atalhos != 60 {
		t.Fatalf("os 60 alterados são conferidos no bucket, os 60 intactos seguem no atalho: s3=%v atalhos=%d", m.s3.Requisicoes(), r.Atalhos)
	}
	if p := m.s3.N("PutObject"); p > 60-15 || r.Stats.FilesUploaded != p {
		t.Fatalf("o que o morto enviou não sobe de novo: PutObject=%d enviados=%d", p, r.Stats.FilesUploaded)
	}
	m.conferir()
}

func pad4(i int) string {
	s := "0000" + itoa(i)
	return s[len(s)-4:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// Data de modificação a menos de 2 s da leitura: uma alteração logo depois
// pode cair na mesma data. A entrada não serve de atalho na execução
// seguinte — o arquivo é relido.
func TestMtimeSuspeitoNaoServeDeAtalho(t *testing.T) {
	semCtime(t)
	m := novaMesa(t)
	m.semear(10)
	m.agora = time.Now().Add(time.Second) // o backup roda logo depois de os arquivos serem gravados
	m.rodar()
	if err := bancada.MudarSemTocar(filepath.Join(m.raiz, "p03/arquivo-0003.txt")); err != nil {
		t.Fatal(err)
	}
	m.agora = m.agora.Add(time.Hour)
	m.s3.Zerar()
	r, em := m.rodar()
	if r.Atalhos != 0 || m.s3.N("HeadObject") != 10 {
		t.Fatalf("entradas de data suspeita foram usadas como atalho: atalhos=%d s3=%v", r.Atalhos, m.s3.Requisicoes())
	}
	if r.Stats.FilesUploaded != 1 || contar(em, api.OpUpsert) != 1 {
		t.Fatalf("a alteração na mesma data devia ser pega: enviados=%d emitidas=%+v", r.Stats.FilesUploaded, em)
	}
	// Relidos longe da data de modificação, deixam de ser suspeitos.
	r, _ = m.rodar()
	if r.Atalhos != 10 {
		t.Fatalf("atalhos=%d", r.Atalhos)
	}
	m.conferir()
}
