package daemon

import (
	"context"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Backup e limpeza de retenção do mesmo bucket não se cruzam: o backup pode
// reaproveitar (dedup) justamente a versão que a limpeza apaga, e a sessão
// gravaria no catálogo uma versão que já saiu do bucket.

func cfgDaExclusao() *config.Config {
	return &config.Config{AgentID: "a1", HostRoot: "/", StorageID: "st1", StorageBucket: "b"}
}

// portao segura uma resposta do painel falso até ser aberto. Abre sozinho no
// fim do teste: com o teste falhando antes, o handler preso impediria o
// fechamento do servidor.
func portao(t *testing.T) (abrir func(), esperar <-chan struct{}) {
	ch := make(chan struct{})
	var uma gosync.Once
	abrir = func() { uma.Do(func() { close(ch) }) }
	t.Cleanup(abrir)
	return abrir, ch
}

func planoDaExclusao(t *testing.T) api.Plan {
	return api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{t.TempDir()},
		StorageRef: api.StorageRef{Bucket: "b"}}
}

// Com sessão de backup em curso, a limpeza não roda — nem pergunta ao painel,
// porque perguntar é o que emite a rodada. Terminado o backup, ela roda.
func TestLimpezaNaoRodaComBackupEmCurso(t *testing.T) {
	painel, c := novoPainel(t)
	entrou := make(chan struct{})
	abrir, solta := portao(t)
	painel.resposta = func(path string, n int) (int, string) {
		if strings.HasSuffix(path, "/sessions/start") && n == 1 {
			close(entrou)
			<-solta
		}
		return 0, ""
	}
	cfg := cfgDaExclusao()
	s3c := s3SemUso(t)
	trava := &exclusaoDoBucket{}

	fim := make(chan error, 1)
	go func() { fim <- executarPlanoExclusivo(context.Background(), c, s3c, cfg, planoDaExclusao(t), trava) }()

	select {
	case <-entrou:
	case <-time.After(5 * time.Second):
		t.Fatal("o backup não abriu a sessão")
	}
	if rodadaDeExpurgo(context.Background(), c, s3c, cfg, trava) {
		t.Fatal("a limpeza rodou com sessão de backup em curso")
	}
	if n := painel.recebeu("/purge-plan"); n != 0 {
		t.Fatalf("a limpeza perguntou ao painel com backup em curso; chamadas: %v", painel.chamadas)
	}

	abrir()
	if err := <-fim; err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !rodadaDeExpurgo(context.Background(), c, s3c, cfg, trava) {
		t.Fatal("a limpeza continuou adiada depois do fim do backup")
	}
	if n := painel.recebeu("/purge-plan"); n != 1 {
		t.Fatalf("a limpeza não perguntou ao painel depois do backup; chamadas: %v", painel.chamadas)
	}
}

// Com limpeza em curso, o backup espera ela terminar para abrir a sessão — e
// não é pulado.
func TestBackupEsperaALimpezaEmCurso(t *testing.T) {
	painel, c := novoPainel(t)
	entrou := make(chan struct{})
	abrir, solta := portao(t)
	painel.resposta = func(path string, n int) (int, string) {
		if strings.HasSuffix(path, "/purge-plan") && n == 1 {
			close(entrou)
			<-solta
		}
		return 0, ""
	}
	cfg := cfgDaExclusao()
	s3c := s3SemUso(t)
	trava := &exclusaoDoBucket{}

	limpou := make(chan bool, 1)
	go func() { limpou <- rodadaDeExpurgo(context.Background(), c, s3c, cfg, trava) }()
	select {
	case <-entrou:
	case <-time.After(5 * time.Second):
		t.Fatal("a limpeza não perguntou ao painel")
	}

	fim := make(chan error, 1)
	go func() { fim <- executarPlanoExclusivo(context.Background(), c, s3c, cfg, planoDaExclusao(t), trava) }()

	time.Sleep(200 * time.Millisecond)
	if n := painel.recebeu("/sessions/start"); n != 0 {
		t.Fatalf("o backup abriu sessão com a limpeza em curso; chamadas: %v", painel.chamadas)
	}

	abrir()
	if !<-limpou {
		t.Fatal("a limpeza se deu por adiada sem backup em curso")
	}
	if err := <-fim; err != nil {
		t.Fatalf("backup: %v", err)
	}
	if n := painel.recebeu("/sessions/start"); n != 1 {
		t.Fatalf("o backup não rodou depois da limpeza; chamadas: %v", painel.chamadas)
	}
}
