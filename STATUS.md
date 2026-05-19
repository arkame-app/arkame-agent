# Arkame Agent — Status

**Status:** Sprints 5 (enrollment + bearer auth) e 6 (sync engine + probe reporter) **concluídas**. Build limpo (`go vet ./...`).

> Trabalho ativo principal está no painel (`~/hugo-projects/arkame/`). Para o status global do produto, ver `~/hugo-projects/arkame/STATUS.md`.

## O que está pronto

- **Enrollment Ed25519**: `internal/enrollment` gera keypair, POST `/api/agents/enroll`, long-poll na `wait-token` até receber JWT bearer
- **Bearer auth**: client HTTP envia `Authorization: Bearer <token>` em todos os requests pós-approval; `ErrNotReady` (204) e `ErrGone` (410) pra long-poll handling
- **Persistência local**: `/etc/arkame/token.jwt` (0600) + `/etc/arkame/key.pem` (0600) + `/etc/arkame/agent.id`
- **Daemon completo** (`internal/daemon/daemon.go`):
  - Loop heartbeat 60s `/api/agents/{id}/heartbeat`
  - Loop probe 1h: `storage.Probe` → POST `/probe` (versioning, object_lock, lifecycle)
  - Loop plans 60s: GET `/plans` → `scheduler.ShouldRun` → `executePlan`
  - `executePlan`: POST `/sessions/start` → `sync.Run` (walker + hash + S3 PutObject + version_map) → POST `/sessions/{sid}/complete` com version_map inline; em falha total POST `/sessions/{sid}/fail`
- **Comandos CLI**: `install` (com `--wait` default true), `run`, `status`, `heartbeat` (one-shot pra testar auth), `version`

## Versionado no GitHub

Repo privado em [`arkame-app/arkame-agent`](https://github.com/arkame-app/arkame-agent). Branch principal `main`. Auth SSH (`hugolf`).

## O que falta (próximos passos)

- **Restore execution** (Tier 1 C do roadmap): daemon consome `plan.kind=restore`, baixa items do bucket (S3 GetObject por versionId), escreve em `destPath`, reporta progresso. Pré-requisito: tabela `restore_items` no painel (Tier 1 D), pra agent saber quais arquivos baixar.
- **Self-update** do binário em produção (fase posterior)
- **mTLS hardening** (fase 2) — substituir bearer JWT por mTLS com CA do painel, sem quebrar o contrato atual
- **Multipart upload** pra arquivos grandes (engine atual usa `PutObject` simples)
- **Dedup file-level**: checar HeadObject + sha256 metadata antes de re-upload

## Como compilar (sem Go local)

```bash
cd ~/hugo-projects/arkame-agent
docker run --rm -v "$PWD:/src" -w /src golang:1.24-alpine sh -c "go build ./..."
```

## Veja também

- `README.md` aqui — arquitetura completa
- `~/hugo-projects/arkame/STATUS.md` — estado do painel e produto
- `~/hugo-projects/arkame/PLAN.md` — histórico de decisões (32 rounds)
