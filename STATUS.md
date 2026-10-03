# Arkame Agent — Status

**Status:** Sprints 5 (enrollment + bearer auth), 6 (sync engine + probe reporter), Tier 1 C (restore execution) e Tier 1 follow-ups (warming + dedup + multipart download) **concluídos**. Build limpo (`go vet ./...`). **Validado E2E contra storage real (2026-06-24)** — ver abaixo.

> Trabalho ativo principal está no painel (`~/hugo-projects/arkame/`). Para o status global do produto, ver `~/hugo-projects/arkame/STATUS.md`.

## 🧭 Instalação em um comando, com a chave testada (2026-09-27, v0.3.0)

Um Windows real instalou com a chave do bucket errada e nada avisou; e a instalação pedia dois
comandos num PowerShell como administrador.
- `install` garante a credencial antes de registrar (`cli/credencial.go`): sem chave no arquivo,
  pergunta ao painel qual bucket (`POST /api/agents/install-config`, pelo código), pede a chave
  no terminal (`internal/terminal`: `/dev/tty` ou `CONIN$`, senha sem eco via `x/sys`), testa
  (`storage.Check` = `GetBucketVersioning`) e grava o arquivo 0600 (Windows: `icacls` por SID).
  Com chave no arquivo, testa; recusada e sem terminal, para com a causa (`storage.Causa`).
  `--check-storage=false` pula.
- Comandos novos: `check-storage`, `set-storage-keys [--restart] [--pause]`.
- `install.ps1` se reabre elevado (UAC) com `-EncodedCommand` e valida código/painel/versão;
  a linha do painel não tem `$` e cabe no Executar. `install.sh` valida o código.
- `service.Detect()` no Windows reportava `ArkameAgent`, nome que não existe; agora `arkame-agent`.
- Provas: `go test ./internal/setup ./internal/storage`; laço e2e do painel (`scripts/laco.sh`)
  roda o agente real contra RustFS, com terminal simulado.

## 🔧 Probe estendido + on-demand + reconcile (2026-06-25)

Acompanha as 13 melhorias de UX do painel. **Compila + `go vet` limpos** (Docker `golang:1.24-alpine`); validação E2E contra storage real pendente (próxima janela com o agente ativo).
- **Tamanho usado** (`storage/probe.go`): `measureUsage` pagina `ListObjectsV2` somando `Size` → `used_bytes`/`object_count` no `ProbeReport` (`api/types.go`). Não-fatal se falhar.
- **Probe on-demand** (`daemon/daemon.go`): `probeLoop` ganhou um segundo ticker (30s) que faz `GET /api/agents/{id}/probe-request`; se o `STORAGE_ID` do agente está na lista de probes solicitados pela UI ("Testar conexão"), roda o probe na hora. O POST `/probe` agora inclui `used_bytes`/`object_count`.
- **Reconcile reativo** (`daemon/daemon.go`): no restore, `isObjectGone(err)` detecta `NoSuchKey`/`NoSuchVersion`/404 e seta `error_code=not_found` no PATCH do restore-item — o painel marca a versão `unavailable_since` no índice (`session_files`).

## ✅ Validação end-to-end real (2026-06-24)

Binário Linux rodando local contra o painel em produção (`save.arkame.app`) + buckets reais: enroll → aprovação → backup → bucket → restore, em **OCI** (S3-compat) e **AWS S3**. 4 arquivos (incl. 120 MB p/ multipart), todos os SHA256 conferidos no bucket e no restore. **7 bugs corrigidos** (só apareciam contra storage de verdade):
- `cli/install.go` — enroll não enviava `install_method` → painel `400 missing required fields`.
- `config/config.go` + `enrollment.go` — `agent.id`/`token.jwt` gravavam em `/etc/arkame` hardcoded; agora honram `AGENT_ID_PATH`/`TOKEN_PATH` (config não zera mais `TokenPath`; novo método `TokenExists()`). Destrava modo rootless/Docker.
- `storage/s3.go` — checksums de integridade do SDK (CRC32 aws-chunked) quebram S3-compat; `RequestChecksumCalculation=WhenRequired` (config + Options) quando há endpoint custom.
- `sync/engine.go` + `sync/throttle.go` — PutObject sem `Content-Length` → 411 (fix: ContentLength + `ThrottledReader` agora é `io.ReadSeeker`); multipart via `s3/manager` emitia aws-chunked → 501 na OCI (fix: **multipart manual** com parte seekable + ContentLength, sem ChecksumAlgorithm).
- `daemon/daemon.go` + `sync/engine.go` — **bug grave de correção**: backup onde TODOS os arquivos falhavam era reportado como `complete`/0 arquivos; agora rastreia `FilesFailed` → `failed`/`partial`.

## O que está pronto

> Atualizado em 2026-10-03 conferindo o código (v0.4.12, publicada em 03/10; tag no
> `5e4b04b`). As seções datadas acima são registro histórico e podem descrever
> comportamento que já mudou.
>
> **Na v0.4.11:** o relato da sondagem leva `noncurrent_expiration_days` e
> `noncurrent_transitions` (prometidos desde a 0.4.10 e que não saíam do agente) e os
> campos novos `lifecycle_error` / `object_lock_error` (leitura negada ≠ bucket sem
> regra); a unit do systemd põe aspas no `--config` e escapa `%`; o serviço do sistema
> (root; SYSTEM no Windows) recusa programa que outro usuário pode trocar (Linux, macOS
> e, pela ACL, Windows, onde o administrador que roda o install vale como dono), e o
> `install.sh` com o programa no home sugere o install sem sudo.
>
> **Na v0.4.12:** arquivo e pasta novos da restauração ficam do dono da
> pasta-mãe (no Windows, a DACL de segredo só na pasta de restauração nova, não no lugar
> de origem); `install.sh` com root usa `/opt/arkame/bin` quando o `/usr/local/bin` não é
> só do root (Homebrew em Mac Intel); o `setup` do Windows passa
> `C:\Program Files\Arkame` aos Administradores (outro administrador reinstala); e
> destino só de leitura (EROFS, `/etc` no serviço nativo) vai ao painel como
> `read_only_destination`.
>
> **Na 0.4.13 (sem tag):** o heartbeat leva `program_path`, o caminho real do programa
> (`os.Executable` com links resolvidos), para o painel montar o comando de trocar a chave
> com `/opt/arkame/bin` quando for o caso; o README cita esse caminho em trocar a chave e
> remover.

- **Enrollment Ed25519**: `internal/enrollment` gera keypair, POST `/api/agents/enroll`, long-poll na `wait-token` até receber JWT bearer
- **Bearer auth**: client HTTP envia `Authorization: Bearer <token>` em todos os requests pós-approval; `ErrNotReady` (204) e `ErrGone` (410) pra long-poll handling
- **Persistência local**: escopo system em `/etc/arkame/` — `token.jwt` (0600) + `key.pem` (0600) + `agent.id`, ao lado de `agent.env`; escopo user (instalação sem root e sem `--config`, fora do Windows) em `~/.config/arkame/` (ou `$XDG_CONFIG_HOME/arkame/`), com os mesmos arquivos
- **Daemon completo** (`internal/daemon/daemon.go`), 6 loops paralelos:
  - Loop heartbeat a cada `HEARTBEAT_INTERVAL_SEC` (padrão 60s) `/api/agents/{id}/heartbeat` (com `service_name`, `service_scope` e `program_path`), gravando o token renovado que vier na resposta
  - Loop probe 1h + on-demand (conferido a cada 30s em `/probe-request`): `storage.Probe` → POST `/probe` (versioning, object_lock, lifecycle, noncurrent_*, uso e, se a leitura falhou por outro motivo que "não há", `lifecycle_error`/`object_lock_error`)
  - Loop plans a cada `POLL_INTERVAL_SEC` (padrão 60s): GET `/plans` → `scheduler.ShouldRun` → `executePlan` (backup)
  - Loop restore a cada `POLL_INTERVAL_SEC`: GET `/restore-items` → PATCH running → `restore.Run` → PATCH complete/failed
  - Loop do explorador de pastas (long-poll): lista pastas para o painel (`internal/fsbrowse`)
  - Loop de expurgo 1h: aplica a retenção autorizada pelo painel (`internal/purge`)
- **`executePlan` (backup)**: POST `/sessions/start` → `sync.Run` (walker + hash + dedup HeadObject + PutObject/multipart + version_map) → POST `/sessions/{sid}/complete` com version_map inline; em falha total POST `/sessions/{sid}/fail`
- **Dedup file-level**: antes de PutObject, faz HeadObject e compara `sha256` no metadata. Hit retorna FileEntry com VersionId existente sem subir bytes; stats `FilesUploaded` não conta dedup hits
- **`restore.Run` (restore)**: `internal/restore/executor.go` — escrita atômica (tmp + rename) com SHA-256 verify → conflict resolution `suffix-version`/`overwrite`/`skip`; respeita `HOST_ROOT`
- **Multipart download**: arquivos >= 100 MB usam `s3manager.Downloader` (4 workers, parts 16 MiB); hash é calculado relendo o tmp file
- **Warming cold storage**: GetObject que falha com `InvalidObjectState` → HeadObject pra checar `x-amz-restore` → RestoreObject (Standard tier, 7d) se necessário; daemon mantém item como `running` pra re-tentar no próximo poll (ErrWarmingRequested / ErrWarmingInProgress), e o PATCH do item leva `warming_state` (`requested`/`in_progress`), `warming_tier` e `warming_eta`
- **Comandos CLI**: `install` (sempre espera a aprovação; `--wait=false` é recusado), `setup` (Windows: copia para Program Files e roda o `install`), `run`, `status`, `heartbeat` (one-shot pra testar auth), `check-storage`, `set-storage-keys`, `service install|uninstall|status`, `uninstall`, `version`

## Versionado no GitHub

Repo público (Apache 2.0) em [`arkame-app/arkame-agent`](https://github.com/arkame-app/arkame-agent). Branch principal `main`. Auth SSH (`hugolf`).

## O que falta (próximos passos)

- **Self-update** do binário em produção (fase posterior)
- **mTLS hardening** (fase 2) — substituir bearer JWT por mTLS com CA do painel, sem quebrar o contrato atual
- **VSS no Windows** pra snapshots consistentes (Linux LVM também no roadmap)

## Como compilar (sem Go local)

```bash
cd ~/hugo-projects/arkame-agent
docker run --rm -v "$PWD:/src" -w /src golang:1.25-alpine sh -c "go build ./..."
```

## Veja também

- `README.md` aqui — arquitetura completa
- `~/hugo-projects/arkame/STATUS.md` — estado do painel e produto
- `~/hugo-projects/arkame/PLAN.md` — histórico de decisões (32 rounds)
