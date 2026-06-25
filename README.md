# Arkame Agent

Agent Go do SaaS [Arkame](https://arkame.app) — roda no servidor do cliente, lê arquivos e envia para o bucket BYOS, reportando status ao painel.

**Status:** funcional. Enrollment (Ed25519 + bearer JWT), daemon com 4 loops (heartbeat, probe, plans/backup, restore), sync engine (walker + hash + dedup HeadObject + multipart upload), restore com escrita atômica + verify, e warming de cold storage estão implementados. Build limpo (`go vet ./...`). Resta hardening (mTLS, self-update, snapshots, observabilidade) — ver "O que falta".

## Arquitetura

```
┌─────────────────────────────┐          ┌──────────────────────┐
│  Máquina do cliente          │          │  Painel Arkame        │
│  ┌────────────────────────┐  │   mTLS   │  save.arkame.app     │
│  │ arkame-agent (daemon)  │◄─┼──────────┤  /api/agents/...     │
│  │                        │  │          └──────────────────────┘
│  │  • enrollment (Ed25519)│  │
│  │  • scheduler (windows) │  │          ┌──────────────────────┐
│  │  • walker + hasher     │  │          │  Bucket BYOS do      │
│  │  • S3 uploader         │──┼──────────┤  cliente (S3/R2/...) │
│  │  • probe periódico     │  │  direto  └──────────────────────┘
│  └────────────────────────┘  │
└─────────────────────────────┘
```

**Princípio:** dados NUNCA passam pelo painel Arkame. O agent fala direto com o bucket do cliente (credenciais via env-file local) e reporta apenas metadata (filenames, paths, sizes, hashes, version_ids) ao painel para indexação.

> **Auth atual:** o agent autentica no painel via **bearer JWT** (obtido no enrollment Ed25519), não mTLS. O diagrama acima reflete o alvo de longo prazo — mTLS é hardening de fase 2 e não quebra o contrato atual.

## Estrutura do código

```
arkame-agent/
├── cmd/arkame-agent/main.go    # entry point (delega pro cobra root)
├── internal/
│   ├── cli/                    # comandos: install, run, status, version
│   ├── config/                 # env-file + flags + defaults
│   ├── crypto/                 # Ed25519 keypair + fingerprint
│   ├── enrollment/             # fluxo de registro (first-time + reinstall)
│   ├── api/                    # HTTP client + types do painel
│   ├── storage/                # S3 client + probe (GetBucketVersioning etc)
│   ├── sync/                   # walker + hasher + engine de upload + throttle
│   ├── scheduler/              # janelas de tempo + decisão de "should run agora"
│   ├── daemon/                 # loop principal (heartbeat, poll, execute)
│   └── service/                # install como systemd/launchd/Windows Service
└── pkg/version/                # build info (injetada via -ldflags)
```

## Build

```bash
# Para a plataforma atual
make build

# Cross-compile para todas as plataformas suportadas
make build-all

# Docker
make docker
```

Binários vão pra `bin/`. Makefile embute `version.Version` / `version.Commit` / `version.BuildDate` via `-ldflags`.

## Uso

### Primeira instalação

```bash
# 1. Criar arquivo de credenciais do bucket
sudo mkdir -p /etc/arkame
sudo tee /etc/arkame/agent.env > /dev/null <<EOF
STORAGE_ACCESS_KEY=SUA_ACCESS_KEY
STORAGE_SECRET_KEY=SEU_SECRET
EOF
sudo chmod 600 /etc/arkame/agent.env

# 2. Enrollar no painel
sudo arkame-agent install \
  --config /etc/arkame/agent.env \
  --enrollment-token enr_01HXQZ3JK8ABC123DEF456 \
  --panel-url https://save.arkame.app

# O output imprime o fingerprint. Aprove no painel em:
#   https://save.arkame.app/agents/<agent_id>

# 3. Verificar status
arkame-agent status
```

### Re-enrollment (trocar servidor mantendo histórico)

Mesmo comando `install` na nova máquina, com um **token fresh** gerado no painel clicando "Reinstalar" em `/agents/:id`. O painel identifica que o token está amarrado a um `agent_id` existente e preserva histórico ao aprovar a nova fingerprint.

### Rodar daemon

```bash
# Se instalou como systemd service (default), já está rodando:
systemctl status arkame-agent

# Manualmente:
arkame-agent run --config /etc/arkame/agent.env
```

### Docker

```bash
docker run -d \
  --name arkame-agent \
  --restart always \
  -v /:/host:ro \
  --env-file /etc/arkame/agent.env \
  -e ENROLLMENT_TOKEN=enr_01HXQZ3JK8... \
  -e PANEL_URL=https://save.arkame.app \
  arkame/agent:latest install --install-service=false
```

## Variáveis de ambiente

Lidas do env-file ou das env vars do processo (CLI tem precedência).

| Variável | Uso |
|---|---|
| `STORAGE_ACCESS_KEY` | **Credencial S3 do cliente** (NUNCA sai desta máquina) |
| `STORAGE_SECRET_KEY` | **Credencial S3 do cliente** |
| `STORAGE_ENDPOINT` | Só para S3-compat não-AWS (MinIO, Wasabi, etc) |
| `STORAGE_REGION` | `us-east-1` default |
| `STORAGE_BUCKET` | Nome do bucket (informativo; path vem do painel) |
| `STORAGE_ID` | ULID do storage no painel |
| `PANEL_URL` | `https://save.arkame.app` |
| `ENROLLMENT_TOKEN` | Temporário, só durante install |
| `AGENT_ID` | Persistido após primeiro enrollment |
| `CERT_PATH` | `/etc/arkame/cert.pem` |
| `PRIVATE_KEY_PATH` | `/etc/arkame/key.pem` (0600) |
| `CA_PATH` | `/etc/arkame/ca.pem` |
| `HOST_ROOT` | `/` nativo, `/host` em Docker |

## O que falta (TODOs)

Núcleo funcional entregue. Itens concluídos e pendências de hardening:

- [x] `enrollment.WaitForApproval` — long-poll real (bearer JWT via `wait-token`)
- [x] `daemon.executePlan` — SessionStart → sync → SessionComplete (com `version_map`)
- [x] `sync.engine` — dedup via HeadObject + multipart upload para arquivos grandes
- [x] `daemon.probeLoop` — cabeado com `storage.Probe` (versioning/object-lock/lifecycle)
- [x] `service.launchd` / `service.systemd` / `service.windows`
- [x] Restore (Plan kind=restore — executor com escrita atômica + SHA-256 verify + warming)
- [ ] Self-update (agente baixa nova versão quando painel sinaliza)
- [ ] mTLS hardening (fase 2) — substituir bearer JWT mantendo o contrato atual
- [ ] Snapshot orquestrado (LVM / VSS / btrfs) — fora do escopo atual (PLAN.md), pode voltar como plugin
- [ ] Testes: integration com MinIO local (scheduler já tem unit)
- [ ] Observabilidade: métricas Prometheus + traces OTEL (endpoint opcional)
- [ ] `.goreleaser.yaml` para GitHub Releases automatizado

## Contribuindo

Antes de abrir PR:

```bash
make lint       # vet + gofmt
make test       # race + coverage
go mod tidy
```

Schema que este agent reporta ao painel está em `arkame/db/schema.sql` no repositório principal — mudanças nos types `api/` precisam bater com as rotas do Next.js em `apps/save/src/app/api/`.
