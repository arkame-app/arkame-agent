package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arkame-app/agent/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Check confere se esta máquina alcança o bucket com a chave que tem.
//
// É a primeira chamada do teste de conexão do painel (Probe), feita antes de
// registrar o servidor: em 27/09 uma instalação no Windows seguiu com a chave
// errada e nada avisou — o primeiro backup falharia em silêncio. Aqui a chave
// recusada para a instalação na hora, com a causa em português.
func Check(ctx context.Context, cfg *config.Config) error {
	if cfg.StorageBucket == "" {
		return fmt.Errorf("STORAGE_BUCKET não configurado")
	}
	client, err := NewS3Client(ctx, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bucket := cfg.StorageBucket
	if _, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket}); err != nil {
		return err
	}
	return nil
}

// Causa traduz o erro do SDK no que a pessoa corrige, e onde. A mensagem
// crua do provedor continua disponível para o suporte.
//
// As mesmas classes que o painel usa na trilha (`causaDaFalha`).
func Causa(err error) string {
	if err == nil {
		return ""
	}
	var ae interface{ ErrorCode() string }
	codigo := ""
	if errors.As(err, &ae) {
		codigo = ae.ErrorCode()
	}
	msg := codigo + " " + err.Error()
	switch {
	case contem(msg, "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied", "Forbidden", "StatusCode: 401", "StatusCode: 403", "InvalidToken", "Unauthorized", "não configurados"):
		return "o bucket recusou a chave: confira a chave de acesso e a senha (e se a chave tem permissão neste bucket)"
	case contem(msg, "NoSuchBucket", "StatusCode: 404", "PermanentRedirect", "AuthorizationHeaderMalformed", "IllegalLocationConstraint"):
		return "o bucket não foi encontrado com o nome, a região ou o endereço cadastrados: corrija o cadastro do armazenamento no painel"
	case contem(msg, "connection refused", "no such host", "dial tcp", "i/o timeout", "deadline exceeded", "certificate", "tls:"):
		return "não foi possível chegar ao bucket pela rede: confira o acesso desta máquina à internet, proxy ou firewall"
	default:
		return "o bucket respondeu com um erro inesperado"
	}
}

func contem(s string, alvos ...string) bool {
	baixo := strings.ToLower(s)
	for _, a := range alvos {
		if strings.Contains(baixo, strings.ToLower(a)) {
			return true
		}
	}
	return false
}
