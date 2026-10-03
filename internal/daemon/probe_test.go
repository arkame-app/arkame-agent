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
