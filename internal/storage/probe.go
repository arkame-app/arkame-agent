package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/api"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// VersioningStatus devolve o estado do versionamento do bucket: "Enabled",
// "Suspended" ou "Off" (nunca ativado).
func VersioningStatus(ctx context.Context, client *s3.Client, bucket string) (string, error) {
	vr, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket})
	if err != nil {
		return "", err
	}
	switch vr.Status {
	case types.BucketVersioningStatusEnabled:
		return "Enabled", nil
	case types.BucketVersioningStatusSuspended:
		return "Suspended", nil
	default:
		return "Off", nil
	}
}

// Probe inspeciona a configuração do bucket via S3 API e devolve um ProbeReport.
// Decisão arquitetural (PLAN.md round 13): o painel NÃO tem credenciais para
// consultar o bucket; o agent é quem descobre a config e reporta.
//
// Chamadas:
//   - GetBucketVersioning
//   - GetObjectLockConfiguration (pode falhar se bucket não tem Object Lock)
//   - GetBucketLifecycleConfiguration (pode falhar se não há lifecycle setado)
func Probe(ctx context.Context, client *s3.Client, bucket, storageID string) api.ProbeReport {
	report := api.ProbeReport{
		StorageID: storageID,
		ProbedAt:  time.Now().UTC(),
	}

	// Versioning
	v, err := VersioningStatus(ctx, client, bucket)
	if err != nil {
		report.Error = "versioning: " + err.Error()
		return report
	}
	report.Versioning = v

	// Object Lock — opcional, pode não estar habilitado
	olr, err := client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &bucket})
	if err == nil && olr.ObjectLockConfiguration != nil && olr.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled {
		ol := &api.ObjectLock{Enabled: true}
		if rule := olr.ObjectLockConfiguration.Rule; rule != nil && rule.DefaultRetention != nil {
			ol.Mode = string(rule.DefaultRetention.Mode)
			if rule.DefaultRetention.Days != nil {
				ol.Days = int(*rule.DefaultRetention.Days)
			}
		}
		report.ObjectLock = ol
	}

	// Lifecycle — opcional
	lr, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &bucket})
	if err == nil {
		report.Lifecycle, report.NoncurrentExpirationDays, report.NoncurrentTransitions = lerLifecycle(lr.Rules)
	}

	// Ocupação real: soma o tamanho de todas as versões (ListObjectVersions
	// paginado). O bucket é versionado e a cobrança do provedor conta cada
	// versão antiga; o ListObjectsV2 só via a atual e subestimava a ocupação.
	// Não fatal se falhar — o resto do probe ainda é útil —, mas aí os campos
	// vão ausentes: um 0 aparecia no painel como "0 B", bucket vazio.
	if used, count, err := measureUsage(ctx, client, bucket); err == nil {
		report.UsedBytes = &used
		report.ObjectCount = &count
	}

	return report
}

// lerLifecycle traduz as regras do bucket. Regra desativada não conta: o
// provedor não a aplica. O prefixo vem do Filter ou, em regra antiga, do
// campo Prefix da própria regra. Além da expiração e das transições da versão
// atual, lê as de versões não-atuais — num bucket versionado são elas que
// apagam ou congelam o histórico que a retenção promete. Devolve também o
// menor NoncurrentDays (nil se nenhuma regra expira versões antigas) e as
// classes de destino das transições de não-atuais, sem repetição.
func lerLifecycle(rules []types.LifecycleRule) (regras []api.Lifecycle, menorNaoAtual *int, classesNaoAtuais []string) {
	vistas := map[string]bool{}
	for _, rule := range rules {
		if rule.Status != types.ExpirationStatusEnabled {
			continue
		}
		lc := api.Lifecycle{}
		switch {
		case rule.Filter != nil && rule.Filter.Prefix != nil:
			lc.Prefix = *rule.Filter.Prefix
		case rule.Filter != nil && rule.Filter.And != nil && rule.Filter.And.Prefix != nil:
			lc.Prefix = *rule.Filter.And.Prefix
		case rule.Prefix != nil:
			lc.Prefix = *rule.Prefix
		}
		for _, t := range rule.Transitions {
			if t.StorageClass != "" {
				lc.Transitions = append(lc.Transitions, string(t.StorageClass))
			}
		}
		if rule.Expiration != nil && rule.Expiration.Days != nil {
			lc.ExpirationDays = int(*rule.Expiration.Days)
		}
		if ne := rule.NoncurrentVersionExpiration; ne != nil && ne.NoncurrentDays != nil && *ne.NoncurrentDays > 0 {
			lc.NoncurrentExpirationDays = int(*ne.NoncurrentDays)
			if menorNaoAtual == nil || lc.NoncurrentExpirationDays < *menorNaoAtual {
				d := lc.NoncurrentExpirationDays
				menorNaoAtual = &d
			}
		}
		for _, t := range rule.NoncurrentVersionTransitions {
			if t.StorageClass == "" {
				continue
			}
			c := string(t.StorageClass)
			lc.NoncurrentTransitions = append(lc.NoncurrentTransitions, c)
			if !vistas[c] {
				vistas[c] = true
				classesNaoAtuais = append(classesNaoAtuais, c)
			}
		}
		regras = append(regras, lc)
	}
	return regras, menorNaoAtual, classesNaoAtuais
}

// measureUsage pagina ListObjectVersions: bytes é a soma de todas as versões
// (a ocupação que o provedor cobra) e objects é o nº de arquivos presentes
// (versões atuais; delete marker não tem tamanho nem conta como arquivo).
// Para buckets muito grandes isto pode ser caro; o probe roda só 1×/h ou
// on-demand.
func measureUsage(ctx context.Context, client *s3.Client, bucket string) (bytes, objects int64, err error) {
	p := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: &bucket})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, 0, err
		}
		for _, v := range page.Versions {
			if v.Size != nil {
				bytes += *v.Size
			}
			if v.IsLatest != nil && *v.IsLatest {
				objects++
			}
		}
	}
	return bytes, objects, nil
}

// IsBenignProbeError identifica erros S3 que indicam "config ausente" em vez de erro real.
// Útil para reportes parciais bem-sucedidos.
func IsBenignProbeError(err error) bool {
	if err == nil {
		return true
	}
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchLifecycleConfiguration",
			"ObjectLockConfigurationNotFoundError":
			return true
		}
	}
	return strings.Contains(err.Error(), "NoSuchLifecycleConfiguration") ||
		strings.Contains(err.Error(), "ObjectLockConfigurationNotFound")
}
