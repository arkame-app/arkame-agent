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

func TestGravarETrocarChaves(t *testing.T) {
	caminho := filepath.Join(t.TempDir(), "arkame", "agent.env")
	regiao := "sa-saopaulo-1"
	p := &DoPainel{AgentID: "a1", Armazenamento: &Armazenamento{ID: "s1", Bucket: "b", Region: &regiao}}
	if err := Gravar(caminho, Linhas(p, "https://painel"), "AK1", "SK1"); err != nil {
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
	if err := TrocarChaves(caminho, "AK2", "SK2"); err != nil {
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

func TestBuscarNoPainel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var corpo map[string]string
		_ = json.NewDecoder(r.Body).Decode(&corpo)
		switch {
		case r.Method != http.MethodPost || r.URL.Path != "/api/agents/install-config":
			w.WriteHeader(http.StatusBadRequest)
		case corpo["token"] == "atk_bom":
			_, _ = w.Write([]byte(`{"agent_id":"a1","display_name":"Srv","panel_url":"https://p","storage":{"id":"s1","display_name":"AWS","bucket":"b","region":null,"endpoint":"https://e"}}`))
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
	if l := Linhas(p, "https://outro"); l[1] != "PANEL_URL=https://p" || l[len(l)-1] != "STORAGE_ENDPOINT=https://e" {
		t.Fatalf("linhas: %v", l)
	}
	if _, err := BuscarNoPainel(context.Background(), srv.URL, "atk_velho"); !errors.Is(err, ErrCodigoInvalido) {
		t.Fatalf("código vencido deveria dar ErrCodigoInvalido, deu %v", err)
	}
	if _, err := BuscarNoPainel(context.Background(), srv.URL, "atk_sem_armazenamento"); err == nil || !strings.Contains(err.Error(), "armazenamento") {
		t.Fatalf("sem armazenamento deveria explicar, deu %v", err)
	}
}
