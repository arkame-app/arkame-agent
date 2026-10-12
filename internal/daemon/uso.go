package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/estado"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A medição de ocupação lista o bucket inteiro (ListObjectVersions, mil
// versões por requisição). Medido na bancada: 349 requisições e 136 MB de XML
// por sondagem com 348 mil versões — de hora em hora, e mais uma a cada
// partida do agente. Acima de limiarDeVersoesDoUso ela passa a rodar uma vez
// por dia (com um desvio aleatório, para os agentes de um mesmo provedor não
// listarem todos na mesma hora), e a sondagem de hora em hora leva o último
// valor medido. As consultas baratas (versionamento, Object Lock, ciclo de
// vida) seguem em toda sondagem.
var (
	limiarDeVersoesDoUso int64 = 100_000
	intervaloDoUsoGrande       = 24 * time.Hour
	desvioDoUsoGrande          = 2 * time.Hour
	agoraDoUso                 = time.Now
	medirUso                   = storage.MedirUso
	sortearDesvio              = func(teto time.Duration) time.Duration { return rand.N(teto) }
)

// usoMedido é a última medição, como fica guardada no disco: uma partida do
// agente não precisa listar um bucket grande de novo.
type usoMedido struct {
	storage.Uso
	Bucket  string    `json:"bucket"`
	Em      time.Time `json:"em"`
	Proxima time.Time `json:"proxima"`
}

// medidorDeUso decide quando medir a ocupação do bucket e guarda a última
// medição.
type medidorDeUso struct {
	arquivos []string // onde a medição pode ficar guardada, em ordem de preferência
	sempre   bool     // ARKAME_STATE=always
	ultimo   *usoMedido
}

func novoMedidorDeUso(cfg *config.Config) *medidorDeUso {
	m := &medidorDeUso{sempre: cfg.StateAlways}
	if cfg.StorageID == "" || cfg.AgentID == "" {
		return m
	}
	// A pasta do estado não é criada aqui: ela só passa a existir quando o
	// estado local entra em uso (ver gravar). Se já existe, a última medição
	// é lida de onde estiver.
	for _, dir := range pastasDoEstado(cfg) {
		base := estado.NomeDoArquivo(dir, identidadeDoEstado(cfg, cfg.StorageBucket))
		arquivo := base[:len(base)-len(filepath.Ext(base))] + ".uso.json"
		m.arquivos = append(m.arquivos, arquivo)
		if m.ultimo != nil {
			continue
		}
		if b, err := os.ReadFile(arquivo); err == nil {
			var u usoMedido
			if json.Unmarshal(b, &u) == nil && u.Bucket == cfg.StorageBucket && !u.Em.IsZero() {
				m.ultimo = &u
			}
		}
	}
	return m
}

// estadoLocalEmUso: algum backup deste processo abriu o estado local (o
// painel ofereceu o índice em lotes, ou ARKAME_STATE=always). Antes disso o
// agente não grava nada na pasta do estado — contra o painel de antes ele não
// deixa arquivo nenhum além dos da v0.4.31.
var estadoLocalEmUso atomic.Bool

// gravar guarda a última medição no disco, na primeira pasta do estado que
// aceitar. Só com o estado local em uso; senão a medição vale em memória,
// até o processo sair.
func (m *medidorDeUso) gravar(u *usoMedido) {
	if !m.sempre && !estadoLocalEmUso.Load() {
		return
	}
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	for _, arquivo := range m.arquivos {
		if os.MkdirAll(filepath.Dir(arquivo), 0o700) != nil {
			continue
		}
		tmp := arquivo + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, arquivo) == nil {
			return
		}
	}
}

// uso devolve a ocupação a relatar: mede agora, ou devolve a última medição
// quando o bucket é grande e ainda não é hora. nil quando não há medição
// (a listagem falhou): os campos vão ausentes no relato.
func (m *medidorDeUso) uso(ctx context.Context, s3c *s3.Client, bucket string) *storage.Uso {
	agora := agoraDoUso()
	if u := m.ultimo; u != nil && u.Bucket == bucket && u.Versoes > limiarDeVersoesDoUso && agora.Before(u.Proxima) && !agora.Before(u.Em) {
		return &u.Uso
	}
	medido, err := medirUso(ctx, s3c, bucket)
	if err != nil {
		slog.Debug("medição de ocupação do bucket falhou", "err", err)
		return nil
	}
	novo := &usoMedido{Uso: medido, Bucket: bucket, Em: agora}
	if medido.Versoes > limiarDeVersoesDoUso {
		novo.Proxima = agora.Add(intervaloDoUsoGrande + sortearDesvio(desvioDoUsoGrande))
		if m.ultimo == nil || m.ultimo.Versoes <= limiarDeVersoesDoUso {
			slog.Info("bucket grande: a ocupação passa a ser medida uma vez por dia; a sondagem de hora em hora leva o último valor",
				"versoes", medido.Versoes, "proxima", novo.Proxima.Format(time.RFC3339))
		}
	}
	m.ultimo = novo
	m.gravar(novo)
	return &medido
}
