package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// bucketVersionado responde o mínimo do probe: versionamento ligado, sem
// Object Lock nem lifecycle, e a listagem de versões em duas páginas. A
// listagem só das atuais (ListObjectsV2) devolve um total menor, para o teste
// pegar quem a usar.
type bucketVersionado struct {
	listagemFalha bool
	lifecycle     string // corpo do GetBucketLifecycleConfiguration; vazio = sem lifecycle
}

func (b *bucketVersionado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case q.Has("versioning"):
		_, _ = io.WriteString(w, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	case q.Has("lifecycle") && b.lifecycle != "":
		_, _ = io.WriteString(w, b.lifecycle)
	case q.Has("object-lock"), q.Has("lifecycle"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchLifecycleConfiguration</Code></Error>`)
	case b.listagemFalha:
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>negado</Message></Error>`)
	case q.Has("versions") && q.Get("key-marker") == "":
		// a.txt: atual 100 + antiga 1000. b.txt: delete marker no topo e uma
		// versão antiga de 10.
		_, _ = io.WriteString(w, `<ListVersionsResult><Name>b</Name><IsTruncated>true</IsTruncated>
<NextKeyMarker>a.txt</NextKeyMarker><NextVersionIdMarker>a1</NextVersionIdMarker>
<Version><Key>a.txt</Key><VersionId>a2</VersionId><IsLatest>true</IsLatest><Size>100</Size></Version>
<Version><Key>a.txt</Key><VersionId>a1</VersionId><IsLatest>false</IsLatest><Size>1000</Size></Version>
</ListVersionsResult>`)
	case q.Has("versions"):
		_, _ = io.WriteString(w, `<ListVersionsResult><Name>b</Name><IsTruncated>false</IsTruncated>
<DeleteMarker><Key>b.txt</Key><VersionId>b2</VersionId><IsLatest>true</IsLatest></DeleteMarker>
<Version><Key>b.txt</Key><VersionId>b1</VersionId><IsLatest>false</IsLatest><Size>10</Size></Version>
</ListVersionsResult>`)
	case q.Get("list-type") == "2":
		_, _ = io.WriteString(w, `<ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>
<Contents><Key>a.txt</Key><Size>100</Size></Contents></ListBucketResult>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func clienteDoBucket(t *testing.T, b *bucketVersionado) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})
}

// O bucket é versionado e o provedor cobra cada versão: a ocupação soma
// todas. O ListObjectsV2 só via a atual (100 B, em vez de 1110 B).
func TestProbeSomaTodasAsVersoes(t *testing.T) {
	r := Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{}), "b", "st")
	if r.Error != "" {
		t.Fatalf("erro no probe: %s", r.Error)
	}
	if r.UsedBytes == nil || *r.UsedBytes != 1110 {
		t.Fatalf("used_bytes = %v, queria 1110 (todas as versões)", valor(r.UsedBytes))
	}
	if r.ObjectCount == nil || *r.ObjectCount != 1 {
		t.Fatalf("object_count = %v, queria 1 (só a.txt está presente)", valor(r.ObjectCount))
	}
}

// Listagem que falha não pode virar "0 B" no painel: os campos vão ausentes.
func TestProbeSemListagemNaoMandaZero(t *testing.T) {
	r := Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{listagemFalha: true}), "b", "st")
	if r.Versioning != "Enabled" {
		t.Fatalf("o resto do probe deveria seguir: %+v", r)
	}
	if r.UsedBytes != nil || r.ObjectCount != nil {
		t.Fatalf("ocupação inventada: used=%v count=%v", valor(r.UsedBytes), valor(r.ObjectCount))
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "used_bytes") || strings.Contains(string(b), "object_count") {
		t.Fatalf("o JSON leva a ocupação mesmo sem medir: %s", b)
	}
}

func valor(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

// Regras do bucket que a sondagem precisa ler: a desativada some, o prefixo
// legado (fora do Filter) vale, e as de versões não-atuais — as que apagam o
// histórico de um bucket versionado — chegam ao painel.
const lifecycleDoBucket = `<LifecycleConfiguration>
<Rule><ID>desligada</ID><Status>Disabled</Status><Filter><Prefix></Prefix></Filter>
  <Expiration><Days>1</Days></Expiration>
  <NoncurrentVersionExpiration><NoncurrentDays>2</NoncurrentDays></NoncurrentVersionExpiration></Rule>
<Rule><ID>antigas-30d</ID><Status>Enabled</Status><Filter><Prefix>srv/</Prefix></Filter>
  <NoncurrentVersionTransition><NoncurrentDays>7</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>
  <NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays></NoncurrentVersionExpiration></Rule>
<Rule><ID>legada</ID><Status>Enabled</Status><Prefix>velho/</Prefix>
  <Transition><Days>60</Days><StorageClass>STANDARD_IA</StorageClass></Transition>
  <NoncurrentVersionTransition><NoncurrentDays>10</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition>
  <NoncurrentVersionTransition><NoncurrentDays>40</NoncurrentDays><StorageClass>DEEP_ARCHIVE</StorageClass></NoncurrentVersionTransition>
  <NoncurrentVersionExpiration><NoncurrentDays>90</NoncurrentDays></NoncurrentVersionExpiration></Rule>
</LifecycleConfiguration>`

func TestProbeLeRegrasDeVersoesNaoAtuais(t *testing.T) {
	r := Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{lifecycle: lifecycleDoBucket}), "b", "st")
	if r.Error != "" {
		t.Fatalf("erro no probe: %s", r.Error)
	}
	if len(r.Lifecycle) != 2 {
		t.Fatalf("regras = %+v, queria só as 2 ativas (a desativada não vale)", r.Lifecycle)
	}
	if r.Lifecycle[0].Prefix != "srv/" || r.Lifecycle[1].Prefix != "velho/" {
		t.Fatalf("prefixos = %q, %q; queria srv/ e o legado velho/", r.Lifecycle[0].Prefix, r.Lifecycle[1].Prefix)
	}
	if r.Lifecycle[0].ExpirationDays != 0 {
		t.Fatalf("expiration_days da regra desativada vazou: %+v", r.Lifecycle[0])
	}
	if r.Lifecycle[0].NoncurrentExpirationDays != 30 || r.Lifecycle[1].NoncurrentExpirationDays != 90 {
		t.Fatalf("noncurrent por regra = %+v", r.Lifecycle)
	}
	if r.NoncurrentExpirationDays == nil || *r.NoncurrentExpirationDays != 30 {
		t.Fatalf("noncurrent_expiration_days = %v, queria 30 (o menor entre as ativas; o 2 é da desativada)", r.NoncurrentExpirationDays)
	}
	if got := strings.Join(r.NoncurrentTransitions, ","); got != "GLACIER,DEEP_ARCHIVE" {
		t.Fatalf("noncurrent_transitions = %q, queria GLACIER,DEEP_ARCHIVE", got)
	}
	b, _ := json.Marshal(r)
	for _, campo := range []string{`"noncurrent_expiration_days":30`, `"noncurrent_transitions":["GLACIER","DEEP_ARCHIVE"]`} {
		if !strings.Contains(string(b), campo) {
			t.Fatalf("o corpo do probe não leva %s: %s", campo, b)
		}
	}
}

// Sem regra que expire versões antigas o campo vai ausente — um 0 seria lido
// como "apaga na hora".
func TestProbeSemExpiracaoDeNaoAtuaisOmiteCampo(t *testing.T) {
	regras, menor, classes := lerLifecycle([]types.LifecycleRule{
		{Status: types.ExpirationStatusEnabled, Expiration: &types.LifecycleExpiration{Days: aws.Int32(365)}},
		{Status: types.ExpirationStatusDisabled, NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(5)}},
	})
	if len(regras) != 1 || regras[0].ExpirationDays != 365 {
		t.Fatalf("regras = %+v", regras)
	}
	if menor != nil || classes != nil {
		t.Fatalf("menor=%v classes=%v; queria ausentes", menor, classes)
	}
	b, _ := json.Marshal(api.ProbeReport{Lifecycle: regras, NoncurrentExpirationDays: menor, NoncurrentTransitions: classes})
	if strings.Contains(string(b), "noncurrent") {
		t.Fatalf("o JSON leva campo de não-atuais sem regra: %s", b)
	}
}

// Sem permissão para ler o ciclo de vida ou o Object Lock, a sondagem não pode
// relatar "o bucket não tem": o erro vai no relato para o painel avisar. "Não
// tem configuração" segue sem erro nenhum.
func TestProbeSeparaSemPermissaoDeSemConfiguracao(t *testing.T) {
	negado := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("lifecycle") || q.Has("object-lock") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>r1</RequestId></Error>`)
			return
		}
		(&bucketVersionado{}).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(negado)
	t.Cleanup(srv.Close)
	c := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer:      aws.NopRetryer{},
	})

	r := Probe(context.Background(), c, "b", "st")
	if r.Error != "" || r.Versioning != "Enabled" {
		t.Fatalf("o resto da sondagem deveria seguir: %+v", r)
	}
	if r.LifecycleError != "AccessDenied: Access Denied" {
		t.Fatalf("lifecycle_error = %q, queria AccessDenied: Access Denied", r.LifecycleError)
	}
	if r.ObjectLockError != "AccessDenied: Access Denied" {
		t.Fatalf("object_lock_error = %q, queria AccessDenied: Access Denied", r.ObjectLockError)
	}
	if r.Lifecycle != nil || r.ObjectLock != nil {
		t.Fatalf("regras inventadas: lifecycle=%+v object_lock=%+v", r.Lifecycle, r.ObjectLock)
	}

	// Bucket sem regra nem trava: nenhum dos dois erros.
	r = Probe(context.Background(), clienteDoBucket(t, &bucketVersionado{}), "b", "st")
	if r.LifecycleError != "" || r.ObjectLockError != "" {
		t.Fatalf("bucket sem configuração relatado como erro: lifecycle=%q object_lock=%q", r.LifecycleError, r.ObjectLockError)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "_error") {
		t.Fatalf("o JSON leva erro sem haver: %s", b)
	}
}
