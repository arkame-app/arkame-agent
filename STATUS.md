# Arkame Agent — Status

**Status:** Sprints 5 (enrollment + bearer auth), 6 (sync engine + probe reporter), Tier 1 C (restore execution) e Tier 1 follow-ups (warming + dedup + multipart download) **concluídos**. Build limpo (`go vet ./...`).

> Trabalho ativo principal está no painel (`~/hugo-projects/arkame/`). Para o status global do produto, ver `~/hugo-projects/arkame/STATUS.md`.

## O que está pronto

- **Enrollment Ed25519**: `internal/enrollment` gera keypair, POST `/api/agents/enroll`, long-poll na `wait-token` até receber JWT bearer
- **Bearer auth**: client HTTP envia `Authorization: Bearer <token>` em todos os requests pós-approval; `ErrNotReady` (204) e `ErrGone` (410) pra long-poll handling
- **Persistência local**: `/etc/arkame/token.jwt` (0600) + `/etc/arkame/key.pem` (0600) + `/etc/arkame/agent.id`
- **Daemon completo** (`internal/daemon/daemon.go`), 4 loops paralelos:
  - Loop heartbeat 60s `/api/agents/{id}/heartbeat`
  - Loop probe 1h: `storage.Probe` → POST `/probe` (versioning, object_lock, lifecycle)
  - Loop plans 60s: GET `/plans` → `scheduler.ShouldRun` → `executePlan` (backup)
  - Loop restore 60s: GET `/restore-items` → PATCH running → `restore.Run` → PATCH complete/failed (Tier 1 C)
- **`executePlan` (backup)**: POST `/sessions/start` → `sync.Run` (walker + hash + dedup HeadObject + S3 PutObject + version_map) → POST `/sessions/{sid}/complete` com version_map inline; em falha total POST `/sessions/{sid}/fail`
- **Dedup file-level**: antes de PutObject, faz HeadObject e compara `sha256` no metadata. Hit retorna FileEntry com VersionId existente sem subir bytes; stats `FilesUploaded` não conta dedup hits
- **`restore.Run` (restore)**: `internal/restore/executor.go` — escrita atômica (tmp + rename) com SHA-256 verify → conflict resolution `suffix-version`/`overwrite`/`skip`; respeita `HOST_ROOT`
- **Multipart download**: arquivos >= 100 MB usam `s3manager.Downloader` (4 workers, parts 16 MiB); hash é calculado relendo o tmp file
- **Warming cold storage**: GetObject que falha com `InvalidObjectState` → HeadObject pra checar `x-amz-restore` → RestoreObject (Standard tier, 7d) se necessário; daemon mantém item como `running` pra re-tentar no próximo poll (ErrWarmingRequested / ErrWarmingInProgress)
- **Comandos CLI**: `install` (com `--wait` default true), `run`, `status`, `heartbeat` (one-shot pra testar auth), `version`

## Versionado no GitHub

Repo privado em [`arkame-app/arkame-agent`](https://github.com/arkame-app/arkame-agent). Branch principal `main`. Auth SSH (`hugolf`).

## O que falta (próximos passos)

- **Self-update** do binário em produção (fase posterior)
- **mTLS hardening** (fase 2) — substituir bearer JWT por mTLS com CA do painel, sem quebrar o contrato atual
- **Multipart upload** pra arquivos grandes no backup (sync engine usa `PutObject` simples)
- **VSS no Windows** pra snapshots consistentes (Linux LVM também no roadmap)
- **Persistir warming_state em restore_items**: PATCH atual só atualiza `status`. Adicionar endpoint dedicado pra warming_state/warming_tier/warming_requested_at quando relevante.

## Como compilar (sem Go local)

```bash
cd ~/hugo-projects/arkame-agent
docker run --rm -v "$PWD:/src" -w /src golang:1.24-alpine sh -c "go build ./..."
```

## Veja também

- `README.md` aqui — arquitetura completa
- `~/hugo-projects/arkame/STATUS.md` — estado do painel e produto
- `~/hugo-projects/arkame/PLAN.md` — histórico de decisões (32 rounds)
