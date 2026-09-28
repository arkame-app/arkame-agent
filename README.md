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
│   ├── cli/                    # comandos: install, run, status, check-storage, set-storage-keys, service, version
│   ├── config/                 # env-file + flags + defaults
│   ├── crypto/                 # Ed25519 keypair + fingerprint
│   ├── enrollment/             # fluxo de registro (first-time + reinstall)
│   ├── api/                    # HTTP client + types do painel
│   ├── storage/                # S3 client + probe (GetBucketVersioning etc) + check
│   ├── setup/                  # chave do bucket na instalação: painel, pergunta, teste, arquivo
│   ├── terminal/               # perguntas no /dev/tty (CONIN$ no Windows), senha sem eco
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

### Primeira instalação — um comando

O painel, em **Servidores → Novo servidor**, gera o comando com o código de
instalação (`atk_…`, vale 24 horas e uma vez). Ele baixa o agente, confere o
checksum, **pergunta a chave de acesso e a senha do bucket, testa no bucket** e
só então registra o servidor e instala o serviço. Chave recusada: diz a causa e
pergunta de novo. A chave fica no servidor; o painel só informa qual bucket
(`POST /api/agents/install-config`).

```bash
# Linux e macOS
curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=atk_...
```

```text
# Windows: Windows + R, colar, Enter. O curl.exe do Windows baixa o agente
# (get.arkame.app/agente.exe → release mais recente) e o `setup` dele pede
# administrador (o "Sim" do Windows), se copia para Program Files, entra no
# PATH e roda o `install`. Sem PowerShell: o Defender barrava o formato
# `powershell -ExecutionPolicy Bypass … irm` como Trojan:Win32/Commando.A!ml.
cmd /c "curl -fsSLo "%TEMP%\arkame-agent.exe" https://get.arkame.app/agente.exe && "%TEMP%\arkame-agent.exe" setup --token=atk_..."
```

O arquivo gravado (`/etc/arkame/agent.env`; no Windows `C:\etc\arkame\agent.env`)
fica legível só pelo administrador (0600; no Windows, Administradores e SYSTEM).

Sem terminal (automação), grave o arquivo antes: o `install` testa a chave que
estiver lá e para com a causa se o bucket recusar. `--check-storage=false` pula
o teste.

### Trocar a chave do bucket

```bash
sudo /usr/local/bin/arkame-agent set-storage-keys --restart   # pergunta, testa, grava e reinicia
arkame-agent check-storage                      # só testa a chave do arquivo
```

No Windows, o painel mostra a linha para o Windows + R
(`Start-Process -Verb RunAs … 'set-storage-keys --restart --pause'`).

### Remover o agente de um servidor

```bash
sudo /usr/local/bin/arkame-agent uninstall   # pergunta "sim"; --yes para automação
```

Tira o serviço, o arquivo de configuração com a chave, a identidade (token,
chave privada, agent.id) e o programa; a pasta só sai se ficar vazia. Os backups
continuam no bucket. No painel, arquive o servidor para ele deixar de ser cobrado.
Antes de tocar em qualquer coisa, confere que achou a configuração e que pode
apagá-la. Com mais de um agente na máquina (`--service-name`), use o mesmo
`--service-name` e `--config` da instalação; o programa só sai com o último.

No Windows, o agente aparece em **Aplicativos instalados** ("Arkame — agente de
backup"); o Desinstalar chama `uninstall --pause` e pede administrador sozinho.
Pelo Executar: `powershell -Command "Start-Process -Verb RunAs 'C:\Program Files\Arkame\arkame-agent.exe' 'uninstall --pause'"`.

### Re-enrollment (trocar servidor mantendo histórico)

Mesmo comando na nova máquina, com um **código novo** gerado no painel em
"Reinstalar" (`/agents/:id`). O painel identifica que o código está amarrado a um
`agent_id` existente e preserva o histórico ao aprovar a nova fingerprint. O
instalador pergunta a chave do bucket que o servidor já usava.

### Rodar daemon

```bash
# Se instalou como serviço (padrão), já está rodando:
systemctl status arkame-agent

# Manualmente:
arkame-agent run --config /etc/arkame/agent.env
```

### Docker — também um comando

O registro roda num container com terminal (`-it`): pergunta e testa a chave,
registra e espera a aprovação, gravando tudo em `/etc/arkame`. Só se ele terminar
bem o container antigo sai e o serviço sobe (`--restart always`), lendo o mesmo
lugar. `label=disable`: com SELinux (RHEL, Rocky, Fedora) o container não
gravava em `/etc/arkame` nem lia o host; sem SELinux, não faz nada.

```bash
sudo docker run --rm -it --user 0 --security-opt label=disable --hostname "$(hostname)" -v /etc/arkame:/etc/arkame \
  ghcr.io/arkame-app/arkame-agent:latest install --token=atk_... --panel-url=https://save.arkame.app --install-service=false \
&& { sudo docker rm -f arkame-agent >/dev/null 2>&1; \
  sudo docker run -d --name arkame-agent --restart always --user 0 --security-opt label=disable --hostname "$(hostname)" \
  -v /:/host:ro -v /etc/arkame:/etc/arkame ghcr.io/arkame-app/arkame-agent:latest; }
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
