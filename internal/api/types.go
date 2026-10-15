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
	AgentID      string    `json:"agent_id"`
	Status       string    `json:"status"` // "pending" | "approved" | "rejected"
	EnrollmentID string    `json:"enrollment_id"`
	WaitURL      string    `json:"wait_url"` // long-poll URL para aguardar approval
	ExpiresAt    time.Time `json:"expires_at"`
	// Armazenamento a que o código de instalação amarra o servidor. Opcionais:
	// painel que não os manda deixa STORAGE_ID/STORAGE_BUCKET do arquivo como
	// estão.
	StorageID     string `json:"storage_id,omitempty"`
	StorageBucket string `json:"storage_bucket,omitempty"`
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
	ID           string     `json:"id"`
	Kind         string     `json:"kind"` // "backup" | "restore"
	Status       string     `json:"status"`
	SourcePaths  []string   `json:"source_paths,omitempty"`
	ExcludeGlobs []string   `json:"exclude_globs,omitempty"`
	DestPath     string     `json:"dest_path,omitempty"`
	StorageRef   StorageRef `json:"storage"`
	Schedule     Schedule   `json:"schedule"`
	Windows      []Window   `json:"windows,omitempty"`
	Throttle     Throttle   `json:"throttle,omitempty"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`

	// Comandos que o plano roda no servidor antes e depois do backup.
	//
	// Existem pelo caso que a vitrine promete: `pg_dump` antes de copiar.
	// Backup do arquivo de um banco aberto é backup de algo que não restaura.
	PreHook  string `json:"pre_hook,omitempty"`
	PostHook string `json:"post_hook,omitempty"`
	// Prazo de cada comando, em segundos. Zero usa o padrão do executor.
	HookTimeoutSeconds int `json:"hook_timeout_seconds,omitempty"`
	// Snapshot: o instantâneo do volume antes de ler (VSS, no Windows).
	// "auto" ou vazio (painel de antes do campo) tenta; "off" lê do disco.
	Snapshot string `json:"snapshot,omitempty"`
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
	Days   []string `json:"days"`  // ["mon","tue","wed","thu","fri"]
	Start  string   `json:"start"` // "22:00"
	End    string   `json:"end"`   // "06:00"
	AllDay bool     `json:"all_day,omitempty"`
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
	// ServiceName/ServiceScope: como o agent está sendo executado, para o painel
	// mostrar o comando exato de reinício quando ele cair. Vazio se rodando à mão.
	ServiceName  string `json:"service_name,omitempty"`
	ServiceScope string `json:"service_scope,omitempty"`
	// ProgramPath: o executável deste agente, com links resolvidos. O
	// instalador nem sempre o põe em /usr/local/bin (com root e um
	// /usr/local/bin que não é só do root, vai a /opt/arkame/bin), e o painel
	// precisa do caminho real para montar o comando de trocar a chave. Vazio
	// se o SO não disse.
	ProgramPath string `json:"program_path,omitempty"`
	// Capabilities: o que este agente sabe fazer além do protocolo de base
	// ("self_update", …). O painel grava e só oferece o que foi declarado.
	// Vai sempre como lista (vazia, nunca ausente): ausente é agente antigo,
	// de antes das capacidades.
	Capabilities []string `json:"capabilities"`
	// Update: o que este agente tem a dizer sobre a atualização do próprio
	// programa. Ausente quando não há nada.
	Update *UpdateReport `json:"update,omitempty"`
	// SelfTest: heartbeat do auto-teste do programa novo, antes da troca. O
	// painel só confere o token e responde; não grava versão nem horário —
	// quem está de pé ainda é o programa antigo.
	SelfTest bool `json:"self_test,omitempty"`
	// LauncherVersion: no Docker, a versão do programa da imagem quando quem
	// roda é o programa baixado para o volume.
	LauncherVersion string `json:"launcher_version,omitempty"`
}

// UpdateReport é o relato do agente sobre a atualização do próprio programa.
type UpdateReport struct {
	// State: "downloading" (baixando e conferindo), "deferred" (adiada: há
	// backup, expurgo ou restauração em curso), "proving" (o programa novo
	// está no lugar, em prova), "failed" (a troca não aconteceu) ou
	// "rolled_back" (o programa novo não provou que funciona e o anterior
	// voltou). Vazio com Blocked preenchido: só o impedimento.
	State string `json:"state,omitempty"`
	// Version é a versão a que o estado se refere.
	Version string `json:"version,omitempty"`
	// Error é o código do motivo (falha, adiamento ou reversão).
	Error string `json:"error,omitempty"`
	// At é de quando é o estado relatado. O agente segue relatando a falha ou
	// a reversão de uma tentativa até a próxima (a reversão fica no registro
	// do disco): com o instante, o painel não toma o relato antigo pelo
	// resultado de uma oferta nova. Ausente no agente anterior a este campo.
	At *time.Time `json:"at,omitempty"`
	// AgeSeconds é há quantos segundos o estado relatado é o atual, medido no
	// relógio deste servidor no instante do envio. É o que o painel usa de
	// preferência ao At: uma idade não depende de o relógio do servidor estar
	// certo — e servidor com o relógio horas fora não é raro.
	AgeSeconds *int64 `json:"age_s,omitempty"`
	// Blocked: por que este agente não se atualiza sozinho ("disabled",
	// "binary_dir_readonly", "no_service", …). Com ele o agente não declara
	// a capacidade "self_update".
	Blocked string `json:"blocked,omitempty"`
}

// HeartbeatResponse é a resposta do heartbeat.
type HeartbeatResponse struct {
	OK         bool   `json:"ok"`
	ServerTime string `json:"server_time,omitempty"`
	// NewToken vem quando o token atual está perto de vencer (o painel renova
	// com 90 dias de antecedência). Ausente: nada muda.
	NewToken string `json:"new_token,omitempty"`
	// Update vem só quando este agente deve atualizar o próprio programa
	// agora (declarou "self_update", a política da organização permite, está
	// na janela). Ausente: nada a fazer. Agente antigo ignora o campo.
	Update *UpdateOffer `json:"update,omitempty"`
}

// UpdateOffer é a atualização que o painel manda aplicar.
type UpdateOffer struct {
	Version string `json:"version"` // "0.5.0"
	// SHA256 do programa deste sistema, segundo o painel: tem de ser igual ao
	// do checksums.txt assinado do release.
	SHA256 string `json:"sha256"`
	// BaseURL de onde baixar: <base_url>/v<version>/<file>.
	BaseURL string `json:"base_url"`
	// File é o nome do programa no release (arkame-agent_linux_amd64).
	File string `json:"file"`
	// Force: tentar de novo uma versão que esta máquina já pôs e reverteu.
	Force bool `json:"force,omitempty"`
}

// --- Session / Manifest ---

// SessionStart é enviado quando uma sessão de backup começa.
type SessionStart struct {
	PlanID      string   `json:"plan_id"`
	AgentID     string   `json:"agent_id"`
	SourcePaths []string `json:"source_paths"`
	// ExcludeGlobs: as exclusões que o walker usa nesta sessão (sempre lista,
	// vazia se não houver). O painel grava estas, não as do plano no /start.
	ExcludeGlobs []string `json:"exclude_globs"`
	// PrefixRoot: o prefixo de chave do armazenamento com que esta sessão
	// monta as chaves (sempre presente, "" se as chaves ficam na raiz). O
	// painel lê a seleção da sessão sob ele, não sob o prefixo em vigor.
	PrefixRoot        string `json:"prefix_root"`
	ConsistencyMethod string `json:"consistency_method"`
	// Capabilities: as mesmas do heartbeat (ver HeartbeatRequest).
	Capabilities []string `json:"capabilities"`
}

// SessionComplete é enviado ao fim da sessão com o version_map já
// escrito no bucket. O agent também POSTa o conteúdo inline aqui
// para que o painel possa indexar no Postgres sem ler o bucket.
type SessionComplete struct {
	SessionID     string       `json:"session_id"`
	Status        string       `json:"status"` // complete | partial | failed
	Stats         SessionStats `json:"stats"`
	ManifestKey   string       `json:"manifest_key"`
	VersionMapKey string       `json:"version_map_key"`
	VersionMap    []FileEntry  `json:"version_map"` // inline — permite indexação sem acesso ao bucket
	ErrorCode     string       `json:"error_code,omitempty"`
	ErrorMessage  string       `json:"error_message,omitempty"`
}

type SessionStats struct {
	FilesTotal    int64 `json:"files_total"`
	FilesUploaded int64 `json:"files_uploaded"`
	FilesDeleted  int64 `json:"files_deleted"`
	BytesUploaded int64 `json:"bytes_uploaded"`
	BytesTotal    int64 `json:"bytes_total"`
	// Estatísticas do índice em lotes (opcionais no contrato). Ponteiros: só
	// vão quando o índice foi em lotes — no /complete inline o corpo é o de
	// sempre.
	FilesNew         *int64 `json:"files_new,omitempty"`
	FilesChanged     *int64 `json:"files_changed,omitempty"`
	FilesRemoved     *int64 `json:"files_removed,omitempty"`
	FilesUnselected  *int64 `json:"files_unselected,omitempty"`
	ChangedSameMtime *int64 `json:"changed_same_mtime,omitempty"`
}

type FileEntry struct {
	Key        string    `json:"key"`
	VersionID  string    `json:"version_id"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"` // hex
	ModifiedAt time.Time `json:"modified_at"`
}

// --- Índice em lotes e delta (contrato: escala, fase 2) ---

// Capacidades do índice que o agente declara no heartbeat e no
// /sessions/start. O painel só oferece o que foi declarado.
const (
	CapacidadeIndiceEmLotes = "index_batches"
	CapacidadeIndiceDelta   = "index_delta"
	// CapacidadeRemovidosEmParcial: o agente sabe emitir `removed` em sessão
	// parcial só com prova positiva de que o arquivo saiu (contrato, item
	// 27). O painel que a conhece responde com IndexOffer.RemovedPartial.
	CapacidadeRemovidosEmParcial = "index_removed_partial"
)

// Operações de uma entrada do índice em lotes.
const (
	OpUpsert     = "upsert"     // presente nesta sessão com esta versão
	OpRemoved    = "removed"    // estava na base e saiu da origem
	OpUnselected = "unselected" // estava na base e saiu da seleção do plano
)

// IndexOffer é o `index` da resposta do /sessions/start. Ausente (ou Mode
// "inline"): painel de antes do índice em lotes, e o /complete leva o
// version_map inteiro, como sempre.
type IndexOffer struct {
	Mode       string `json:"mode"` // "inline" | "batches"
	Delta      bool   `json:"delta"`
	MaxEntries int    `json:"max_entries"`
	MaxBytes   int    `json:"max_bytes"`
	// BaseSessionID é a última sessão concluída deste agente + plano, ou
	// vazio. O delta é relativo a ela.
	BaseSessionID string `json:"base_session_id"`
	// RemovedPartial: o painel aplica o `removed` desta sessão mesmo que ela
	// feche `partial`. Só vem para quem declarou
	// CapacidadeRemovidosEmParcial e recebeu delta. Ausente (painel de antes):
	// sessão parcial não emite `removed`.
	RemovedPartial bool `json:"removed_partial"`
}

// IndexEntry é uma entrada de um lote do índice
// (POST /api/agents/{id}/sessions/{sid}/files).
type IndexEntry struct {
	Op         string     `json:"op"`
	Key        string     `json:"key"`
	VersionID  string     `json:"version_id,omitempty"`
	Size       *int64     `json:"size,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
}

// Upsert monta a entrada de um arquivo presente na sessão.
func Upsert(e FileEntry) IndexEntry {
	tam, data := e.Size, e.ModifiedAt
	return IndexEntry{Op: OpUpsert, Key: e.Key, VersionID: e.VersionID, Size: &tam, SHA256: e.SHA256, ModifiedAt: &data}
}

// IndexBatch é o corpo de um lote.
type IndexBatch struct {
	Seq     int          `json:"seq"`
	Entries []IndexEntry `json:"entries"`
}

// IndexBatchResponse é a resposta 200 de um lote.
type IndexBatchResponse struct {
	OK       bool  `json:"ok"`
	Seq      int   `json:"seq"`
	Received int64 `json:"received"`
}

// IndexSummary é o `index` do /complete quando o índice foi em lotes.
type IndexSummary struct {
	Mode          string  `json:"mode"` // "full" | "delta"
	BaseSessionID *string `json:"base_session_id"`
	Batches       int     `json:"batches"`
	EntriesSent   int64   `json:"entries_sent"`
}

// --- Storage probe (discovered config) ---

// ProbeReport.UsedBytes e ObjectCount são nil quando a listagem do bucket
// falhou: vão ausentes no JSON, e não 0 ("0 B" no painel).
type ProbeReport struct {
	StorageID   string      `json:"storage_id"`
	Versioning  string      `json:"versioning"` // "Enabled" | "Suspended" | "Off"
	ObjectLock  *ObjectLock `json:"object_lock,omitempty"`
	Lifecycle   []Lifecycle `json:"lifecycle,omitempty"`
	UsedBytes   *int64      `json:"used_bytes,omitempty"`   // soma de todas as versões do bucket (ocupação real)
	ObjectCount *int64      `json:"object_count,omitempty"` // nº de arquivos presentes (versões atuais)
	// Resumo das regras ativas sobre versões não-atuais — o que de fato apaga
	// ou congela o histórico de um bucket versionado. NoncurrentExpirationDays
	// é o menor NoncurrentDays entre as regras ativas (ausente se nenhuma
	// expira versões antigas); NoncurrentTransitions, as classes de destino.
	NoncurrentExpirationDays *int      `json:"noncurrent_expiration_days,omitempty"`
	NoncurrentTransitions    []string  `json:"noncurrent_transitions,omitempty"`
	ProbedAt                 time.Time `json:"probed_at"`
	Error                    string    `json:"error,omitempty"`
	// LifecycleError e ObjectLockError: a leitura das regras de ciclo de vida
	// ou do Object Lock falhou por outro motivo que não "o bucket não tem"
	// (tipicamente AccessDenied: a chave sem s3:GetLifecycleConfiguration ou
	// s3:GetBucketObjectLockConfiguration). Texto "Código: mensagem" do
	// provedor; ausente quando a leitura deu certo ou o bucket não tem a
	// configuração. Com ele preenchido, Lifecycle/ObjectLock ausentes querem
	// dizer "não sei", e não "não há" — o painel deve avisar. A partir do
	// agente 0.4.11.
	LifecycleError  string `json:"lifecycle_error,omitempty"`
	ObjectLockError string `json:"object_lock_error,omitempty"`
}

type ObjectLock struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode,omitempty"` // GOVERNANCE | COMPLIANCE
	Days    int    `json:"days,omitempty"`
}

// Lifecycle é uma regra ativa (Status Enabled) do ciclo de vida do bucket.
type Lifecycle struct {
	Prefix                   string   `json:"prefix,omitempty"`
	Transitions              []string `json:"transitions,omitempty"`
	ExpirationDays           int      `json:"expiration_days,omitempty"`
	NoncurrentExpirationDays int      `json:"noncurrent_expiration_days,omitempty"`
	NoncurrentTransitions    []string `json:"noncurrent_transitions,omitempty"`
}

// --- Restore ---

// RestoreItem é uma linha pendente em restore_items.
// O agent puxa items via GET /api/agents/<id>/restore-items, baixa cada um do
// bucket via GetObject(VersionId) e reporta status via PATCH.
type RestoreItem struct {
	ItemID string `json:"item_id"`
	JobID  string `json:"job_id"`
	Bucket string `json:"bucket"`
	// StorageID é o armazenamento do item. Opcional: painel antigo não manda,
	// e aí só o bucket separa os processos irmãos.
	StorageID        string `json:"storage_id,omitempty"`
	DestPath         string `json:"dest_path"`         // diretório destino absoluto
	ConflictStrategy string `json:"conflict_strategy"` // suffix-version | overwrite | skip
	SourceKey        string `json:"source_key"`        // S3 key dentro do bucket
	SourceVersionID  string `json:"source_version_id"` // S3 VersionId
	SourceSize       int64  `json:"source_size"`
	SourceSha256     string `json:"source_sha256"` // hex
	DestFilename     string `json:"dest_filename"` // basename a escrever em dest_path
	Status           string `json:"status"`        // queued | running | complete | failed
	// SourceModifiedAt é a data de modificação do arquivo no backup
	// (file_versions.modified_at). Opcional: sem ela, o arquivo restaurado
	// fica com a data da restauração.
	SourceModifiedAt *time.Time `json:"source_modified_at,omitempty"`
}

// ListRestoreItemsResponse é a resposta de GET /api/agents/<id>/restore-items.
type ListRestoreItemsResponse struct {
	Items []RestoreItem `json:"items"`
}

// RestoreItemUpdate é o body de PATCH /api/agents/<id>/restore-items/<itemId>.
type RestoreItemUpdate struct {
	Status       string `json:"status"` // running | complete | skipped | failed
	ErrorMessage string `json:"error_message,omitempty"`
	// ErrorCode estruturado; "not_found" sinaliza ao painel que o objeto sumiu do
	// bucket — o índice é reconciliado (versão marcada como indisponível).
	ErrorCode string `json:"error_code,omitempty"`
	// Warming: cold storage (Glacier/Archive). Status segue "running" enquanto aquece.
	WarmingState string `json:"warming_state,omitempty"` // requested | in_progress
	WarmingTier  string `json:"warming_tier,omitempty"`
	WarmingETA   string `json:"warming_eta,omitempty"` // RFC3339
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

// FsRequest é uma requisição de listagem de diretório vinda do painel
// (explorador de pastas do wizard de plano).
type FsRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// FsRequestsResponse é a resposta de GET /api/agents/<id>/fs-requests (long-poll).
type FsRequestsResponse struct {
	Requests []FsRequest `json:"requests"`
}

// FsEntry é um item de diretório reportado ao painel.
type FsEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

// FsListingReport é o body de POST /api/agents/<id>/fs-requests/<reqId>.
type FsListingReport struct {
	// Sem omitempty: pasta vazia vai como [] — sem o campo, o explorador do
	// painel entendia que a listagem não tinha chegado e ficava girando.
	Entries []FsEntry `json:"entries"`
	Error   string    `json:"error,omitempty"`
}

// --- Retenção: expurgo de versões (painel autoriza, agent executa) ---

// PurgePlanVersion é uma versão que o painel autorizou apagar.
type PurgePlanVersion struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
	Reason    string `json:"reason"` // thinning | hard_delete | deleted_file
}

// PurgePlanResponse é a resposta de GET /api/agents/{id}/purge-plan.
// RunID vazio significa "nada a fazer agora" — o caso comum.
type PurgePlanResponse struct {
	RunID      string             `json:"run_id"`
	StorageID  string             `json:"storage_id"`
	Bucket     string             `json:"bucket"`
	PrefixRoot string             `json:"prefix_root"`
	Truncated  bool               `json:"truncated"`
	Versions   []PurgePlanVersion `json:"versions"`
}

// PurgeDeleted identifica uma versão que saiu do bucket.
type PurgeDeleted struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
}

// PurgeFailure identifica uma versão que não saiu, e por quê.
type PurgeFailure struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Error     string `json:"error"`
}

// PurgeResult é o corpo de POST /api/agents/{id}/purge-result.
type PurgeResult struct {
	RunID   string         `json:"run_id"`
	Deleted []PurgeDeleted `json:"deleted"`
	Failed  []PurgeFailure `json:"failed,omitempty"`
}

// PurgeResultResponse é a confirmação do painel — o que ele aceitou dar baixa.
type PurgeResultResponse struct {
	VersionsDeleted    int   `json:"versions_deleted"`
	BytesFreed         int64 `json:"bytes_freed"`
	CatalogRowsRemoved int   `json:"catalog_rows_removed"`
	Failed             int   `json:"failed"`
	Rejected           int   `json:"rejected"`
}

// --- Teste de restauração por amostra (contrato, itens 34 a 38) ---

// CapacidadeVerificarRestauracao: o agente sabe restaurar uma amostra que o
// painel escolhe, conferir e relatar. Só quem a declara recebe plano.
const CapacidadeVerificarRestauracao = "verify_restore"

// VerifyItem é uma versão que o painel escolheu para a amostra.
type VerifyItem struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"` // hex, 64
}

// VerifyPlanResponse é a resposta de GET /api/agents/{id}/verify-plan.
// RunID vazio ou Items vazio = nada a fazer.
type VerifyPlanResponse struct {
	RunID           string       `json:"run_id"`
	StorageID       string       `json:"storage_id"`
	Bucket          string       `json:"bucket"`
	PrefixRoot      string       `json:"prefix_root"`
	MaxBytes        int64        `json:"max_bytes"`
	DeadlineSeconds int64        `json:"deadline_seconds"`
	Items           []VerifyItem `json:"items"`
}

// VerifyResultItem é o que aconteceu com um item da amostra. Status: "ok",
// "failed" ou "skipped"; Error é o código (not_found, hash_mismatch, cold…).
// SHA256 é o do arquivo relido do disco, e só vai em "ok".
type VerifyResultItem struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Bytes     int64  `json:"bytes"`
	Ms        int64  `json:"ms"`
	SHA256    string `json:"sha256,omitempty"`
}

// VerifyResult é o corpo de POST /api/agents/{id}/verify-result.
type VerifyResult struct {
	RunID      string             `json:"run_id"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Items      []VerifyResultItem `json:"items"`
}

// VerifyResultResponse é o que o painel registrou da rodada.
type VerifyResultResponse struct {
	Status            string `json:"status"`
	OK                int    `json:"ok"`
	Failed            int    `json:"failed"`
	Skipped           int    `json:"skipped"`
	Rejected          int    `json:"rejected"`
	MarkedUnavailable int    `json:"marked_unavailable"`
	Duplicate         bool   `json:"duplicate,omitempty"`
}

// --- Inventário do bucket (contrato, itens 48 a 52) ---

// CapacidadeInventariarBucket: o agente sabe conferir o catálogo que o painel
// entrega em páginas contra a listagem do bucket. Só quem a declara recebe
// plano de inventário.
const CapacidadeInventariarBucket = "bucket_inventory"

// CapacidadeInstantaneoDoVolume: o agente lê o backup de um instantâneo do
// volume (VSS, no Windows de 64 bits). O painel só mostra a opção do plano a
// quem a declara.
const CapacidadeInstantaneoDoVolume = "volume_snapshot"

// CapacidadePastaDeDump: o agente tem `prepare-dump-folder` e passa o próprio
// caminho aos comandos do plano (ARKAME_AGENT). A receita do SQL Server no
// painel depende dos dois.
const CapacidadePastaDeDump = "dump_folder"

// InventoryItem é uma versão que o catálogo do painel diz existir.
type InventoryItem struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
	// Unavailable: o painel já tem a versão por indisponível. Se ela estiver
	// no bucket com o tamanho certo, vai em found_again (e a marca é desfeita);
	// se continuar ausente, não é relatada de novo.
	Unavailable bool `json:"unavailable,omitempty"`
}

// InventoryPlanResponse é a resposta de GET /api/agents/{id}/inventory-plan:
// uma página do plano. RunID vazio = nada a fazer. Next vazio = última página.
type InventoryPlanResponse struct {
	RunID           string          `json:"run_id"`
	StorageID       string          `json:"storage_id"`
	Bucket          string          `json:"bucket"`
	PrefixRoot      string          `json:"prefix_root"`
	Prefix          string          `json:"prefix"`
	Page            int             `json:"page"`
	DeadlineSeconds int64           `json:"deadline_seconds"`
	SampleSize      int             `json:"sample_size"`
	Items           []InventoryItem `json:"items"`
	Next            string          `json:"next"`
}

// InventoryVersion aponta uma versão num relato de inventário.
type InventoryVersion struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
}

// InventorySize é uma versão que está no bucket com tamanho diferente do
// catálogo; Size é o do bucket.
type InventorySize struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
}

// InventoryKey é uma chave com versão catalogada presente e marcador de
// exclusão por cima.
type InventoryKey struct {
	Key string `json:"key"`
}

// InventorySampleItem é o que o HeadObject de um objeto da amostra respondeu.
// Quem compara com o catálogo e com a política de Object Lock é o painel.
type InventorySampleItem struct {
	Key          string     `json:"key"`
	VersionID    string     `json:"version_id"`
	SHA256       string     `json:"sha256,omitempty"` // o metadado do objeto
	LockMode     string     `json:"lock_mode,omitempty"`
	RetainUntil  *time.Time `json:"retain_until,omitempty"`
	LastModified *time.Time `json:"last_modified,omitempty"`
}

// InventorySample é a amostra de HeadObject da rodada.
type InventorySample struct {
	Checked int                   `json:"checked"`
	Errors  int                   `json:"errors"`
	Items   []InventorySampleItem `json:"items,omitempty"`
}

// InventoryResult é o corpo de POST /api/agents/{id}/inventory-result.
//
// Complete só é verdade quando o prefixo foi listado até o fim e o plano foi
// consumido até a última página; senão Interrupted diz por quê, e nenhuma
// lista de faltantes vai junto. Truncated: uma lista que importa (missing,
// size_mismatch, found_again) foi cortada para caber.
type InventoryResult struct {
	RunID       string    `json:"run_id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Complete    bool      `json:"complete"`
	Interrupted string    `json:"interrupted,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	Versioning  string    `json:"versioning,omitempty"`

	PlanVersions   int64 `json:"plan_versions"`
	Found          int64 `json:"found"`
	FoundBytes     int64 `json:"found_bytes"`
	BucketVersions int64 `json:"bucket_versions"`
	BucketBytes    int64 `json:"bucket_bytes"`
	ExtraVersions  int64 `json:"extra_versions"`
	ListRequests   int64 `json:"list_requests"`
	HeadRequests   int64 `json:"head_requests"`
	Unconfirmed    int64 `json:"unconfirmed"`

	Missing            []InventoryVersion `json:"missing,omitempty"`
	MissingTotal       int64              `json:"missing_total"`
	SizeMismatch       []InventorySize    `json:"size_mismatch,omitempty"`
	SizeMismatchTotal  int64              `json:"size_mismatch_total"`
	DeleteMarkers      []InventoryKey     `json:"delete_markers,omitempty"`
	DeleteMarkersTotal int64              `json:"delete_markers_total"`
	FoundAgain         []InventoryVersion `json:"found_again,omitempty"`
	Sample             InventorySample    `json:"sample"`
	Truncated          bool               `json:"truncated,omitempty"`
}

// InventoryResultResponse é o que o painel registrou da rodada.
type InventoryResultResponse struct {
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	Missing           int    `json:"missing"`
	SizeMismatch      int    `json:"size_mismatch"`
	Rejected          int    `json:"rejected"`
	MarkedUnavailable int    `json:"marked_unavailable"`
	Unmarked          int    `json:"unmarked"`
	Duplicate         bool   `json:"duplicate,omitempty"`
}
