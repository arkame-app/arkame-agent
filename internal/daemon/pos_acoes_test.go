//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// A importação do Zimbra pedida pela tela (17/10): o agente importa só de
// dentro da pasta das restaurações, apaga a cópia depois de tentar (importada
// ou não), não apaga o que recusou, e relata cada uma ao painel. Pede as de
// todos os armazenamentos: a do plano na cópia local também é do principal.
func TestPosAcoesDoZimbra(t *testing.T) {
	base := filepath.Join(t.TempDir(), nomeDaPastaDeRestauracao)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	soAPropria(t)

	pasta := func(job, conta string) string {
		p := filepath.Join(base, job, conta)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	boa := pasta("j1", "maria@empresa.com.br")
	ruim := pasta("j3", "jose@empresa.com.br")
	tipoErrado := pasta("j4", "ana@empresa.com.br")
	// Pasta que existe, com o nome certo, mas fora da pasta das restaurações.
	fora := filepath.Join(t.TempDir(), "x", "maria@empresa.com.br")
	if err := os.MkdirAll(fora, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pasta das restaurações aberta para outros usuários: recusada, e nada apagado.
	aberta := filepath.Join(t.TempDir(), nomeDaPastaDeRestauracao)
	if err := os.MkdirAll(filepath.Join(aberta, "j5", "rui@empresa.com.br"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(aberta, 0o777); err != nil {
		t.Fatal(err)
	}
	daAberta := filepath.Join(aberta, "j5", "rui@empresa.com.br")
	acoes := []posAcao{
		{JobID: "j5", Type: "zimbra_restore", Path: daAberta, Account: "rui@empresa.com.br"},
		{JobID: "j1", Type: "zimbra_restore", Path: boa, Account: "maria@empresa.com.br"},
		{JobID: "j2", Type: "zimbra_restore", Path: fora, Account: "maria@empresa.com.br"},
		{JobID: "j3", Type: "zimbra_restore", Path: ruim, Account: "jose@empresa.com.br"},
		{JobID: "j4", Type: "apagar_tudo", Path: tipoErrado, Account: "ana@empresa.com.br"},
	}
	var mu sync.Mutex
	relatos := map[string]map[string]any{}
	var consulta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			consulta = r.URL.String()
			_ = json.NewEncoder(w).Encode(map[string]any{"actions": acoes})
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			mu.Lock()
			relatos[filepath.Base(r.URL.Path)] = m
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	cfg := &config.Config{AgentID: "ag", StorageID: "st"}

	var importadas []string
	importar := func(_ context.Context, a posAcao, log *bytes.Buffer) error {
		importadas = append(importadas, a.JobID)
		fmt.Fprintln(log, "importando", a.Account)
		if a.JobID == "j3" {
			return errors.New("o Zimbra recusou")
		}
		return nil
	}
	processarPosAcoes(context.Background(), c, cfg, nil, importar)

	if consulta != "/api/agents/ag/post-actions" {
		t.Fatalf("consulta = %q", consulta)
	}
	if strings.Join(importadas, ",") != "j1,j3" {
		t.Fatalf("importou %v: só as de dentro da pasta, do tipo certo", importadas)
	}
	for job, ok := range map[string]bool{"j1": true, "j2": false, "j3": false, "j4": false, "j5": false} {
		r := relatos[job]
		if r == nil || r["ok"] != ok {
			t.Fatalf("relato de %s = %v, quer ok=%v", job, r, ok)
		}
	}
	if _, err := os.Stat(filepath.Dir(boa)); !os.IsNotExist(err) {
		t.Fatal("a cópia da conta importada ficou no disco")
	}
	if _, err := os.Stat(filepath.Dir(ruim)); !os.IsNotExist(err) {
		t.Fatal("a cópia da importação que falhou ficou no disco")
	}
	if _, err := os.Stat(tipoErrado); err != nil {
		t.Fatal("a pasta da pós-ação recusada foi apagada")
	}
	if _, err := os.Stat(fora); err != nil {
		t.Fatal("a pasta de fora foi apagada")
	}
	if _, err := os.Stat(daAberta); err != nil {
		t.Fatal("a pasta da restauração numa base aberta foi apagada")
	}
	if !strings.Contains(fmt.Sprint(relatos["j1"]["log"]), "importando") {
		t.Fatal("o log não foi ao painel")
	}
}

// O serviço parado no meio da importação: a pasta fica e nada é relatado, para
// o próximo processo encontrar a pendência e importar de novo.
func TestPosAcaoInterrompidaNaoApagaNemRelata(t *testing.T) {
	base := filepath.Join(t.TempDir(), nomeDaPastaDeRestauracao)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	soAPropria(t)
	p := filepath.Join(base, "j1", "maria@empresa.com.br")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	var relatos int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			relatos++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"actions": []posAcao{{JobID: "j1", Type: "zimbra_restore", Path: p, Account: "maria@empresa.com.br"}}})
	}))
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: "t"})
	ctx, cancelar := context.WithCancel(context.Background())
	processarPosAcoes(ctx, c, &config.Config{AgentID: "ag"}, nil, func(context.Context, posAcao, *bytes.Buffer) error {
		cancelar()
		return context.Canceled
	})
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a pasta da importação interrompida foi apagada")
	}
	if relatos != 0 {
		t.Fatalf("relatou %d vez(es) a importação interrompida", relatos)
	}
}

// soAPropria: o teste roda em /tmp (gravável por todos), que a conferência
// das pastas de cima recusa — e ela tem o próprio teste. Aqui, só a base.
func soAPropria(t *testing.T) {
	t.Helper()
	antes := pastaFechada
	pastaFechada = func(dir string) error { return donaEFechada(dir, true) }
	t.Cleanup(func() { pastaFechada = antes })
}

// As pastas de cima da base também: uma gravável por outros (como o /tmp)
// deixaria trocar a base por um link durante a importação (revisão, 17/10).
func TestPastaFechadaConfereAsDeCima(t *testing.T) {
	base := filepath.Join(t.TempDir(), nomeDaPastaDeRestauracao)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pastaFechada(base); err == nil {
		t.Fatal("aceitou uma base dentro do /tmp, que todos gravam")
	}
	if err := donaEFechada(base, true); err != nil {
		t.Fatalf("a própria base está certa: %v", err)
	}
}
