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

> Atualizado em 2026-10-03 conferindo o código. A versão publicada é a que diz
> `git tag --sort=-v:refname` (ou o GitHub Releases), não este texto. As seções datadas
> acima são registro histórico e podem descrever comportamento que já mudou.
>
> **Mudanças recentes** (a versão de cada uma está nas tags; `git tag --contains <commit>`):
> - **v0.4.11:** o relato da sondagem leva `noncurrent_expiration_days` e
>   `noncurrent_transitions` (prometidos desde a 0.4.10 e que não saíam do agente) e os
>   campos novos `lifecycle_error` / `object_lock_error` (leitura negada ≠ bucket sem
>   regra); a unit do systemd põe aspas no `--config` e escapa `%`; o serviço do sistema
>   (root; SYSTEM no Windows) recusa programa que outro usuário pode trocar (Linux, macOS
>   e, pela ACL, Windows, onde o administrador que roda o install vale como dono), e o
>   `install.sh` com o programa no home sugere o install sem sudo.
> - **v0.4.12:** arquivo e pasta novos da restauração ficam do dono da pasta-mãe (no
>   Windows, a DACL de segredo só na pasta de restauração nova, não no lugar de origem);
>   `install.sh` com root usa `/opt/arkame/bin` quando o `/usr/local/bin` não é só do root
>   (Homebrew em Mac Intel); o `setup` do Windows passa `C:\Program Files\Arkame` aos
>   Administradores (outro administrador reinstala); e destino só de leitura (EROFS,
>   `/etc` no serviço nativo) vai ao painel como `read_only_destination`.
> - **v0.4.13:** o heartbeat leva `program_path`, o caminho real do programa
>   (`os.Executable` com links resolvidos), para o painel montar o comando de trocar a
>   chave com `/opt/arkame/bin` quando for o caso; o README cita esse caminho em trocar a
>   chave e remover.
>
> **Também recentes:** compatibilidade com systemd anterior ao 231 (CentOS/RHEL 7 tem
> o 219, Ubuntu 16.04 o 229). A unit de sistema escreve também `ReadWriteDirectories=`
> ao lado de `ReadWritePaths=`: o systemd antigo ignorava `ReadWritePaths=` mas aplicava
> o `ProtectSystem=full`, e `/etc/arkame` ficava só leitura — o token renovado não era
> gravado e, num reboot depois do vencimento do antigo, o agente tomava 401 para sempre.
> Vale para quem instalar ou reinstalar o serviço (a unit é reescrita no `install`). O
> `install.sh` lê o `ExecStart` com `systemctl show -p ExecStart` e tira o prefixo com
> `sed`, sem `--value` (só existe a partir do 230): nesses hosts a atualização não achava
> nenhum agente, ninguém reiniciava e o script pedia registro a servidor já registrado.
>
> O `install.sh` reconhece os agentes pelo programa, não pelo nome
> do serviço, como o lado Go (`registrados()` do systemd e do launchd). Ele só olhava
> as units `arkame-agent*` e os plists `app.arkame.agent*`; um serviço de nome legado
> (até a v0.4.3 o install aceitava qualquer nome, como `backup-oci`) ficava no binário
> antigo sem aviso depois da troca e, se fosse o único agente da máquina, o script
> mandava "registre este servidor" a um servidor já registrado. Agora lista todas as
> units de serviço ativas (sem padrão de nome) e todos os `app.arkame.*.plist`, e filtra
> pelo `ExecStart`/`ProgramArguments`.
>
> Backup e limpeza de retenção não rodam
> juntos no mesmo bucket (`internal/daemon/exclusao.go`). Antes, a limpeza podia apagar
> no meio de um backup a versão que ele reaproveitava por dedup, e a sessão gravava no
> catálogo uma versão que já não existia. O backup espera a limpeza em curso; a limpeza
> com backup em curso nem pergunta ao painel e tenta de novo em 1 minuto.
>
> O `/sessions/start` leva `exclude_globs` (sempre lista, vazia se não houver): as
> exclusões que o walker usa nesta sessão, lidas quando o plano foi buscado. O painel
> gravava as do plano em vigor no `/start`; com um `*.log` tirado do plano enquanto
> outro rodava, a sessão dizia "sem exclusões" sem ter nenhum `.log`, e a falta virava
> remoção. O painel precisa gravar o campo do agente (e cair no do plano quando ele não
> vier, de agente antigo).
>
> Link para arquivo cujo destino não dá para ler (EACCES; no macOS, sem Acesso Total ao
> Disco) entra na conta do backup parcial, como um arquivo comum ilegível. Antes era
> pulado calado: sumia de uma sessão "concluída" e o painel lia a falta como remoção.
> Link quebrado e link para pasta continuam fora, sem deixar a sessão parcial. Conta
> como link quebrado também o link em laço (`x -> x`, `a -> b -> a`: ELOOP), o que
> atravessa um arquivo (`l -> a.txt/x`: ENOTDIR) e o de nome longo demais
> (ENAMETOOLONG); no Windows, ERROR_CANT_RESOLVE_FILENAME e ERROR_FILENAME_EXCED_RANGE.
> Só os demais erros (EACCES, EPERM, EIO, ESTALE…) deixam a sessão parcial: antes, um
> link em laço esquecido deixava todo backup daquela origem parcial para sempre.
>
> O `/sessions/start` leva também `prefix_root` (string, sempre presente, `""` com as
> chaves na raiz do bucket): o prefixo de chave do armazenamento com que esta sessão
> monta as chaves (`<prefix_root>data/<agente>/…`). O painel lia a seleção da sessão sob
> o prefixo em vigor; com o prefixo trocado e devolvido (`A/` → `B/` → `A/`), as chaves
> sob `A/` de um servidor que ainda não rodou de novo pareciam removidas do servidor e
> saíam na limpeza. O painel precisa ler a seleção sob o prefixo gravado, e tratar a
> sessão sem o campo (agente antigo) como sem seleção conhecida.
>
> No Windows, o `setup` não trava mais quando o `.old` da troca anterior ainda está em
> uso (um segundo agente no mesmo exe, ou a janela fechada no meio): o programa atual sai
> como `.old-<aleatório>`, como no `install.ps1`, e na entrada saem todos os `.old*`
> livres. E o `install.ps1` não para mais o serviço para trocar o exe (a troca é por
> rename): antes, ao atualizar só o binário, o serviço ficava parado até o próximo boot.
> Agora, nesse modo (sem `-Token`), todos os serviços que estavam rodando esse exe (o
> `-ServiceName` e um segundo agente, como `arkame-agent-oci`, achados pelo `PathName`
> do Win32_Service) são reiniciados já com a versão nova; os que não reiniciarem saem
> num aviso como ainda na versão antiga. Antes só o `-ServiceName` reiniciava, e o
> outro seguia no `.old` sem aviso até o próximo boot. O mesmo vale para a instalação
> com código: o `setup` reinicia, logo depois de trocar o exe, os outros serviços que o
> rodavam (menos o `arkame-agent`, que o `install` re-registra), e o `install.ps1
> -Token` faz isso depois do `install` (menos o `-ServiceName`, salvo com `-NoService`);
> os que falham saem num aviso. Se o `install` não termina (chave errada ou cancelada,
> código vencido), ele não re-registra o serviço principal, que seguia no `.old` sem
> aviso: o `install.ps1 -Token` passa a reiniciar também o `-ServiceName`, e o `setup`
> avisa o `install` (flag oculta `--restart-on-failure=arkame-agent`, só quando o
> serviço rodava o programa trocado), que o reinicia ao sair com erro. O que não
> reinicia sai no aviso "Continuam na versão antiga".
>
> Plano de outro armazenamento (`storage.id` do `/plans` diferente do `STORAGE_ID` do
> processo, e bucket fora de `SIBLING_BUCKETS`) não roda mais: o agente abre a sessão e
> a falha logo em seguida (`/sessions/{sid}/fail`, `error_code` `wrong_storage`, com os
> dois armazenamentos na mensagem), sem comando de antes nem envio. Antes, o backup
> subia com a chave, a região e o endpoint do armazenamento instalado para um bucket
> cuja restauração o próprio agente recusa (`wrong_bucket`) e cuja retenção ninguém
> consulta. Sem `STORAGE_ID` (instalação antiga) ou sem id no plano, roda como antes.
>
> O `install.sh` não apaga mais o programa antes de copiar o novo: copia para
> `.arkame-agent.novo.<pid>` na mesma pasta e troca por `mv -f`. Se a cópia falha (disco
> cheio), sai só o temporário e o programa antigo continua; antes, o antigo sumia, o novo
> ficava pela metade e o serviço não subia no próximo reinício.
>
> No Linux e no macOS, trocar o programa agora reinicia os outros agentes que o rodam,
> como no Windows. O `mv -f` troca o inode e cada processo seguia com o binário antigo
> até alguém reiniciá-lo ou o host reiniciar; com `--token`, só o `--service-name`
> reiniciava, e sem `--token`, nenhum. Antes do `mv`, o `install.sh` anota as units de
> serviço ativas, de qualquer nome (systemd do sistema e `--user`), cujo `ExecStart`
> aponta para o programa instalado (`systemctl show -p ExecStart`), ou os jobs
> `app.arkame.*` rodando (`launchctl print` → `state = running`) cujo `ProgramArguments` aponta para
> ele. Depois da troca reinicia todos: com `--token`, menos o `--service-name`, que o
> `install` reinicia (salvo com `--no-service`, e salvo se o `install` sair com erro:
> aí o script o reinicia); sem `--token`, todos, inclusive o principal, e não mostra
> mais "registre este servidor" quando é atualização de servidor já instalado. Os que
> falham saem no aviso "Continuam na versão antiga". No pacote `service`,
> `RodandoOPrograma` e `Reiniciar` passam a funcionar no systemd e no launchd
> (`systemctl [--user] is-active`/`restart`; `launchctl print`/`kickstart -k`, no escopo
> em que o serviço está registrado), e o `setup` no Linux/macOS reinicia os outros do
> programa pelo mesmo caminho do Windows.

- **Enrollment Ed25519**: `internal/enrollment` gera keypair, POST `/api/agents/enroll`, long-poll na `wait-token` até receber JWT bearer
- **Bearer auth**: client HTTP envia `Authorization: Bearer <token>` em todos os requests pós-approval; `ErrNotReady` (204) e `ErrGone` (410) pra long-poll handling
- **Persistência local**: escopo system em `/etc/arkame/` — `token.jwt` (0600) + `key.pem` (0600) + `agent.id`, ao lado de `agent.env`; escopo user (instalação sem root e sem `--config`, fora do Windows) em `~/.config/arkame/` (ou `$XDG_CONFIG_HOME/arkame/`), com os mesmos arquivos
- **Daemon completo** (`internal/daemon/daemon.go`), 6 loops paralelos:
  - Loop heartbeat a cada `HEARTBEAT_INTERVAL_SEC` (padrão 60s) `/api/agents/{id}/heartbeat` (com `service_name`, `service_scope` e `program_path`), gravando o token renovado que vier na resposta
  - Loop probe 1h + on-demand (conferido a cada 30s em `/probe-request`): `storage.Probe` → POST `/probe` (versioning, object_lock, lifecycle, noncurrent_*, uso e, se a leitura falhou por outro motivo que "não há", `lifecycle_error`/`object_lock_error`)
  - Loop plans a cada `POLL_INTERVAL_SEC` (padrão 60s): GET `/plans` → `scheduler.ShouldRun` → `executePlan` (backup)
  - Loop restore a cada `POLL_INTERVAL_SEC`: GET `/restore-items` → PATCH running → `restore.Run` → PATCH complete/failed
  - Loop do explorador de pastas (long-poll): lista pastas para o painel (`internal/fsbrowse`)
  - Loop de expurgo 1h: aplica a retenção autorizada pelo painel (`internal/purge`); nunca junto com um backup (`exclusaoDoBucket`: o backup espera, a limpeza é adiada e reprovada a cada minuto)
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
