package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/api"
)

// Sem medir a ocupação, o relato não leva used_bytes nem object_count: com 0,
// o painel mostrava "0 B" para um bucket cheio. Medido, leva — inclusive o 0
// de um bucket vazio de verdade.
func TestCorpoDoProbeOcupacaoAusenteQuandoNaoMedida(t *testing.T) {
	b, _ := json.Marshal(corpoDoProbe(api.ProbeReport{StorageID: "st", Versioning: "Enabled"}))
	if s := string(b); strings.Contains(s, "used_bytes") || strings.Contains(s, "object_count") {
		t.Fatalf("ocupação não medida foi no relato: %s", s)
	}
	zero := int64(0)
	b, _ = json.Marshal(corpoDoProbe(api.ProbeReport{StorageID: "st", Versioning: "Enabled", UsedBytes: &zero, ObjectCount: &zero}))
	if s := string(b); !strings.Contains(s, `"used_bytes":0`) || !strings.Contains(s, `"object_count":0`) {
		t.Fatalf("bucket vazio medido deveria mandar 0: %s", s)
	}
}

// O resumo das regras de versões não-atuais e os erros de leitura do ciclo de
// vida e do Object Lock vão no relato: o painel lê noncurrent_* desde o
// agente 0.4.10, e o corpo os deixava de fora.
func TestCorpoDoProbeLevaNaoAtuaisEErrosDeLeitura(t *testing.T) {
	trinta := 30
	b, _ := json.Marshal(corpoDoProbe(api.ProbeReport{
		StorageID:                "st",
		Versioning:               "Enabled",
		NoncurrentExpirationDays: &trinta,
		NoncurrentTransitions:    []string{"GLACIER", "DEEP_ARCHIVE"},
		LifecycleError:           "AccessDenied: Access Denied",
		ObjectLockError:          "AccessDenied: negado",
	}))
	s := string(b)
	for _, campo := range []string{
		`"noncurrent_expiration_days":30`,
		`"noncurrent_transitions":["GLACIER","DEEP_ARCHIVE"]`,
		`"lifecycle_error":"AccessDenied: Access Denied"`,
		`"object_lock_error":"AccessDenied: negado"`,
	} {
		if !strings.Contains(s, campo) {
			t.Errorf("o relato não leva %s: %s", campo, s)
		}
	}

	b, _ = json.Marshal(corpoDoProbe(api.ProbeReport{StorageID: "st", Versioning: "Enabled"}))
	for _, campo := range []string{"noncurrent_", "lifecycle_error", "object_lock_error"} {
		if strings.Contains(string(b), campo) {
			t.Errorf("sem valor, %s não deveria ir no relato: %s", campo, b)
		}
	}
}
