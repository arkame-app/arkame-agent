package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Com HTTP(S)_PROXY definido, o agente fala com o painel pelo proxy. O
// transporte próprio não tinha Proxy e ignorava a variável.
//
// O http.ProxyFromEnvironment lê o ambiente uma vez por processo: este é o
// único teste do pacote que passa por ele.
func TestClientUsaProxyDoAmbiente(t *testing.T) {
	var pedido string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		pedido = r.Method + " " + r.URL.String()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	c, _ := New(Options{BaseURL: "http://painel.arkame.invalid"})
	if err := c.POST(context.Background(), "/api/agents/a1/heartbeat", struct{}{}, nil); err != nil {
		t.Fatalf("o pedido não passou pelo proxy: %v", err)
	}
	if pedido != "POST http://painel.arkame.invalid/api/agents/a1/heartbeat" {
		t.Fatalf("o proxy recebeu %q", pedido)
	}
}
