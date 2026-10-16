package daemon

import (
	"strings"
	"testing"

	"github.com/arkame-app/agent/internal/config"
)

// A cópia local de uma instalação anterior, sem HTTPS, não roda: mandaria o
// backup em texto puro para quem estivesse na porta.
func TestCopiaLocalSemTLSNaoRoda(t *testing.T) {
	boa := &config.Config{LocalStoreProgram: "/opt/arkame/rustfs/rustfs", LocalStoreTLS: "/etc/arkame/rustfs/tls", StorageCAFile: "/etc/arkame/rustfs/tls/rustfs_cert.pem", StorageEndpoint: "https://127.0.0.1:9900"}
	if err := conferirCopiaLocal(boa); err != nil {
		t.Fatal(err)
	}
	if err := conferirCopiaLocal(&config.Config{StorageEndpoint: "http://minio:9000"}); err != nil {
		t.Fatalf("o agente sem cópia local foi recusado: %v", err)
	}
	for nome, estraga := range map[string]func(*config.Config){
		"http":    func(c *config.Config) { c.StorageEndpoint = "http://127.0.0.1:9900" },
		"sem CA":  func(c *config.Config) { c.StorageCAFile = "" },
		"sem TLS": func(c *config.Config) { c.LocalStoreTLS = "" },
	} {
		c := *boa
		estraga(&c)
		if err := conferirCopiaLocal(&c); err == nil || !strings.Contains(err.Error(), "local-storage install") {
			t.Errorf("%s: aceito (%v)", nome, err)
		}
	}
}
