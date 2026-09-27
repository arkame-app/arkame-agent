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
		var h *api.HTTPError
		if errors.Is(err, api.ErrGone) || (errors.As(err, &h) && h.Status == 404) {
			return nil, ErrCodigoInvalido
		}
		return nil, fmt.Errorf("consultando o painel em %s: %w", panelURL, err)
	}
	if r.Armazenamento == nil {
		return nil, errors.New("o painel não sabe qual armazenamento este servidor vai usar: gere o comando pela tela Novo servidor, escolhendo o armazenamento")
	}
	return &r, nil
}

// Linhas monta o arquivo de configuração, sem as chaves. O que o painel não
// disse (região, endereço) fica fora, e o agente usa o padrão. O painel é o
// que a instalação usou (--panel-url): o serviço lê só este arquivo, e o
// endereço que o painel conhece de si pode não ser o que esta máquina alcança.
func Linhas(p *DoPainel, panelURL string) []string {
	a := p.Armazenamento
	l := []string{
		"AGENT_ID=" + p.AgentID,
		"PANEL_URL=" + panelURL,
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

// NaConfig devolve a configuração com o bucket que o painel indicou, pelos
// mesmos padrões de `config.Load` — é o que o teste da chave usa antes de o
// arquivo existir.
func NaConfig(p *DoPainel, cfg *config.Config) *config.Config {
	c := *cfg
	a := p.Armazenamento
	c.StorageID, c.StorageBucket, c.StorageEndpoint = a.ID, a.Bucket, ""
	if a.Endpoint != nil {
		c.StorageEndpoint = *a.Endpoint
	}
	c.StorageRegion = config.DefaultRegion
	if a.Region != nil && *a.Region != "" {
		c.StorageRegion = *a.Region
	}
	return &c
}

// MaxTentativas antes de desistir: quem errou cinco vezes precisa conferir a
// chave no provedor, não digitar de novo.
const MaxTentativas = 5

// PerguntarETestar pede a chave e a senha até o bucket aceitar. Devolve as
// duas quando o teste passa. Só pergunta de novo quando o problema é a chave:
// rede, nome do bucket ou região não se corrigem digitando outra chave.
func PerguntarETestar(ctx context.Context, t *terminal.Terminal, base *config.Config) (string, string, error) {
	out := t.Saida()
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
			if storage.Classe(err) != "chave" {
				return "", "", fmt.Errorf("%s", storage.Causa(err))
			}
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

// Gravar escreve as linhas no arquivo, legível só pelo administrador (0600;
// no Windows, Administradores e SYSTEM). O que o arquivo já tinha e não é
// uma destas chaves fica — SIBLING_BUCKETS, caminhos próprios, proxy.
func Gravar(caminho string, linhas []string) error {
	if err := os.MkdirAll(filepath.Dir(caminho), 0o700); err != nil {
		return fmt.Errorf("criando %s: %w", filepath.Dir(caminho), err)
	}
	novas := map[string]bool{}
	for _, l := range linhas {
		k, _, _ := strings.Cut(l, "=")
		novas[k] = true
	}
	var mantidas []string
	if b, err := os.ReadFile(caminho); err == nil {
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			k, _, _ := strings.Cut(strings.TrimSpace(l), "=")
			if strings.TrimSpace(l) != "" && !novas[k] {
				mantidas = append(mantidas, l)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lendo %s: %w", caminho, err)
	}
	conteudo := strings.Join(append(mantidas, linhas...), "\n") + "\n"
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

// Chaves são as duas linhas da credencial.
func Chaves(ak, sk string) []string {
	return []string{"STORAGE_ACCESS_KEY=" + ak, "STORAGE_SECRET_KEY=" + sk}
}

// PodeGravar confere, antes de perguntar qualquer coisa, que o arquivo pode
// ser escrito: sem isso, quem instala sem sudo digitava a chave, via o teste
// passar e só então esbarrava na permissão.
func PodeGravar(caminho string) error {
	dir := filepath.Dir(caminho)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sem permissão para gravar em %s: rode com sudo (ou como administrador), ou aponte --config para um caminho seu", dir)
	}
	f, err := os.CreateTemp(dir, ".arkame-teste-*")
	if err != nil {
		return fmt.Errorf("sem permissão para gravar em %s: rode com sudo (ou como administrador), ou aponte --config para um caminho seu", dir)
	}
	f.Close()
	return os.Remove(f.Name())
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
