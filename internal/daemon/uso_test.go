package daemon

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arkame-app/agent/internal/bancada"
	"github.com/arkame-app/agent/internal/config"
	"github.com/arkame-app/agent/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"strings"
)

func bucketCom(t *testing.T, objetos int) (*bancada.S3Falso, *s3.Client) {
	t.Helper()
	f := bancada.NovoS3Falso()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := bancada.ClienteS3(srv.URL)
	for i := 0; i < objetos; i++ {
		if _, err := c.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String("b"), Key: aws.String(fmt.Sprintf("data/a/%05d", i)), Body: strings.NewReader("0123456789"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.Zerar()
	return f, c
}

func relogioDoUso(t *testing.T, limiar int64) *time.Time {
	t.Helper()
	agora := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	antesAgora, antesLimiar, antesSorteio := agoraDoUso, limiarDeVersoesDoUso, sortearDesvio
	agoraDoUso = func() time.Time { return agora }
	limiarDeVersoesDoUso = limiar
	sortearDesvio = func(time.Duration) time.Duration { return 30 * time.Minute }
	t.Cleanup(func() { agoraDoUso, limiarDeVersoesDoUso, sortearDesvio = antesAgora, antesLimiar, antesSorteio })
	return &agora
}

// Bucket grande: a listagem inteira roda uma vez por dia, não de hora em
// hora — e nem a cada partida do agente. A sondagem segue relatando a última
// ocupação medida, e as consultas baratas seguem em toda sondagem.
func TestOcupacaoDeBucketGrandeEMedidaUmaVezPorDia(t *testing.T) {
	agora := relogioDoUso(t, 2000)
	f, c := bucketCom(t, 2500) // 3 páginas de listagem
	cfg := &config.Config{AgentID: "ag1", StorageID: "st1", StorageBucket: "b", StateDir: t.TempDir()}
	// A medição só fica no disco com o estado local em uso (o painel oferece
	// o índice em lotes).
	antes := estadoLocalEmUso.Swap(true)
	t.Cleanup(func() { estadoLocalEmUso.Store(antes) })
	ctx := context.Background()

	sondar := func(m *medidorDeUso) *storage.Uso {
		t.Helper()
		r := storage.ProbeConfig(ctx, c, "b", "st1")
		if r.Error != "" || r.Versioning != "Enabled" {
			t.Fatalf("sondagem: %+v", r)
		}
		return m.uso(ctx, c, "b")
	}

	m := novoMedidorDeUso(cfg)
	u := sondar(m)
	if u == nil || u.Versoes != 2500 || u.Objetos != 2500 || u.Bytes != 25000 || f.N("ListObjectVersions") != 3 {
		t.Fatalf("primeira medição: %+v, listagens=%d", u, f.N("ListObjectVersions"))
	}
	// 23 sondagens de hora em hora: nenhuma listagem, o valor continua indo.
	for h := 1; h <= 23; h++ {
		*agora = agora.Add(time.Hour)
		if u := sondar(m); u == nil || u.Bytes != 25000 {
			t.Fatalf("hora %d: a sondagem tem de levar a última ocupação: %+v", h, u)
		}
	}
	if n := f.N("ListObjectVersions"); n != 3 {
		t.Fatalf("bucket grande listado de novo dentro do dia: %d listagens", n)
	}
	if f.N("GetBucketVersioning") != 24 || f.N("GetObjectLockConfiguration") != 24 || f.N("GetBucketLifecycleConfiguration") != 24 {
		t.Fatalf("as consultas baratas seguem em toda sondagem: %v", f.Requisicoes())
	}
	// O agente reinicia: a medição guardada vale, sem listar.
	m = novoMedidorDeUso(cfg)
	if u := sondar(m); u == nil || u.Bytes != 25000 || f.N("ListObjectVersions") != 3 {
		t.Fatalf("a partida do agente listou o bucket: %+v, listagens=%d", u, f.N("ListObjectVersions"))
	}
	// Passado um dia (mais o desvio), mede de novo.
	*agora = agora.Add(2 * time.Hour)
	if sondar(m); f.N("ListObjectVersions") != 6 {
		t.Fatalf("depois de um dia a ocupação é medida de novo: %d listagens", f.N("ListObjectVersions"))
	}
}

// Bucket pequeno segue como era: mede em toda sondagem.
func TestOcupacaoDeBucketPequenoSegueDeHoraEmHora(t *testing.T) {
	agora := relogioDoUso(t, 2000)
	f, c := bucketCom(t, 300)
	m := novoMedidorDeUso(&config.Config{AgentID: "ag1", StorageID: "st1", StorageBucket: "b", StateDir: t.TempDir()})
	for h := 0; h < 5; h++ {
		*agora = agora.Add(time.Hour)
		if u := m.uso(context.Background(), c, "b"); u == nil || u.Objetos != 300 {
			t.Fatalf("%+v", u)
		}
	}
	if n := f.N("ListObjectVersions"); n != 5 {
		t.Fatalf("listagens = %d, queria 5", n)
	}
}

// Sem o estado local em uso (painel de antes do índice em lotes), a medição
// vale em memória e nada é gravado na pasta do estado — nem a pasta é criada.
func TestOcupacaoNaoGravaNadaSemEstadoLocalEmUso(t *testing.T) {
	relogioDoUso(t, 2000)
	antes := estadoLocalEmUso.Swap(false)
	t.Cleanup(func() { estadoLocalEmUso.Store(antes) })
	_, c := bucketCom(t, 2500)
	dir := filepath.Join(t.TempDir(), "estado")
	cfg := &config.Config{AgentID: "ag1", StorageID: "st1", StorageBucket: "b", StateDir: dir}
	m := novoMedidorDeUso(cfg)
	if u := m.uso(context.Background(), c, "b"); u == nil || u.Versoes != 2500 {
		t.Fatalf("%+v", u)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a pasta do estado foi criada sem o estado em uso (err=%v)", err)
	}
	// O estado entra em uso (um backup o abriu): a medição que estava em
	// memória vai para o disco, e a partida seguinte não lista o bucket.
	estadoLocalEmUso.Store(true)
	guardarUsoPendente()
	if restos, _ := os.ReadDir(dir); len(restos) != 1 {
		t.Fatalf("a medição pendente devia ter sido gravada quando o estado entrou em uso: %d arquivo(s)", len(restos))
	}
	_ = os.RemoveAll(dir)
	estadoLocalEmUso.Store(false)
	// Com ARKAME_STATE=always, grava.
	cfg.StateAlways = true
	m = novoMedidorDeUso(cfg)
	m.uso(context.Background(), c, "b")
	if restos, _ := os.ReadDir(dir); len(restos) != 1 {
		t.Fatalf("com o estado forçado a medição devia ficar guardada: %d arquivo(s)", len(restos))
	}
}
