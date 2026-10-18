//go:build !windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
)

// Plano com as pastas vindas do MCP e uma origem que é um link para /etc: o
// backup falha com restricted_path antes de rodar o comando de antes e de
// falar com o bucket. Sem a marca, o mesmo plano (feito por uma pessoa) segue.
func TestOrigemSensivelDoMCPRecusadaPeloCaminhoReal(t *testing.T) {
	painel, c := novoPainel(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "inocente")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	marca := filepath.Join(t.TempDir(), "rodou")
	s3c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:1"), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""), Retryer: aws.NopRetryer{}})
	plan := api.Plan{ID: "p1", Kind: "backup", SourcePaths: []string{link}, StorageRef: api.StorageRef{Bucket: "b"},
		PreHook: "echo x > " + marca, RestrictedPaths: true}
	err := executePlan(context.Background(), c, s3c, &config.Config{AgentID: "a1", HostRoot: "/"}, plan)
	if err == nil || !strings.Contains(err.Error(), "restricted_path") {
		t.Fatalf("erro = %v", err)
	}
	if painel.recebeu("/sessions/s1/fail") != 1 || !strings.Contains(painel.corpo("/api/agents/a1/sessions/s1/fail"), "restricted_path") {
		t.Fatalf("o painel não recebeu a falha restricted_path: %v", painel.chamadas)
	}
	if _, err := os.Stat(marca); err == nil {
		t.Fatal("o comando de antes rodou com a origem recusada")
	}
}
