package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestCausa(t *testing.T) {
	casos := map[string]string{
		"operation error S3: GetBucketVersioning, https response error StatusCode: 403, api error InvalidAccessKeyId": "recusou a chave",
		"api error SignatureDoesNotMatch":                          "recusou a chave",
		"StatusCode: 404, api error NoSuchBucket":                  "não foi encontrado",
		"dial tcp: lookup x: no such host":                         "pela rede",
		"STORAGE_ACCESS_KEY / STORAGE_SECRET_KEY não configurados": "recusou a chave",
		"algo novo": "inesperado",
	}
	for msg, trecho := range casos {
		if c := Causa(errors.New(msg)); !strings.Contains(c, trecho) {
			t.Errorf("Causa(%q) = %q, esperado conter %q", msg, c, trecho)
		}
	}
}
