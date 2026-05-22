// Package api define os tipos trocados entre agent e Core API.
// Devem bater com as rotas do painel (apps/save no monorepo).
package api

import "time"

// --- Enrollment ---

// EnrollRequest é enviado pelo agent durante install (first-time ou reinstall).
type EnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	PublicKey       []byte `json:"public_key"`  // Ed25519 32 bytes
	Fingerprint     string `json:"fingerprint"` // "AA:BB:CC:..."
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`             // "linux-amd64"
	AgentVersion    string `json:"agent_version"`  // "1.4.2"
	InstallMethod   string `json:"install_method"` // "docker" | "binary"
}

// EnrollResponse é a resposta do painel ao enrollment request.
// O certificado mTLS NÃO é entregue aqui — o painel precisa esperar aprovação
// humana antes de emitir cert. Este response só confirma que o painel recebeu
// o enrollment e está aguardando.
type EnrollResponse struct {
	AgentID        string    `json:"agent_id"`
	Status         string    `json:"status"` // "pending" | "approved" | "rejected"
	EnrollmentID   string    `json:"enrollment_id"`
	WaitURL        string    `json:"wait_url"` // long-poll URL para aguardar approval
	ExpiresAt      time.Time `json:"expires_at"`
}

// TokenResponse é retornado quando o agent faz long-poll na WaitURL e a approval chega.
//
// Decisão arquitetural (Sprint 5): bearer JWT no lugar de mTLS na fase 1.
// O painel emite um JWT HS256 assinado com AGENT_TOKEN_SECRET; o agent
// envia em todo request via header Authorization: Bearer <token>.
// mTLS volta como hardening de transport em fase futura.
type TokenResponse struct {
	AgentToken string    `json:"agent_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// --- Plans ---

// Plan é a descrição de um plano de backup ou restore enviada ao agent.
type Plan struct {
	ID           string      `json:"id"`
	Kind         string      `json:"kind"` // "backup" | "restore"
	Status       string      `json:"status"`
	SourcePaths  []string    `json:"source_paths,omitempty"`
	ExcludeGlobs []string    `json:"exclude_globs,omitempty"`
	DestPath     string      `json:"dest_path,omitempty"`
	StorageRef   StorageRef  `json:"storage"`
	Schedule     Schedule    `json:"schedule"`
	Windows      []Window    `json:"windows,omitempty"`
	Throttle     Throttle    `json:"throttle,omitempty"`
	NextRunAt    *time.Time  `json:"next_run_at,omitempty"`
}

type StorageRef struct {
	ID         string `json:"id"`
	Bucket     string `json:"bucket"`
	PrefixRoot string `json:"prefix_root"`
}

type Schedule struct {
	Type        string `json:"type"` // manual | daily | hourly | cron
	Cron        string `json:"cron,omitempty"`
	DailyTime   string `json:"daily_time,omitempty"`   // "03:00"
	HourlyEvery int    `json:"hourly_every,omitempty"` // horas
}

type Window struct {
	Days    []string `json:"days"`    // ["mon","tue","wed","thu","fri"]
	Start   string   `json:"start"`   // "22:00"
	End     string   `json:"end"`     // "06:00"
	AllDay  bool     `json:"all_day,omitempty"`
}

type Throttle struct {
	MaxMbps      int `json:"max_mbps,omitempty"`
	PauseCPUOver int `json:"pause_cpu_over,omitempty"` // %
}

// --- Heartbeat ---

type HeartbeatRequest struct {
	AgentID      string    `json:"agent_id"`
	AgentVersion string    `json:"agent_version"`
	OS           string    `json:"os"`
	ReportedAt   time.Time `json:"reported_at"`
}

// --- Session / Manifest ---

// SessionStart é enviado quando uma sessão de backup começa.
type SessionStart struct {
	PlanID            string   `json:"plan_id"`
	AgentID           string   `json:"agent_id"`
	SourcePaths       []string `json:"source_paths"`
	ConsistencyMethod string   `json:"consistency_method"`
}

// SessionComplete é enviado ao fim da sessão com o version_map já
// escrito no bucket. O agent também POSTa o conteúdo inline aqui
// para que o painel possa indexar no Postgres sem ler o bucket.
type SessionComplete struct {
	SessionID     string          `json:"session_id"`
	Status        string          `json:"status"` // complete | partial | failed
	Stats         SessionStats    `json:"stats"`
	ManifestKey   string          `json:"manifest_key"`
	VersionMapKey string          `json:"version_map_key"`
	VersionMap    []FileEntry     `json:"version_map"` // inline — permite indexação sem acesso ao bucket
	ErrorCode     string          `json:"error_code,omitempty"`
	ErrorMessage  string          `json:"error_message,omitempty"`
}

type SessionStats struct {
	FilesTotal    int64 `json:"files_total"`
	FilesUploaded int64 `json:"files_uploaded"`
	FilesDeleted  int64 `json:"files_deleted"`
	BytesUploaded int64 `json:"bytes_uploaded"`
	BytesTotal    int64 `json:"bytes_total"`
}

type FileEntry struct {
	Key        string    `json:"key"`
	VersionID  string    `json:"version_id"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"` // hex
	ModifiedAt time.Time `json:"modified_at"`
}

// --- Storage probe (discovered config) ---

type ProbeReport struct {
	StorageID    string     `json:"storage_id"`
	Versioning   string     `json:"versioning"` // "Enabled" | "Suspended" | "Off"
	ObjectLock   *ObjectLock `json:"object_lock,omitempty"`
	Lifecycle    []Lifecycle `json:"lifecycle,omitempty"`
	ProbedAt     time.Time  `json:"probed_at"`
	Error        string     `json:"error,omitempty"`
}

type ObjectLock struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode,omitempty"` // GOVERNANCE | COMPLIANCE
	Days    int    `json:"days,omitempty"`
}

type Lifecycle struct {
	Prefix          string   `json:"prefix,omitempty"`
	Transitions     []string `json:"transitions,omitempty"`
	ExpirationDays  int      `json:"expiration_days,omitempty"`
}

// --- Restore ---

// RestoreItem é uma linha pendente em restore_items.
// O agent puxa items via GET /api/agents/<id>/restore-items, baixa cada um do
// bucket via GetObject(VersionId) e reporta status via PATCH.
type RestoreItem struct {
	ItemID           string `json:"item_id"`
	JobID            string `json:"job_id"`
	Bucket           string `json:"bucket"`
	DestPath         string `json:"dest_path"`         // diretório destino absoluto
	ConflictStrategy string `json:"conflict_strategy"` // suffix-version | overwrite | skip
	SourceKey        string `json:"source_key"`        // S3 key dentro do bucket
	SourceVersionID  string `json:"source_version_id"` // S3 VersionId
	SourceSize       int64  `json:"source_size"`
	SourceSha256     string `json:"source_sha256"` // hex
	DestFilename     string `json:"dest_filename"` // basename a escrever em dest_path
	Status           string `json:"status"`        // queued | running | complete | failed
}

// ListRestoreItemsResponse é a resposta de GET /api/agents/<id>/restore-items.
type ListRestoreItemsResponse struct {
	Items []RestoreItem `json:"items"`
}

// RestoreItemUpdate é o body de PATCH /api/agents/<id>/restore-items/<itemId>.
type RestoreItemUpdate struct {
	Status       string `json:"status"` // running | complete | failed
	ErrorMessage string `json:"error_message,omitempty"`
}

// RestoreItemUpdateResponse retorna o estado agregado do job após o update.
type RestoreItemUpdateResponse struct {
	OK          bool   `json:"ok"`
	JobID       string `json:"job_id"`
	JobStatus   string `json:"job_status"`
	ItemsDone   int    `json:"items_done"`
	ItemsFailed int    `json:"items_failed"`
	ItemsTotal  int    `json:"items_total"`
}
