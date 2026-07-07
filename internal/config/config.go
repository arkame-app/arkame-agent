// Package config carrega configuração do agent a partir de:
//  1. Env-file (formato KEY=VALUE, um por linha)
//  2. Variáveis de ambiente do processo
//  3. Overrides de CLI (--flag)
//
// Precedência: CLI > env do processo > env-file > defaults.
//
// IMPORTANTE — credenciais de storage:
// STORAGE_ACCESS_KEY / STORAGE_SECRET_KEY nunca saem desta máquina.
// O agent usa essas credenciais localmente para falar com S3/compat.
// O painel Arkame não recebe nem armazena essas chaves.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Config é o conjunto completo de variáveis conhecidas pelo agent.
type Config struct {
	// Identidade & painel
	AgentID         string // setado após enrollment bem-sucedido (persisted em /etc/arkame/agent.id)
	PanelURL        string // https://save.arkame.app (ou subdomínio de partner whitelabel)
	EnrollmentToken string // só presente em install (first-time ou re-enrollment); limpo após sucesso
	Fingerprint     string // SHA-256 do public_key (mostrado para aprovação humana)

	// Auth — bearer JWT recebido do painel após approval
	TokenPath      string // /etc/arkame/token.jwt — JWT bearer (0600)
	PrivateKeyPath string // /etc/arkame/key.pem  — Ed25519 private key (0600), preservada p/ futura rotação
	AgentIDPath    string // /etc/arkame/agent.id — agent_id persistido após enrollment (configurável p/ rootless)

	// Filesystem
	HostRoot string // raiz da máquina; em container Docker vira /host (read-only)

	// Storage BYOS — credenciais que NUNCA saem desta máquina
	StorageAccessKey string
	StorageSecretKey string
	StorageEndpoint  string // para MinIO / S3-compat não-AWS
	StorageRegion    string
	StorageBucket    string // nome do bucket (informativo; path autoritativo vem do painel no plan)
	StorageID        string // ULID do registro no painel
	// Buckets atendidos por OUTROS processos deste mesmo agent no host (um
	// processo por conjunto de credenciais). Itens de restore desses buckets
	// são pulados em silêncio (o irmão pega); de buckets desconhecidos, falham
	// rápido com erro claro. CSV em SIBLING_BUCKETS.
	SiblingBuckets []string

	// Misc
	HeartbeatIntervalSec int
	PollIntervalSec      int
}

// Overrides representa valores vindos da CLI que têm precedência sobre env.
type Overrides struct {
	EnrollmentToken string
	PanelURL        string
	HostRoot        string
}

// Load lê o env-file (se existir) + variáveis de ambiente + overrides.
// Retorna erro só se o env-file for passado e não puder ser aberto.
func Load(envFile string, o Overrides) (*Config, error) {
	env := make(map[string]string)

	if envFile != "" {
		if f, err := os.Open(envFile); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
			if err := scanner.Err(); err != nil {
				return nil, fmt.Errorf("lendo %s: %w", envFile, err)
			}
		} else if !os.IsNotExist(err) {
			// existe mas falhou ao abrir (permissão, etc) — é erro
			return nil, fmt.Errorf("abrindo %s: %w", envFile, err)
		}
		// se não existe, seguimos com env vazio (primeira instalação)
	}

	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return env[key]
	}

	cfg := &Config{
		AgentID:              get("AGENT_ID"),
		PanelURL:             firstNonEmpty(o.PanelURL, get("PANEL_URL"), "https://save.arkame.app"),
		EnrollmentToken:      firstNonEmpty(o.EnrollmentToken, get("ENROLLMENT_TOKEN")),
		Fingerprint:          get("AGENT_FINGERPRINT"),
		TokenPath:            firstNonEmpty(get("TOKEN_PATH"), "/etc/arkame/token.jwt"),
		PrivateKeyPath:       firstNonEmpty(get("PRIVATE_KEY_PATH"), "/etc/arkame/key.pem"),
		AgentIDPath:          firstNonEmpty(get("AGENT_ID_PATH"), "/etc/arkame/agent.id"),
		HostRoot:             firstNonEmpty(o.HostRoot, get("HOST_ROOT"), "/"),
		StorageAccessKey:     get("STORAGE_ACCESS_KEY"),
		StorageSecretKey:     get("STORAGE_SECRET_KEY"),
		StorageEndpoint:      get("STORAGE_ENDPOINT"),
		StorageRegion:        firstNonEmpty(get("STORAGE_REGION"), "us-east-1"),
		StorageBucket:        get("STORAGE_BUCKET"),
		StorageID:            get("STORAGE_ID"),
		SiblingBuckets:       splitCSV(get("SIBLING_BUCKETS")),
		HeartbeatIntervalSec: 60,
		PollIntervalSec:      60,
	}

	return cfg, nil
}

// TokenExists informa se o JWT bearer já foi persistido (enrollment aprovado).
// Substitui o antigo "zerar TokenPath" — assim o caminho configurado (env-file)
// é preservado para o PersistToken escrever no lugar certo em instalações rootless.
func (c *Config) TokenExists() bool {
	if c.TokenPath == "" {
		return false
	}
	_, err := os.Stat(c.TokenPath)
	return err == nil
}

// LoadToken lê o JWT bearer do disco (TokenPath). Retorna "" se não existe ou vazio.
func (c *Config) LoadToken() (string, error) {
	if c.TokenPath == "" {
		return "", nil
	}
	b, err := os.ReadFile(c.TokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func firstNonEmpty(vv ...string) string {
	for _, v := range vv {
		if v != "" {
			return v
		}
	}
	return ""
}

// splitCSV divide uma lista separada por vírgulas, ignorando vazios e espaços.
func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
