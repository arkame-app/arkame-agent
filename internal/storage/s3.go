// Package storage encapsula acesso ao bucket BYOS do cliente via AWS SDK v2
// (S3 e compatíveis: Backblaze B2, Wasabi, Cloudflare R2, MinIO, OCI S3-compat, GCS S3-compat).
package storage

import (
	"context"
	"fmt"

	"github.com/arkame-app/agent/internal/config"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NewS3Client constrói um S3 client com credenciais locais.
// O endpoint_url é respeitado para S3-compat (MinIO, Wasabi, etc.).
func NewS3Client(ctx context.Context, cfg *config.Config) (*s3.Client, error) {
	if cfg.StorageAccessKey == "" || cfg.StorageSecretKey == "" {
		return nil, fmt.Errorf("STORAGE_ACCESS_KEY / STORAGE_SECRET_KEY não configurados")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.StorageRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.StorageAccessKey, cfg.StorageSecretKey, "",
		)),
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.StorageEndpoint != "" {
			o.BaseEndpoint = &cfg.StorageEndpoint
			o.UsePathStyle = true // compat com MinIO
		}
	}), nil
}
