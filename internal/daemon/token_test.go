package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/service"
)

func jwtQueVenceEm(t time.Time) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":"a1","exp":%d}`, t.Unix())))
	return h + "." + p + ".assinatura"
}

// painelDeHeartbeat responde o heartbeat com o corpo dado e guarda o
// Authorization de cada chamada.
type painelDeHeartbeat struct {
	mu    gosync.Mutex
	auths []string
	corpo string
}

func (p *painelDeHeartbeat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	p.mu.Lock()
	p.auths = append(p.auths, r.Header.Get("Authorization"))
	corpo := p.corpo
	p.mu.Unlock()
	_, _ = io.WriteString(w, corpo)
}

func capturarLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	antes := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(antes) })
	return &buf
}

// O painel manda um token novo no heartbeat: o agente grava (protegido) e
// usa na hora.
func TestHeartbeatAplicaTokenNovo(t *testing.T) {
	antigo := jwtQueVenceEm(time.Now().Add(60 * 24 * time.Hour))
	novo := jwtQueVenceEm(time.Now().Add(365 * 24 * time.Hour))
	tokenPath := filepath.Join(t.TempDir(), "token.jwt")
	if err := os.WriteFile(tokenPath, []byte(antigo), 0o600); err != nil {
		t.Fatal(err)
	}

	painel := &painelDeHeartbeat{corpo: `{"ok":true,"new_token":"` + novo + `"}`}
	srv := httptest.NewServer(painel)
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: antigo})
	cfg := &config.Config{AgentID: "a1", TokenPath: tokenPath}
	estado := &estadoDoToken{}

	enviarHeartbeat(context.Background(), c, cfg, service.Detected{}, estado)
	painel.mu.Lock()
	painel.corpo = `{"ok":true}`
	painel.mu.Unlock()
	enviarHeartbeat(context.Background(), c, cfg, service.Detected{}, estado)
	// Clients derivados (o /complete usa ComPrazo) também passam a usá-lo.
	_ = c.ComPrazo(time.Second).POST(context.Background(), "/x", struct{}{}, nil)

	if got := painel.auths; len(got) != 3 || got[0] != "Bearer "+antigo || got[1] != "Bearer "+novo || got[2] != "Bearer "+novo {
		t.Fatalf("Authorization por chamada: %v", got)
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil || string(b) != novo {
		t.Fatalf("o token novo não foi gravado em %s: %q %v", tokenPath, b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(tokenPath); st.Mode().Perm() != 0o600 {
			t.Fatalf("token gravado com modo %v, queria 0600", st.Mode().Perm())
		}
	}
	if _, err := os.Stat(tokenPath + ".novo"); !os.IsNotExist(err) {
		t.Fatal("ficou o temporário da troca atômica")
	}
}

// Sem new_token (painel antigo, ou token longe de vencer): nada muda.
func TestHeartbeatSemTokenNovoNaoMuda(t *testing.T) {
	atual := jwtQueVenceEm(time.Now().Add(300 * 24 * time.Hour))
	tokenPath := filepath.Join(t.TempDir(), "token.jwt")
	if err := os.WriteFile(tokenPath, []byte(atual), 0o600); err != nil {
		t.Fatal(err)
	}
	painel := &painelDeHeartbeat{corpo: `{"ok":true,"server_time":"x"}`}
	srv := httptest.NewServer(painel)
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: atual})
	log := capturarLog(t)
	enviarHeartbeat(context.Background(), c, &config.Config{AgentID: "a1", TokenPath: tokenPath}, service.Detected{}, &estadoDoToken{})

	if c.Bearer() != atual {
		t.Fatal("o token em uso mudou sem new_token")
	}
	if b, _ := os.ReadFile(tokenPath); string(b) != atual {
		t.Fatal("o arquivo do token mudou sem new_token")
	}
	if strings.Contains(log.String(), "level=WARN") {
		t.Fatalf("aviso sem motivo: %s", log.String())
	}
}

// Token a menos de 30 dias do vencimento e nenhum novo: avisa, uma vez por dia.
func TestTokenPertoDeVencerAvisa(t *testing.T) {
	agora := time.Now()
	atual := jwtQueVenceEm(agora.Add(10 * 24 * time.Hour))
	painel := &painelDeHeartbeat{corpo: `{"ok":true}`}
	srv := httptest.NewServer(painel)
	t.Cleanup(srv.Close)
	c, _ := api.New(api.Options{BaseURL: srv.URL, Bearer: atual})
	cfg := &config.Config{AgentID: "a1", TokenPath: filepath.Join(t.TempDir(), "token.jwt")}
	log := capturarLog(t)
	estado := &estadoDoToken{agora: func() time.Time { return agora }}

	enviarHeartbeat(context.Background(), c, cfg, service.Detected{}, estado)
	enviarHeartbeat(context.Background(), c, cfg, service.Detected{}, estado)
	if n := strings.Count(log.String(), "vence em breve"); n != 1 {
		t.Fatalf("esperava um aviso, vieram %d:\n%s", n, log.String())
	}
	agora = agora.Add(25 * time.Hour)
	enviarHeartbeat(context.Background(), c, cfg, service.Detected{}, estado)
	if n := strings.Count(log.String(), "vence em breve"); n != 2 {
		t.Fatalf("esperava o aviso de novo no dia seguinte, vieram %d", n)
	}
}

func TestVencimentoDoJWT(t *testing.T) {
	quando := time.Unix(1893456000, 0)
	if v, ok := vencimentoDoJWT(jwtQueVenceEm(quando)); !ok || !v.Equal(quando) {
		t.Fatalf("exp lido %v %v", v, ok)
	}
	for _, ruim := range []string{"", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		if _, ok := vencimentoDoJWT(ruim); ok {
			t.Errorf("%q não deveria ter exp", ruim)
		}
	}
}
