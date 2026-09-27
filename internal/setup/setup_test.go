package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGravar(t *testing.T) {
	caminho := filepath.Join(t.TempDir(), "arkame", "agent.env")
	regiao := "sa-saopaulo-1"
	p := &DoPainel{AgentID: "a1", Armazenamento: &Armazenamento{ID: "s1", Bucket: "b", Region: &regiao}}
	if err := Gravar(caminho, append(Linhas(p, "https://painel"), Chaves("AK1", "SK1")...)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(caminho)
	want := "AGENT_ID=a1\nPANEL_URL=https://painel\nSTORAGE_ID=s1\nSTORAGE_BUCKET=b\nSTORAGE_REGION=sa-saopaulo-1\nSTORAGE_ACCESS_KEY=AK1\nSTORAGE_SECRET_KEY=SK1\n"
	if string(b) != want {
		t.Fatalf("arquivo:\n%s\nesperado:\n%s", b, want)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(caminho); st.Mode().Perm() != 0o600 {
			t.Fatalf("permissão %v, esperado 0600", st.Mode().Perm())
		}
	}

	// Trocar as chaves preserva o resto, inclusive linha acrescentada à mão.
	if err := os.WriteFile(caminho, append(b, []byte("SIBLING_BUCKETS=x\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Gravar(caminho, Chaves("AK2", "SK2")); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(caminho)
	s := string(b)
	for _, l := range []string{"AGENT_ID=a1", "SIBLING_BUCKETS=x", "STORAGE_ACCESS_KEY=AK2", "STORAGE_SECRET_KEY=SK2"} {
		if !strings.Contains(s, l+"\n") {
			t.Fatalf("faltou %q em:\n%s", l, s)
		}
	}
	if strings.Contains(s, "AK1") || strings.Contains(s, "SK1") {
		t.Fatalf("chave antiga ficou:\n%s", s)
	}
	if _, err := os.Stat(caminho + ".novo"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("arquivo temporário ficou para trás")
	}
}

// Arquivo que já existia sem a chave (escrito à mão, com linhas próprias):
// gravar a configuração da instalação não apaga o que era dele.
func TestGravarPreservaOQueJaEstava(t *testing.T) {
	caminho := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(caminho, []byte("SIBLING_BUCKETS=outro\nHTTPS_PROXY=http://proxy:3128\nSTORAGE_BUCKET=velho\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Gravar(caminho, []string{"STORAGE_BUCKET=novo", "STORAGE_ACCESS_KEY=AK"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(caminho)
	want := "SIBLING_BUCKETS=outro\nHTTPS_PROXY=http://proxy:3128\nSTORAGE_BUCKET=novo\nSTORAGE_ACCESS_KEY=AK\n"
	if string(b) != want {
		t.Fatalf("arquivo:\n%s\nesperado:\n%s", b, want)
	}
}

func TestPodeGravar(t *testing.T) {
	if err := PodeGravar(filepath.Join(t.TempDir(), "sub", "agent.env")); err != nil {
		t.Fatalf("diretório próprio deveria poder: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root escreve em qualquer lugar")
	}
	somenteLeitura := t.TempDir()
	if err := os.Chmod(somenteLeitura, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(somenteLeitura, 0o700)
	if err := PodeGravar(filepath.Join(somenteLeitura, "agent.env")); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("sem permissão deveria avisar antes de perguntar a chave, deu %v", err)
	}
}

func TestBuscarNoPainel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var corpo map[string]string
		_ = json.NewDecoder(r.Body).Decode(&corpo)
		switch {
		case r.Method != http.MethodPost || r.URL.Path != "/api/agents/install-config":
			w.WriteHeader(http.StatusBadRequest)
		case corpo["token"] == "atk_bom":
			_, _ = w.Write([]byte(`{"agent_id":"a1","display_name":"Srv","panel_url":"https://p","storage":{"id":"s1","display_name":"AWS","bucket":"b","region":null,"endpoint":"https://e"}}`))
		case corpo["token"] == "atk_injecao":
			_, _ = w.Write([]byte(`{"agent_id":"a1","display_name":"Srv","storage":{"id":"s1","display_name":"x","bucket":"b","region":"us-east-1\nPANEL_URL=https://mau","endpoint":null}}`))
		case corpo["token"] == "atk_sem_armazenamento":
			_, _ = w.Write([]byte(`{"agent_id":"a1","display_name":"Srv","panel_url":"https://p","storage":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, err := BuscarNoPainel(context.Background(), srv.URL+"/", "atk_bom")
	if err != nil {
		t.Fatal(err)
	}
	if p.Armazenamento.Bucket != "b" || p.Armazenamento.Region != nil || *p.Armazenamento.Endpoint != "https://e" {
		t.Fatalf("resposta mal lida: %+v", p.Armazenamento)
	}
	// O painel gravado é o que a instalação usou (--panel-url), não o que o
	// painel diz de si: o serviço só lê o arquivo.
	if l := Linhas(p, "https://outro"); l[1] != "PANEL_URL=https://outro" || l[len(l)-1] != "STORAGE_ENDPOINT=https://e" {
		t.Fatalf("linhas: %v", l)
	}
	if _, err := BuscarNoPainel(context.Background(), srv.URL, "atk_velho"); !errors.Is(err, ErrCodigoInvalido) {
		t.Fatalf("código vencido deveria dar ErrCodigoInvalido, deu %v", err)
	}
	if _, err := BuscarNoPainel(context.Background(), srv.URL, "atk_injecao"); err == nil || !strings.Contains(err.Error(), "quebra de linha") {
		t.Fatalf("valor com quebra de linha deveria ser recusado, deu %v", err)
	}
	if _, err := BuscarNoPainel(context.Background(), srv.URL, "atk_sem_armazenamento"); err == nil || !strings.Contains(err.Error(), "armazenamento") {
		t.Fatalf("sem armazenamento deveria explicar, deu %v", err)
	}
}
