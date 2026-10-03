package daemon

import (
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Dois processos do mesmo agente (um por credencial): cada um roda só os
// planos do seu bucket. Antes, os dois rodavam todos.
func TestPlanoDeOutroProcesso(t *testing.T) {
	cfg := &config.Config{StorageBucket: "aws-b", SiblingBuckets: []string{"oci-b"}}
	plano := func(b string) api.Plan { return api.Plan{StorageRef: api.StorageRef{Bucket: b}} }

	if !planoDeOutroProcesso(cfg, plano("oci-b")) {
		t.Fatal("plano do bucket do irmão deveria ser pulado")
	}
	if planoDeOutroProcesso(cfg, plano("aws-b")) {
		t.Fatal("plano do próprio bucket roda")
	}
	if planoDeOutroProcesso(cfg, plano("desconhecido")) {
		t.Fatal("bucket fora dos irmãos segue como antes (roda e falha com erro visível)")
	}
	if planoDeOutroProcesso(&config.Config{}, plano("oci-b")) {
		t.Fatal("sem bucket configurado, nada é filtrado")
	}
}
