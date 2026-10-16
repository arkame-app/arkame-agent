package localstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/arkame-app/agent/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PrepararBucket deixa o bucket da cópia local pronto para o agente: existe e
// tem versionamento ligado (sem ele o agente recusa o backup — a versão
// anterior de um arquivo é o que se restaura depois de um ataque). Idempotente.
//
// Sem Object Lock: a limpeza das versões antigas, pela retenção, é do próprio
// agente, e a cópia que fica fora do alcance de um ataque é a da nuvem.
func PrepararBucket(ctx context.Context, c *s3.Client, bucket string) error {
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var nf *types.NotFound
		var nsb *types.NoSuchBucket
		if !errors.As(err, &nf) && !errors.As(err, &nsb) {
			return fmt.Errorf("conferindo o bucket %s da cópia local: %w", bucket, err)
		}
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			var ja *types.BucketAlreadyOwnedByYou
			if !errors.As(err, &ja) {
				return fmt.Errorf("criando o bucket %s da cópia local: %w", bucket, err)
			}
		}
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		return fmt.Errorf("ligando o versionamento do bucket %s: %w", bucket, err)
	}
	st, err := storage.VersioningStatus(ctx, c, bucket)
	if err != nil {
		return fmt.Errorf("conferindo o versionamento do bucket %s: %w", bucket, err)
	}
	if st != "Enabled" {
		return fmt.Errorf("o versionamento do bucket %s ficou %q, não Enabled", bucket, st)
	}
	return nil
}
