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
	"github.com/arkame-app/agent/internal/segredo"
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

// ErrSemChave: a pessoa saiu sem digitar a chave.
var ErrSemChave = errors.New("saída sem a chave do bucket: nada foi gravado. Se você não tem a chave, crie uma nova no console do provedor do bucket e rode o comando de novo")

// ErrCodigoInvalido: código expirado, já usado ou digitado errado.
var ErrCodigoInvalido = errors.New("o painel não reconheceu o código de instalação: ele expira em 24 horas e vale uma vez. Gere outro no painel, em Servidores → Novo servidor")

// ErrSemArmazenamento: o código de instalação não está amarrado a um
// armazenamento.
var ErrSemArmazenamento = errors.New("o painel não sabe qual armazenamento este servidor vai usar: gere o comando pela tela Novo servidor, escolhendo o armazenamento")

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
		return nil, ErrSemArmazenamento
	}
	// Cada valor vira uma linha do arquivo: quebra de linha escreveria outra
	// chave nele (PANEL_URL=…). O painel já recusa no cadastro; aqui é a
	// segunda camada.
	a := r.Armazenamento
	for _, v := range []*string{&r.AgentID, &a.ID, &a.Bucket, a.Region, a.Endpoint} {
		if v != nil && strings.ContainsAny(*v, "\r\n") {
			return nil, errors.New("o painel respondeu um valor com quebra de linha: não gravo isso no arquivo de configuração")
		}
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

// LinhasDoArmazenamento são as linhas do armazenamento que o painel indicou,
// para trocar o de um arquivo que já existe. Região e endereço vão sempre,
// vazios quando o painel não os diz (vazio é o padrão, como em config.Load):
// sem isso, a região e o endereço do armazenamento antigo sobravam no arquivo
// junto do bucket novo.
func LinhasDoArmazenamento(p *DoPainel) []string {
	a := p.Armazenamento
	regiao, endereco := "", ""
	if a.Region != nil {
		regiao = *a.Region
	}
	if a.Endpoint != nil {
		endereco = *a.Endpoint
	}
	return []string{
		"STORAGE_ID=" + a.ID,
		"STORAGE_BUCKET=" + a.Bucket,
		"STORAGE_REGION=" + regiao,
		"STORAGE_ENDPOINT=" + endereco,
	}
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
		ak, err := t.Pergunta("Chave de acesso (access key) — em branco para sair")
		if err != nil {
			return "", "", err
		}
		// Quem não tem a chave precisa de uma saída que não seja errar cinco
		// vezes (teste do fundador, 27/09).
		if ak == "" {
			return "", "", ErrSemChave
		}
		sk, err := t.Segredo("Senha da chave (secret key)")
		if err != nil {
			return "", "", err
		}
		if sk == "" {
			fmt.Fprintln(out, "  ✗ falta a senha da chave.")
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
				fmt.Fprintln(out, "  Digite de novo (sem a chave? deixe em branco e aperte Enter para sair):")
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
	if err := segredo.CriarPasta(filepath.Dir(caminho)); err != nil {
		return err
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
	return segredo.Gravar(caminho, []byte(conteudo), 0o600)
}

// Remover tira do arquivo as linhas destas chaves; o resto fica como está.
// Arquivo que não existe ou não tem nenhuma delas não é reescrito.
func Remover(caminho string, chaves ...string) error {
	b, err := os.ReadFile(caminho)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lendo %s: %w", caminho, err)
	}
	tirar := map[string]bool{}
	for _, k := range chaves {
		tirar[k] = true
	}
	var mantidas []string
	mudou := false
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(l), "=")
		if tirar[strings.TrimSpace(k)] {
			mudou = true
			continue
		}
		if strings.TrimSpace(l) != "" {
			mantidas = append(mantidas, l)
		}
	}
	if !mudou {
		return nil
	}
	conteudo := strings.Join(mantidas, "\n")
	if conteudo != "" {
		conteudo += "\n"
	}
	return segredo.Gravar(caminho, []byte(conteudo), 0o600)
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
	if err := segredo.CriarPasta(dir); err != nil {
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
