# Desenvolvimento do arkame-agent (pt-BR)

> Notas internas que saíram do README. Para usar o agente, veja o
> [README](../README.md) (inglês) ou o [guia de uso em português](USAGE.pt-BR.md).
> O histórico detalhado de cada passada está em [STATUS.md](../STATUS.md).

## Status

**Status:** funcional. Enrollment (Ed25519 + bearer JWT), daemon com 6 loops (heartbeat com renovação do token, probe, planos/backup, restauração, explorador de pastas, expurgo de retenção), sync engine (walker + hash + dedup HeadObject + multipart upload), restore com escrita atômica + verify, e warming de cold storage estão implementados. Build limpo (`go vet ./...`). Resta hardening (mTLS, self-update, snapshots, observabilidade) — ver "O que falta".

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
│  │  • S3 uploader         │──┼──────────┤  cliente (S3/B2/...) │
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
│   ├── cli/                    # comandos: install, setup, run, status, heartbeat, check-storage, set-storage-keys, service, uninstall, version
│   ├── config/                 # env-file + flags + defaults
│   ├── crypto/                 # Ed25519 keypair + fingerprint
│   ├── enrollment/             # fluxo de registro (first-time + reinstall)
│   ├── api/                    # HTTP client + types do painel
│   ├── storage/                # S3 client + probe (GetBucketVersioning etc) + check
│   ├── setup/                  # chave do bucket na instalação: painel, pergunta, teste, arquivo
│   ├── segredo/                # gravação protegida de chave, token e identidade (0600; DACL no Windows)
│   ├── terminal/               # perguntas no /dev/tty (CONIN$ no Windows), senha sem eco
│   ├── caminho/                # caminhos entre painel, disco e bucket (unidade do Windows, HostRoot)
│   ├── sync/                   # walker + hasher + engine de upload + throttle
│   ├── hooks/                  # comandos de antes/depois do backup (ex.: pg_dump)
│   ├── restore/                # executor de restauração (escrita atômica, sha256, cold storage)
│   ├── purge/                  # expurgo de versões autorizado pelo painel (retenção)
│   ├── fsbrowse/               # listagem de pastas para o explorador do painel
│   ├── scheduler/              # janelas de tempo + decisão de "should run agora"
│   ├── daemon/                 # loops do serviço (heartbeat, probe, planos, restauração, pastas, expurgo)
│   ├── service/                # install como systemd/launchd/Windows Service
│   ├── aplicativos/            # entrada em "Aplicativos instalados" do Windows e remoção do programa
│   └── logarquivo/             # log em arquivo com rodízio (serviço do Windows)
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
- [x] Testes de integração contra S3 de verdade: a CI e a release rodam `go test -race ./...` com um RustFS 1.0.0 local (o MinIO deixou de ser baixável em 24/09)
- [ ] Observabilidade: métricas Prometheus + traces OTEL (endpoint opcional)

## Antes de abrir PR

```bash
make lint       # vet + gofmt
make test       # race + coverage
go mod tidy
```

Os testes que tocam bucket (purga, por exemplo) usam um S3 local quando
`ARKAME_TEST_S3_ENDPOINT` está definido; a CI sobe um RustFS 1.0.0:

```bash
docker run -d --name s3 -p 9000:9000 \
  -e RUSTFS_ACCESS_KEY=arkametest -e RUSTFS_SECRET_KEY=arkametest123 \
  docker.io/rustfs/rustfs:1.0.0
ARKAME_TEST_S3_ENDPOINT=http://127.0.0.1:9000 go test -race ./...
```

## Contrato com o painel

O schema do que este agent reporta ao painel está no repositório do painel (`arkame`), nas migrações em `db/migrations/` e no acesso a dados em `packages/db/` — mudanças nos types `api/` precisam bater com as rotas do Next.js em `apps/save/src/app/api/`.
