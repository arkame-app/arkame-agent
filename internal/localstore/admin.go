package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Admin fala com a API de administração do RustFS (`/rustfs/admin/v3`, a
// mesma do MinIO, com o corpo em JSON) usando a credencial da cópia local.
// Serve para dar a outro servidor da rede uma chave que grava e lê no bucket,
// mas não apaga (`local-storage share`).
type Admin struct {
	// Endereco é o https://127.0.0.1:porta do RustFS.
	Endereco string
	Chave    string
	Segredo  string
	// Cliente com o certificado da cópia local fixado.
	Cliente *http.Client
}

var reUsuario = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)

// PoliticaDaRede é a política de um servidor da rede no bucket: só na pasta
// dele (`<prefixo>data/<agentID>/`, onde o agente grava), listar, ler (as
// versões também, para restaurar) e gravar. Nada de apagar objeto ou versão,
// mexer no versionamento ou no bucket, nem ler ou gravar na pasta de outro
// servidor: quem tomar um desses servidores não leva nem estraga os backups
// dos outros guardados aqui. Só o processo da cópia local (com a credencial
// do RustFS) expurga.
func PoliticaDaRede(bucket, prefixo, agentID string) ([]byte, error) {
	if !reAgentID.MatchString(agentID) {
		return nil, fmt.Errorf("id do agente inválido: %q", agentID)
	}
	pasta := prefixo + "data/" + agentID + "/"
	pol := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:GetBucketLocation", "s3:GetBucketVersioning"},
				"Resource": []string{"arn:aws:s3:::" + bucket},
			},
			{
				"Effect":    "Allow",
				"Action":    []string{"s3:ListBucket", "s3:ListBucketVersions"},
				"Resource":  []string{"arn:aws:s3:::" + bucket},
				"Condition": map[string]any{"StringLike": map[string]any{"s3:prefix": []string{pasta, pasta + "*"}}},
			},
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:PutObject", "s3:GetObject", "s3:GetObjectVersion", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"},
				"Resource": []string{"arn:aws:s3:::" + bucket + "/" + pasta + "*"},
			},
		},
	}
	return json.Marshal(pol)
}

var reAgentID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$|^[a-z0-9-]{3,64}$`)

// NomeDaPolitica é o nome da política de um usuário da rede no RustFS.
func NomeDaPolitica(usuario string) string { return "politica-" + usuario }

// DarAcessoDaRede cria a política do servidor cliente e o usuário dele, e
// liga os dois. O segredo vai só para o RustFS e para quem rodou o comando.
func (a *Admin) DarAcessoDaRede(ctx context.Context, bucket, prefixo, agentID, usuario, segredo string) error {
	if !reUsuario.MatchString(usuario) {
		return fmt.Errorf("nome de usuário inválido: %q", usuario)
	}
	if len(segredo) < 16 {
		return fmt.Errorf("o segredo precisa de 16 caracteres ou mais")
	}
	pol, err := PoliticaDaRede(bucket, prefixo, agentID)
	if err != nil {
		return err
	}
	if err := a.chamar(ctx, http.MethodPut, "add-canned-policy", url.Values{"name": {NomeDaPolitica(usuario)}}, pol); err != nil {
		return fmt.Errorf("criando a política do servidor: %w", err)
	}
	corpo, _ := json.Marshal(map[string]string{"secretKey": segredo, "status": "enabled"})
	if err := a.chamar(ctx, http.MethodPut, "add-user", url.Values{"accessKey": {usuario}}, corpo); err != nil {
		return fmt.Errorf("criando o usuário %s: %w", usuario, err)
	}
	q := url.Values{"policyName": {NomeDaPolitica(usuario)}, "userOrGroup": {usuario}, "isGroup": {"false"}}
	if err := a.chamar(ctx, http.MethodPut, "set-user-or-group-policy", q, nil); err != nil {
		return fmt.Errorf("ligando a política ao usuário %s: %w", usuario, err)
	}
	return nil
}

// RemoverUsuario tira o acesso de um servidor da rede.
func (a *Admin) RemoverUsuario(ctx context.Context, usuario string) error {
	if !reUsuario.MatchString(usuario) {
		return fmt.Errorf("nome de usuário inválido: %q", usuario)
	}
	if err := a.chamar(ctx, http.MethodDelete, "remove-user", url.Values{"accessKey": {usuario}}, nil); err != nil {
		return err
	}
	// A política era só dele; sem o usuário, não serve a mais ninguém.
	_ = a.chamar(ctx, http.MethodDelete, "remove-canned-policy", url.Values{"name": {NomeDaPolitica(usuario)}}, nil)
	return nil
}

// Usuarios lista os usuários do RustFS (só os nomes).
func (a *Admin) Usuarios(ctx context.Context) ([]string, error) {
	var lista map[string]json.RawMessage
	if err := a.chamarELer(ctx, http.MethodGet, "list-users", nil, nil, &lista); err != nil {
		return nil, err
	}
	nomes := make([]string, 0, len(lista))
	for n := range lista {
		nomes = append(nomes, n)
	}
	return nomes, nil
}

func (a *Admin) chamar(ctx context.Context, metodo, acao string, q url.Values, corpo []byte) error {
	return a.chamarELer(ctx, metodo, acao, q, corpo, nil)
}

func (a *Admin) chamarELer(ctx context.Context, metodo, acao string, q url.Values, corpo []byte, saida any) error {
	u := a.Endereco + "/rustfs/admin/v3/" + acao
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	c, cancelar := context.WithTimeout(ctx, 30*time.Second)
	defer cancelar()
	req, err := http.NewRequestWithContext(c, metodo, u, bytes.NewReader(corpo))
	if err != nil {
		return err
	}
	if corpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h := sha256.Sum256(corpo)
	hash := hex.EncodeToString(h[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	cred := aws.Credentials{AccessKeyID: a.Chave, SecretAccessKey: a.Segredo}
	if err := v4.NewSigner().SignHTTP(c, cred, req, hash, "s3", "us-east-1", time.Now()); err != nil {
		return err
	}
	res, err := a.Cliente.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("RustFS respondeu %d: %s", res.StatusCode, bytes.TrimSpace(b))
	}
	if saida != nil {
		return json.Unmarshal(b, saida)
	}
	return nil
}
