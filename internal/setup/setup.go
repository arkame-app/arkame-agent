// Package setup prepara a credencial do bucket antes do registro.
//
// A instalação era em dois passos: um comando gravava o arquivo com a chave do
// bucket, outro instalava o agente. Ninguém conferia a chave — num Windows real
// (27/09) ela foi digitada errada e nada avisou. Agora o `install` pergunta a
// chave, testa no bucket e só registra o servidor quando o bucket aceita.
//
// O painel nunca recebe a chave: ele só diz qual bucket (pelo código de
// instalação), e o teste acontece daqui.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/arkame-app/agent/internal/terminal"
)

// Armazenamento é o que o painel diz do bucket, pelo código de instalação.
type Armazenamento struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Bucket      string  `json:"bucket"`
	Region      *string `json:"region"`
	Endpoint    *string `json:"endpoint"`
}

// DoPainel é a resposta de POST /api/agents/install-config.
type DoPainel struct {
	AgentID       string         `json:"agent_id"`
	DisplayName   string         `json:"display_name"`
	PanelURL      string         `json:"panel_url"`
	Armazenamento *Armazenamento `json:"storage"`
}

// ErrCodigoInvalido: código expirado, já usado ou digitado errado.
var ErrCodigoInvalido = errors.New("o painel não reconheceu o código de instalação: ele expira em 24 horas e vale uma vez. Gere outro no painel, em Servidores → Novo servidor")

// BuscarNoPainel pergunta ao painel qual bucket este servidor vai usar.
func BuscarNoPainel(ctx context.Context, panelURL, codigo string) (*DoPainel, error) {
	c, err := api.New(api.Options{BaseURL: strings.TrimRight(panelURL, "/"), Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	var r DoPainel
	if err := c.POST(ctx, "/api/agents/install-config", map[string]string{"token": codigo}, &r); err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 410") {
			return nil, ErrCodigoInvalido
		}
		return nil, fmt.Errorf("consultando o painel em %s: %w", panelURL, err)
	}
	if r.Armazenamento == nil {
		return nil, errors.New("o painel não sabe qual armazenamento este servidor vai usar: gere o comando pela tela Novo servidor, escolhendo o armazenamento")
	}
	return &r, nil
}

// Linhas monta o arquivo de configuração, sem as chaves.
func Linhas(p *DoPainel, panelURL string) []string {
	a := p.Armazenamento
	l := []string{
		"AGENT_ID=" + p.AgentID,
		"PANEL_URL=" + firstNonEmpty(p.PanelURL, panelURL),
		"STORAGE_ID=" + a.ID,
		"STORAGE_BUCKET=" + a.Bucket,
	}
	if a.Region != nil && *a.Region != "" {
		l = append(l, "STORAGE_REGION="+*a.Region)
	}
	if a.Endpoint != nil && *a.Endpoint != "" {
		l = append(l, "STORAGE_ENDPOINT="+*a.Endpoint)
	}
	return l
}

// MaxTentativas antes de desistir: quem errou cinco vezes precisa conferir a
// chave no provedor, não digitar de novo.
const MaxTentativas = 5

// PerguntarETestar pede a chave e a senha até o bucket aceitar. Devolve as
// duas quando o teste passa.
func PerguntarETestar(ctx context.Context, t *terminal.Terminal, base *config.Config, out io.Writer) (string, string, error) {
	fmt.Fprintf(out, "\n  Credencial do bucket %s (fica só nesta máquina; o painel não a recebe)\n\n", base.StorageBucket)
	for tentativa := 1; tentativa <= MaxTentativas; tentativa++ {
		ak, err := t.Pergunta("Chave de acesso (access key)")
		if err != nil {
			return "", "", err
		}
		sk, err := t.Segredo("Senha da chave (secret key)")
		if err != nil {
			return "", "", err
		}
		if ak == "" || sk == "" {
			fmt.Fprintln(out, "  ✗ as duas são obrigatórias.")
			continue
		}
		c := *base
		c.StorageAccessKey, c.StorageSecretKey = ak, sk
		fmt.Fprintln(out, "  Testando no bucket…")
		if err := storage.Check(ctx, &c); err != nil {
			fmt.Fprintf(out, "  ✗ %s.\n    (%s)\n\n", storage.Causa(err), resumo(err))
			if tentativa < MaxTentativas {
				fmt.Fprintln(out, "  Digite de novo:")
			}
			continue
		}
		fmt.Fprintln(out, "  ✓ O bucket aceitou a chave.")
		return ak, sk, nil
	}
	return "", "", fmt.Errorf("o bucket recusou a chave %d vezes: confira a chave no provedor e rode o comando de novo", MaxTentativas)
}

// Gravar escreve o arquivo com as linhas e as chaves, legível só pelo
// administrador (0600; no Windows, Administradores e SYSTEM).
func Gravar(caminho string, linhas []string, ak, sk string) error {
	if err := os.MkdirAll(filepath.Dir(caminho), 0o700); err != nil {
		return fmt.Errorf("criando %s: %w", filepath.Dir(caminho), err)
	}
	conteudo := strings.Join(append(append([]string{}, linhas...),
		"STORAGE_ACCESS_KEY="+ak,
		"STORAGE_SECRET_KEY="+sk,
	), "\n") + "\n"
	tmp := caminho + ".novo"
	if err := os.WriteFile(tmp, []byte(conteudo), 0o600); err != nil {
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	if err := protegerArquivo(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, caminho); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gravando %s: %w", caminho, err)
	}
	return nil
}

// TrocarChaves regrava só as duas chaves, preservando o resto do arquivo.
func TrocarChaves(caminho, ak, sk string) error {
	b, err := os.ReadFile(caminho)
	if err != nil {
		return fmt.Errorf("lendo %s: %w", caminho, err)
	}
	var linhas []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(l), "=")
		if l == "" || k == "STORAGE_ACCESS_KEY" || k == "STORAGE_SECRET_KEY" {
			continue
		}
		linhas = append(linhas, l)
	}
	return Gravar(caminho, linhas, ak, sk)
}

// resumo encurta a mensagem do SDK: a primeira linha basta ao suporte.
func resumo(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

func firstNonEmpty(vv ...string) string {
	for _, v := range vv {
		if v != "" {
			return v
		}
	}
	return ""
}
