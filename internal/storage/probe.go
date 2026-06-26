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
	vr, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket})
	if err != nil {
		report.Error = "versioning: " + err.Error()
		return report
	}
	switch vr.Status {
	case types.BucketVersioningStatusEnabled:
		report.Versioning = "Enabled"
	case types.BucketVersioningStatusSuspended:
		report.Versioning = "Suspended"
	default:
		report.Versioning = "Off"
	}

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
		for _, rule := range lr.Rules {
			lc := api.Lifecycle{}
			if rule.Filter != nil && rule.Filter.Prefix != nil {
				lc.Prefix = *rule.Filter.Prefix
			}
			for _, t := range rule.Transitions {
				if t.StorageClass != "" {
					lc.Transitions = append(lc.Transitions, string(t.StorageClass))
				}
			}
			if rule.Expiration != nil && rule.Expiration.Days != nil {
				lc.ExpirationDays = int(*rule.Expiration.Days)
			}
			report.Lifecycle = append(report.Lifecycle, lc)
		}
	}

	// Ocupação real: soma o tamanho de todos os objetos (ListObjectsV2 paginado).
	// Não fatal se falhar — o resto do probe ainda é útil.
	if used, count, err := measureUsage(ctx, client, bucket); err == nil {
		report.UsedBytes = used
		report.ObjectCount = count
	}

	return report
}

// measureUsage pagina ListObjectsV2 somando Size de cada objeto (versão corrente).
// Para buckets muito grandes isto pode ser caro; o probe roda só 1×/h ou on-demand.
func measureUsage(ctx context.Context, client *s3.Client, bucket string) (int64, int64, error) {
	var totalBytes, count int64
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return totalBytes, count, err
		}
		for _, obj := range page.Contents {
			if obj.Size != nil {
				totalBytes += *obj.Size
			}
			count++
		}
	}
	return totalBytes, count, nil
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
