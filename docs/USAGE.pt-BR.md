# Guia de uso do agente (pt-BR)

> Referência detalhada de instalação e operação, em português. O resumo em
> inglês está no [README](../README.md); a documentação para usuários fica em
> [arkame.app/docs](https://arkame.app/docs?utm_source=github&utm_medium=readme&utm_campaign=agent-repo).
> Notas de desenvolvimento: [DEVELOPMENT.md](DEVELOPMENT.md).

## Uso

### Primeira instalação — um comando

O painel, em **Servidores → Novo servidor**, gera o comando com o código de
instalação (`atk_…`, vale 24 horas e uma vez). Ele baixa o agente, confere o
checksum, **pergunta a chave de acesso e a senha do bucket, testa no bucket** e
só então registra o servidor e instala o serviço. Chave recusada: diz a causa e
pergunta de novo. A chave fica no servidor; o painel só informa qual bucket
(`POST /api/agents/install-config`).

#### Docker (padrão em Linux)

O registro roda num container com terminal (`-it`): pergunta e testa a chave,
registra e espera a aprovação, gravando tudo em `/etc/arkame`. Só se ele terminar
bem o container antigo sai e o serviço sobe (`--restart always`), lendo o mesmo
lugar. `label=disable`: com SELinux (RHEL, Rocky, Fedora) o container não
gravava em `/etc/arkame` nem lia o host; sem SELinux, não faz nada. A raiz do
servidor vai montada com leitura e escrita (`-v /:/host`, sem `:ro`): a
restauração grava no servidor, inclusive no lugar original. O prazo de 150 s
(`--stop-timeout`, `stop -t`, `restart -t`) é o tempo que o agente pode levar para
fechar um backup em andamento e avisar o painel (`/complete`); os 10 s padrão do
Docker, ou o `rm -f`, matam o processo antes e o ponto de restauração se perde.
`--pull always` no primeiro `docker run`: o agente não se atualiza sozinho, e
reinstalar com este comando é como ele se atualiza no Docker; sem a opção, o
Docker reaproveita a `:latest` que já está no servidor e o agente continua na
versão antiga.

```bash
sudo docker run --pull always --rm -it --user 0 --security-opt label=disable --hostname "$(hostname)" -v /etc/arkame:/etc/arkame \
  ghcr.io/arkame-app/arkame-agent:latest install --token=atk_... --panel-url=https://save.arkame.app --install-service=false \
&& { sudo docker stop -t 150 arkame-agent >/dev/null 2>&1; sudo docker rm arkame-agent >/dev/null 2>&1; \
  sudo docker run -d --name arkame-agent --restart always --stop-timeout 150 --user 0 --security-opt label=disable --hostname "$(hostname)" \
  -v /:/host -v /etc/arkame:/etc/arkame ghcr.io/arkame-app/arkame-agent:latest; }
```

No Docker, os comandos antes/depois do backup de um plano não rodam (a imagem
não tem shell, e os programas estão no host): agende dumps de banco no próprio
host (cron), numa pasta incluída no plano.

Servidor sem sinal: `sudo docker logs --tail 50 arkame-agent` e
`sudo docker restart -t 150 arkame-agent`. Trocar a chave do bucket:
`sudo docker run --rm -it --user 0 --security-opt label=disable -v /etc/arkame:/etc/arkame ghcr.io/arkame-app/arkame-agent:latest set-storage-keys && sudo docker restart -t 150 arkame-agent`
(o `label=disable` pelo mesmo motivo da instalação: com SELinux, sem ele o
container não grava em `/etc/arkame`).

#### Nativo (Linux, macOS e Windows)

```bash
# Linux e macOS
curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=atk_...
```

```text
# Windows: Windows + R, colar, Enter. O curl.exe do Windows baixa o agente
# (get.arkame.app/agente.exe → release mais recente) e o `setup` dele pede
# administrador (o "Sim" do Windows), confere o SHA-256 do próprio programa
# com o checksums.txt do release da versão dele, se copia para Program Files,
# entra no PATH e roda o `install`. Sem PowerShell: o Defender barrava o formato
# `powershell -ExecutionPolicy Bypass … irm` como Trojan:Win32/Commando.A!ml.
cmd /c "curl -fsSLo "%TEMP%\arkame-agent.exe" https://get.arkame.app/agente.exe && "%TEMP%\arkame-agent.exe" setup --token=atk_... || pause"
```

Checksum diferente aborta sem instalar nada. Se o `checksums.txt` não pode ser
baixado (sem acesso a `github.com`, por exemplo), não tem a linha do pacote ou
não há como calcular o SHA-256, os instaladores também param sem instalar nada.
Para instalar assim mesmo, sem conferir, é preciso pedir: `--skip-checksum`
depois do `setup` (Windows), `--skip-checksum` no `install.sh`
(`… | sudo sh -s -- --token=atk_... --skip-checksum`) ou `-SkipChecksum` no
`install.ps1`.

O `curl.exe` vem no Windows 10 (1803+), 11 e Server 2019+. No Server 2016, use o
`install.ps1` (PowerShell como administrador).

> **Windows 11 com Controle Inteligente de Aplicativos (Smart App Control):** o
> Windows bloqueia o agente, que ainda não tem assinatura de código. Windows 10 e
> Windows Server funcionam normalmente.

> **Restaurar no lugar original em `/etc`, `/usr` ou `/boot` (Linux nativo):** o
> serviço do systemd roda com `ProtectSystem=full`, que deixa essas pastas só de
> leitura para o agente. A restauração para lá falha sempre ("read-only file
> system"), e o item aparece no painel com o código `read_only_destination` e a
> explicação. Restaure em outra pasta (`/restore`, por exemplo) e copie de lá. No
> Docker o mesmo pedido funciona: a raiz do servidor vai montada com escrita.

O arquivo gravado (`/etc/arkame/agent.env`; no Windows `C:\etc\arkame\agent.env`)
fica legível só pelo administrador (0600; no Windows, Administradores e SYSTEM).
Na instalação sem root (`--service-scope user`, o padrão de quem roda sem
sudo), sem `--config`, o arquivo é `$XDG_CONFIG_HOME/arkame/agent.env`
(`~/.config/arkame/agent.env`), com o token, a chave e o agent.id ao lado.

O serviço do sistema (instalação com sudo) roda como root, e o `install` recusa
registrá-lo se o programa, ou alguma pasta acima dele, não for do root ou puder
ser gravado por outro usuário (grupo ou outros; o grupo do root vale): quem
trocasse o arquivo viraria root no próximo reinício do serviço. É o caso de
`~/.local/bin`, onde o `install.sh` sem sudo põe o programa. Com o programa no
home, instale sem sudo (`~/.local/bin/arkame-agent install --token=atk_...`,
escopo user) ou rode o instalador com sudo, que o põe em `/usr/local/bin`. No
macOS (LaunchDaemon) vale o mesmo. Quando o `/usr/local/bin` (ou uma pasta
acima dele) não é só do root — o Homebrew, em Macs Intel, o deixa com o seu
usuário —, o instalador com sudo põe o programa em `/opt/arkame/bin`, e o
comando do painel funciona como está. Para outra pasta só do root:
`curl -fsSL https://get.arkame.app/install.sh | sudo env ARKAME_BIN_DIR=/caminho sh -s -- --token=atk_...`.
No Windows, o serviço roda como SYSTEM e o `install` confere a mesma coisa pela
lista de permissões: o programa e as pastas acima dele só podem ser alteráveis
pelos Administradores, pelo SYSTEM, pelo TrustedInstaller ou pelo administrador
que roda a instalação (dono do que ele cria, quando a política de dono padrão é
"Criador do objeto"). O `setup` (comando
do painel) e o `install.ps1` põem o programa em `C:\Program Files\Arkame`, que
passa; um `install` rodado de Downloads é recusado.

No Windows, o serviço grava o log ao lado da configuração, em
`C:\etc\arkame\agent.log` (rodízio aos 10 MB; o anterior fica em
`agent.log.1`) — o Visualizador de Eventos não tem nada do agente:

```text
powershell -Command "Get-Content -Tail 50 -Wait 'C:\etc\arkame\agent.log'"
```

> **OneDrive (Windows):** arquivos que estão só na nuvem (os marcadores
> "disponível online") ficam de fora do backup — lê-los faria o agente baixar o
> OneDrive inteiro para o disco do servidor. Para entrar no backup, o arquivo
> precisa estar disponível offline ("Sempre manter neste dispositivo"). A sessão
> informa ao painel quantos ficaram de fora, e a falta deles não conta como
> arquivo removido na origem.

> **macOS — Acesso Total ao Disco:** como serviço (LaunchDaemon ou
> LaunchAgent), o agente não lê Mesa, Documentos, Downloads nem o iCloud Drive
> até receber o Acesso Total ao Disco: o macOS devolve "operation not
> permitted" e o backup sai parcial. Depois do `install`:
>
> 1. Ajustes do Sistema → Privacidade e Segurança → Acesso Total ao Disco → **+**.
> 2. No seletor, Cmd+Shift+G e cole o caminho do programa:
>    `/usr/local/bin/arkame-agent` (instalação com sudo; `/opt/arkame/bin/arkame-agent`
>    com o `/usr/local/bin` do Homebrew) ou
>    `~/.local/bin/arkame-agent` (instalação sem root). Ative a chave dele.
> 3. Reinicie o serviço com o comando que o `install` mostrou
>    (`sudo launchctl kickstart -k system/app.arkame.agent`, ou
>    `launchctl kickstart -k gui/$(id -u)/app.arkame.agent` sem root).
>
> O `install` mostra esse passo no fim, e a sessão parcial causada por isso diz
> o mesmo no painel. Trocar o programa (atualização) pode exigir conceder de
> novo.

Sem terminal (automação), grave o arquivo antes: o `install` testa a chave que
estiver lá e para com a causa se o bucket recusar. `--check-storage=false` pula
o teste.

O `install` sempre espera a aprovação no painel (Ctrl-C cancela sem mexer no
arquivo de configuração): a identidade nova, e também o armazenamento e a
chave já testados, só são gravados com ela, e o serviço só é instalado
depois, com o token no disco. Não há `--wait=false` — o enrollment deixado sem
espera não teria quem o concluísse.

### Trocar a chave do bucket

```bash
sudo /usr/local/bin/arkame-agent set-storage-keys --restart   # pergunta, testa, grava e reinicia
sudo /usr/local/bin/arkame-agent check-storage                # só testa a chave do arquivo

# Com root, quando o instalador avisou que /usr/local/bin não é só do root
# (Homebrew em Mac Intel): o programa está em /opt/arkame/bin
sudo /opt/arkame/bin/arkame-agent set-storage-keys --restart
sudo /opt/arkame/bin/arkame-agent check-storage

# Instalação sem root (Linux ou macOS)
~/.local/bin/arkame-agent check-storage --service-scope user
```

No Windows, o painel mostra a linha para o Windows + R
(`Start-Process -Verb RunAs … 'set-storage-keys --restart --pause'`).

### Remover o agente de um servidor

```bash
sudo /usr/local/bin/arkame-agent uninstall   # pergunta "sim"; --yes para automação

# Com root, quando o instalador avisou que /usr/local/bin não é só do root
sudo /opt/arkame/bin/arkame-agent uninstall

# Instalação sem root (Linux ou macOS)
~/.local/bin/arkame-agent uninstall --service-scope user

# Docker (o programa está só na imagem; não há uninstall no host)
sudo docker stop -t 150 arkame-agent 2>/dev/null; sudo docker rm arkame-agent 2>/dev/null; sudo rm -rf /etc/arkame; sudo docker rmi ghcr.io/arkame-app/arkame-agent:latest 2>/dev/null
```

Tira o serviço, o arquivo de configuração com a chave, a identidade (token,
chave privada, agent.id) e o programa; a pasta só sai se ficar vazia. Os backups
continuam no bucket. No painel, arquive o servidor para ele deixar de ser cobrado.
Antes de tocar em qualquer coisa, confere que achou a configuração e que pode
apagá-la. Com mais de um agente na máquina (`--service-name`), use o mesmo
`--service-name` e `--config` da instalação; o programa só sai com o último, e
token, chave e agent.id que a configuração de outro agente ainda usa ficam (se
a configuração de algum não puder ser lida, a identidade fica toda).

Mais de um agente na máquina (um por credencial de bucket): instale cada um com
o seu `--service-name` e `--config` (`install.sh --config=/etc/arkame/agent-oci.env
--service-name=arkame-agent-oci`; no `install.ps1`, `-Config` e `-ServiceName`).
Com `--config` diferente do padrão, o `install` grava token, chave e agent.id
ao lado do arquivo (`agent-oci.token.jwt`, `agent-oci.key.pem`,
`agent-oci.agent.id`), a menos que `TOKEN_PATH`, `PRIVATE_KEY_PATH` ou
`AGENT_ID_PATH` já estejam definidos.

No Windows, o agente aparece em **Aplicativos instalados** ("Arkame — agente de
backup"); o Desinstalar chama `uninstall --pause` e pede administrador sozinho.
Pelo Executar: `powershell -Command "Start-Process -Verb RunAs 'C:\Program Files\Arkame\arkame-agent.exe' 'uninstall --pause'"`.

### Re-enrollment (trocar servidor mantendo histórico)

Mesmo comando na nova máquina, com um **código novo** gerado no painel em
"Reinstalar" (`/agents/:id`). O painel identifica que o código está amarrado a um
`agent_id` existente e preserva o histórico ao aprovar a nova fingerprint. O
instalador pergunta a chave do bucket que o servidor já usava.
Com a chave já no arquivo, o `install` confere no painel o armazenamento do
código: se for outro (bucket, região ou endereço), testa a chave nele e grava o
armazenamento novo inteiro; recusada, pergunta outra — sem terminal, para com a
causa e não mexe no arquivo.

### Rodar daemon

```bash
# Se instalou como serviço (padrão), já está rodando:
systemctl status arkame-agent

# Manualmente, em primeiro plano. Pare o serviço antes, para não ficarem dois
# agentes com a mesma identidade rodando ao mesmo tempo:
sudo systemctl stop arkame-agent
sudo /usr/local/bin/arkame-agent run --config /etc/arkame/agent.env

# Com root, quando o instalador avisou que /usr/local/bin não é só do root
# (Homebrew em Mac Intel): o programa está em /opt/arkame/bin
sudo /opt/arkame/bin/arkame-agent run --config /etc/arkame/agent.env

# Instalação sem root (Linux ou macOS)
systemctl --user stop arkame-agent
~/.local/bin/arkame-agent run --config ~/.config/arkame/agent.env
```

No macOS o serviço é do launchd, não do systemd: pare-o com
`sudo launchctl bootout system/app.arkame.agent` (sem root,
`launchctl bootout gui/$(id -u)/app.arkame.agent`). Para o serviço voltar:
`sudo systemctl start arkame-agent` (sem root, `systemctl --user start arkame-agent`)
no Linux; no macOS,
`sudo launchctl bootstrap system /Library/LaunchDaemons/app.arkame.agent.plist`
(sem root, `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/app.arkame.agent.plist`).

## Variáveis de ambiente

Lidas do env-file ou das env vars do processo (CLI tem precedência).

| Variável | Uso |
|---|---|
| `STORAGE_ACCESS_KEY` | **Credencial S3 do cliente** (NUNCA sai desta máquina) |
| `STORAGE_SECRET_KEY` | **Credencial S3 do cliente** |
| `STORAGE_ENDPOINT` | Só para S3-compat não-AWS (MinIO, Wasabi, etc) |
| `STORAGE_REGION` | `us-east-1` default |
| `STORAGE_BUCKET` | Nome do bucket. Obrigatório para o teste do bucket, a limpeza da retenção e o `check-storage`; também separa os planos e restaurações deste processo dos de outro (os caminhos dentro do bucket vêm do painel) |
| `STORAGE_ID` | UUID do storage no painel |
| `PANEL_URL` | `https://save.arkame.app` |
| `ENROLLMENT_TOKEN` | Temporário, só durante install; sai do arquivo com a aprovação |
| `AGENT_ID` | Identidade do agente; sem ela, vale o conteúdo de `AGENT_ID_PATH` (gravado no enrollment) |
| `AGENT_ID_PATH` | `/etc/arkame/agent.id` |
| `TOKEN_PATH` | `/etc/arkame/token.jwt` — bearer do painel (0600; no Windows, Administradores e SYSTEM) |
| `PRIVATE_KEY_PATH` | `/etc/arkame/key.pem` (idem) |
| `HOST_ROOT` | `/` nativo, `/host` em Docker |
| `SIBLING_BUCKETS` | Buckets atendidos por outros processos deste agente no host (CSV): planos e restaurações desses buckets ficam para o irmão |
| `POLL_INTERVAL_SEC` | Intervalo de consulta de planos e restaurações (padrão 60) |
| `HEARTBEAT_INTERVAL_SEC` | Intervalo do heartbeat (padrão 60) |

