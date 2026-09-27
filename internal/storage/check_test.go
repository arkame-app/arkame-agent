package storage

import (
	"errors"
	"strings"
	"testing"
)

// Só a chave se corrige digitando outra chave: o instalador pergunta de novo
// nesse caso, e para com a causa nos outros.
func TestClasse(t *testing.T) {
	casos := map[string]string{
		"StatusCode: 403, api error InvalidAccessKeyId": "chave",
		"api error PermanentRedirect":                   "bucket",
		"dial tcp 1.2.3.4:443: i/o timeout":             "rede",
		"algo novo":                                     "outra",
	}
	for msg, classe := range casos {
		if c := Classe(errors.New(msg)); c != classe {
			t.Errorf("Classe(%q) = %q, esperado %q", msg, c, classe)
		}
	}
}

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
