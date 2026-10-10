# Assinatura do release e atualização automática

O agente troca o próprio programa quando o painel manda (`internal/atualizacao`).
Ele só aceita um programa cujo `checksums.txt` foi assinado pela **chave de
release** — Ed25519, com a parte pública embutida no agente. Este documento é
de quem publica: como gerar a chave, onde guardá-la, como trocá-la e como
ensaiar tudo sem a chave de verdade.

## O que é conferido

Antes de pôr um programa novo no lugar, o agente exige, nesta ordem:

1. **Versão maior** que a atual (X.Y.Z, comparada campo a campo).
2. **Assinatura Ed25519** do `checksums.txt` do release (`checksums.txt.ed25519`)
   contra as chaves públicas embutidas — duas posições, a atual e a próxima.
   O que se assina é `arkame-agent-release\n<versão>\n` + o conteúdo do
   `checksums.txt`: a versão entra para um `checksums.txt` antigo (e assinado)
   não poder ser servido como se fosse a versão nova.
3. **SHA-256 em dois canais**: o do `checksums.txt` assinado tem de ser igual
   ao que o painel mandou no heartbeat, e o do arquivo baixado tem de ser esse.
   Quem compromete só o download, ou só o painel, não empurra programa.
4. **Auto-teste**: o programa novo roda `self-test` com a configuração do
   agente (carrega a configuração, dá um heartbeat de teste, consulta o
   bucket) e tem de dizer que é da versão pedida.

Depois da troca o programa novo tem **5 minutos** para um heartbeat aceito pelo
painel. Sem isso, ou na terceira partida seguida sem sucesso, o programa
anterior (`arkame-agent.anterior`, ao lado) volta ao lugar sozinho. A prova é
**estendida, até 30 minutos contados da troca**, enquanto o painel não atende
(erro de rede, 5xx, 408 ou 429 no último heartbeat): o painel fora do ar não
desfaz uma versão boa. Não estendem: o `401`/`403` que dura o prazo inteiro,
o processo que nem chega a tentar um heartbeat, e o que cai (este é pela
contagem de partidas, que só conta a partida que carregou a configuração e
tem identidade e token).

Mais o que o agente confere no caminho:

- **O endereço**: só `https` (ou `http` da própria máquina), sem usuário e
  senha, sem consulta nem fragmento na base; cada redirecionamento passa pela
  mesma regra, e são no máximo cinco.
- **O arquivo**: é sempre o deste sistema (`arkame-agent_<so>_<arquitetura>`),
  pelo nome que o próprio programa sabe. O nome que o painel manda só é
  conferido; se for outro, o pedido é recusado (`file_mismatch`).
- **A unidade checksums + assinatura**: os dois vêm da mesma base. Se a
  primeira base (a do painel) entrega um par que não confere, a reserva (o
  GitHub) é tentada antes de falhar; o mesmo para o programa com SHA-256
  errado.
- **O `.novo` e o `.anterior`**: o download nunca segue link (o que estiver no
  caminho sai antes, e a abertura é exclusiva), só arquivo comum ganha
  permissão de execução, e o `.anterior` só volta ao lugar se confere com o
  SHA-256 do programa que saiu, guardado no registro da troca.
- **Sem anterior utilizável**, a prova que vence não finge que reverteu: o
  registro vira `sem_volta`, o painel recebe `rollback_unavailable` e o
  programa novo segue — o primeiro heartbeat aceito ainda confirma a troca.
- **A restauração não grava na pasta do programa** nem na da configuração do
  agente (`protected_destination`): é a pasta que o serviço pode gravar para
  se atualizar, e por ela um item restaurado como `arkame-agent` entraria sem
  assinatura nenhuma.

O cosign keyless (`checksums.txt.sig`/`.pem`) continua no release, para
auditoria humana. O agente não o confere: exigiria rede (TUF/Rekor) e uma
biblioteca pesada, e falharia atrás de proxy.

## Gerar e guardar a chave de produção (uma vez)

Feito por quem é dono do repositório, numa máquina de confiança. Nada disto é
automático, e a chave privada **nunca** entra no repositório nem aparece no
terminal.

1. Gere o par, com a privada indo para um arquivo:

   ```sh
   go run ./cmd/assinar-release gerar --saida ~/arkame-release.key
   ```

   A **privada** (o seed, 44 caracteres em base64) é gravada no arquivo, com
   modo `0600`; o comando recusa gravar por cima de um arquivo que já existe.
   No terminal saem só a **pública** (44 caracteres) e o caminho do arquivo.
   Prefira um caminho que não seja sincronizado (nuvem, backup automático) —
   ou, melhor, um disco em memória (`/dev/shm/arkame-release.key`).

2. Guarde a privada em dois lugares, **nesta ordem**:
   - **Fora do GitHub**: copie o conteúdo do arquivo para o cofre
     (gerenciador de senhas, ou papel em cofre). Sem essa cópia, perder o
     acesso ao GitHub é perder a capacidade de publicar atualização que os
     agentes aceitem — só restaria a atualização manual de cada servidor.
   - **GitHub** → repositório `arkame-app/arkame-agent` → *Settings* →
     *Environments* → **New environment** `release`. Em *Deployment protection
     rules*, marque **Required reviewers** (você) e, em *Deployment branches
     and tags*, restrinja a tags `v*`. Depois grave o segredo direto do
     arquivo, sem colar nada:

     ```sh
     gh secret set ARKAME_RELEASE_ED25519_KEY --env release \
       --repo arkame-app/arkame-agent < ~/arkame-release.key
     ```

3. **Apague o arquivo** depois de conferir que a cópia do cofre abre e que o
   segredo aparece em *Environment secrets*:

   ```sh
   shred -u ~/arkame-release.key     # ou rm, em disco em memória
   ```

4. Embuta a pública: em `internal/atualizacao/chaves.go`, preencha
   `chavePublicaAtual` com a linha da chave pública. Commit normal — a chave
   pública não é segredo. A mesma linha vai na variável
   `AGENT_RELEASE_PUBLIC_KEYS` do backoffice (ver "Publicar a versão no
   backoffice").

5. Confira antes da primeira tag:

   ```sh
   go test -tags release ./internal/atualizacao/     # a chave está preenchida e é válida
   go build -tags release ./...                      # o build de release compila
   ```

Com a constante vazia, `go build -tags release` **não compila** (o erro aponta
`chave_release.go`, que explica), o hook do GoReleaser para com a mensagem
"RELEASE PARADO" e o job `testes` da release falha no passo "Chave de release
embutida". É de propósito: um release sem a chave sairia com um agente que não
confia em atualização nenhuma.

## O que a release faz

`.github/workflows/release.yml`, ao receber uma tag `v*`, roda três jobs em
sequência. Todas as actions estão fixadas pelo SHA do commit, o GoReleaser tem
a versão fixa, e cada job declara só as permissões que usa.

1. **`testes`** (`contents: read`) — prova o commit da tag; inclui
   `go test -tags release ./internal/atualizacao/`.
2. **`goreleaser`** — compila com `-tags release`, gera o `checksums.txt`,
   assina com o cosign (keyless), publica as imagens no ghcr e cria o release
   como **rascunho**. Este job **não tem a chave Ed25519**: nele rodam o
   GoReleaser, o cosign, o buildx e o go-winres, e nenhum deles vê o segredo.
   O `checksums.txt` sai como artefato do workflow.
3. **`assinar`**, no environment `release` (espera a sua aprovação) — só
   `actions/checkout` e `actions/setup-go` (fixados), a ferramenta deste
   repositório e o `gh` do runner. Baixa o `checksums.txt` do job anterior,
   confere que é, byte a byte, o que está no rascunho do release, assina com
   `go run -tags release ./cmd/assinar-release assinar <tag> checksums.txt`
   (a ferramenta recusa assinar com uma chave que não seja uma das embutidas),
   confere a assinatura, sobe o `checksums.txt.ed25519` com
   `gh release upload` e **publica o release** (`gh release edit --draft=false`).

**Não existe release público sem a assinatura.** Se o job `assinar` falhar, ou
se a aprovação for negada ou expirar, o workflow fica vermelho e o release
continua rascunho — invisível para o `install.sh` (que baixa o "latest") e
para os agentes. Para tentar de novo: *Re-run failed jobs* no workflow. Para
desistir: apague o rascunho e a tag.

O que **já saiu** antes da aprovação: as imagens no ghcr (`:<versão>` e
`:latest`), publicadas pelo job `goreleaser`. A imagem não é o que a
atualização automática distribui; se a aprovação for negada, apague as tags
da imagem no ghcr junto com o rascunho.

### Publicar a versão no backoffice

Depois do release publicado, publique a versão no backoffice (Agente →
Versões): informe só a versão. O backoffice baixa o `checksums.txt` e o
`checksums.txt.ed25519` **daquele release**, confere a assinatura com a chave
pública configurada em `AGENT_RELEASE_PUBLIC_KEYS` (as mesmas de
`chaves.go`; duas, separadas por vírgula, durante uma rotação) e tira dali o
SHA-256 de cada sistema — o segundo canal da conferência que o agente faz.

**O painel só publica versão cujo `checksums.txt.ed25519` existe e confere.**
Release sem a assinatura (rascunho, ou de antes da atualização automática)
não é publicável: os agentes o recusariam de qualquer forma, e a onda pararia
com `signature_invalid` em todo servidor. Sem a variável configurada, publicar
é recusado em produção; em desenvolvimento passa, com aviso.

## Trocar a chave (rotação)

As duas posições existem para a troca não exigir atualização manual:

1. Gere a chave nova (`gerar --saida`, como acima) e ponha a pública em
   `chavePublicaProxima` — e, no backoffice, acrescente-a a
   `AGENT_RELEASE_PUBLIC_KEYS`. Publique uma versão (assinada ainda com a
   chave atual).
2. Espere os agentes chegarem a essa versão — eles passam a conhecer as duas.
3. Troque o segredo `ARKAME_RELEASE_ED25519_KEY` pela privada nova
   (`gh secret set … --env release < arquivo`, e apague o arquivo); no código,
   passe a pública nova para `chavePublicaAtual` e esvazie
   `chavePublicaProxima`. Do release seguinte em diante, quem assina é a nova.
   Quando não houver mais release assinado pela antiga a publicar, tire-a de
   `AGENT_RELEASE_PUBLIC_KEYS`.

Agentes que ficaram numa versão anterior ao passo 1 não aceitam releases
assinados só com a chave nova: atualizam à mão uma vez (o instalador).

Se a chave **vazar**: troque o segredo, publique uma versão com a pública nova
em `chavePublicaAtual` assinada pela chave antiga (os agentes ainda a aceitam),
e retire no backoffice as versões que não forem suas. Quem tem a chave vazada
ainda precisaria do painel para empurrar um programa (o SHA-256 vem de lá).

## Ensaiar sem a chave de produção

### A troca, numa máquina

O build de desenvolvimento aceita uma chave de teste, e só ela, quando a
variável de build a habilita:

```sh
mkdir -p /tmp/ensaio/v0.9.1
go run ./cmd/assinar-release gerar --saida /tmp/ensaio/ensaio.key   # um par só para o ensaio
PUB=<a pública que o comando imprimiu>
PKG=github.com/arkame-app/agent
go build -ldflags "-X $PKG/pkg/version.Version=0.9.1 -X $PKG/internal/atualizacao.chavePublicaDeTeste=$PUB" \
  -o /tmp/ensaio/v0.9.1/arkame-agent_linux_amd64 ./cmd/arkame-agent
(cd /tmp/ensaio/v0.9.1 && sha256sum arkame-agent_linux_amd64 > checksums.txt)
ARKAME_RELEASE_ED25519_KEY="$(cat /tmp/ensaio/ensaio.key)" ARKAME_RELEASE_ED25519_PUB=$PUB \
  go run ./cmd/assinar-release assinar 0.9.1 /tmp/ensaio/v0.9.1/checksums.txt
```

Sirva `/tmp/ensaio` por HTTP em `127.0.0.1` (o agente só aceita `http` para a
própria máquina) e publique a versão num painel local com esse endereço como
base (`AGENT_RELEASES_BASE_URL`, no painel e no backoffice) e a pública do
ensaio em `AGENT_RELEASE_PUBLIC_KEYS`. No build de release (`-tags release`)
a variável da chave de teste não existe: um programa publicado nunca aceita
chave de teste.

### O workflow de release, sem publicar nada

O workflow só roda com tag `v*`, e o job `assinar` precisa do environment
`release`. Para ensaiá-lo sem tocar no repositório de verdade nem na chave de
produção, use um **fork privado** (ou um repositório de teste com o mesmo
código):

1. No fork, crie o environment `release` e grave nele uma chave **de ensaio**
   (`gerar --saida`, `gh secret set ARKAME_RELEASE_ED25519_KEY --env release
   --repo <você>/<fork> < arquivo`). Num branch do fork, ponha a pública do
   ensaio em `chavePublicaAtual` e troque `arkame-app/arkame-agent` pelo fork
   em `.goreleaser.yaml` (o `release.github` e as imagens do ghcr).
2. Empurre uma tag de ensaio para o fork (`git tag v0.0.1 && git push <fork>
   v0.0.1`).
3. Confira, na ordem: o job `goreleaser` termina **sem** `checksums.txt.ed25519`
   no rascunho e o release não aparece como público; o job `assinar` fica à
   espera da aprovação; aprovado, sobe a assinatura e publica; baixando os dois
   arquivos do release, `ARKAME_RELEASE_ED25519_PUB=<pública do ensaio> go run
   ./cmd/assinar-release verificar 0.0.1 checksums.txt` confere.
4. Ensaie a falha: negue a aprovação (ou grave no segredo uma chave que não é
   a embutida) e confira que o workflow fica vermelho e o release continua
   rascunho.

Ao mexer nos workflows, passe o `actionlint` neles antes do commit (não é
dependência do repositório; a mudança que separou o job `assinar` foi
conferida só com um leitor de YAML, e a primeira execução de verdade é a do
ensaio acima). A lógica do job `assinar` — baixar, comparar, assinar,
conferir — roda igual fora do GitHub, com os comandos `assinar` e `verificar`
acima.

## Limites conhecidos

- **Windows com Smart App Control**: o `.exe` ainda não tem assinatura de
  código. Com o Smart App Control ligado (Windows 11), o programa novo é
  barrado ao rodar o `self-test`; a troca **não acontece**, o programa atual
  segue e o painel mostra a falha (`self_test_failed`). Nesses servidores a
  atualização automática só passa a funcionar quando o `.exe` for assinado.
  Windows 10 e Windows Server não têm o bloqueio.
- **Serviço do sistema no Linux instalado antes desta versão**: a unit antiga
  não deixa o serviço gravar na pasta do programa (`ProtectSystem=full`). O
  agente percebe, não declara a capacidade e o painel mostra "atualize
  manualmente uma última vez": rodar o instalador
  (`curl -fsSL https://get.arkame.app/install.sh | sudo sh`) troca o programa
  **e** regrava a unit (`arkame-agent service refresh`).
- **Docker**: o programa novo vai para `bin/` no volume da configuração
  (`/etc/arkame/bin/`), e o programa da imagem o executa ao subir se ele
  conferir (assinatura e SHA-256, de novo, sem rede) e for mais novo que a
  imagem. Se não provar que funciona, é abandonado e vale o da imagem. A
  imagem em si não muda: trocar a imagem continua manual
  (`docker run --pull always …`). `ARKAME_AUTO_UPDATE=off` desliga.
- **Agente rodando à mão** (sem systemd, launchd, SCM ou Docker): não se
  atualiza sozinho — ninguém religaria o processo. O comando
  `arkame-agent update` continua valendo.
- **Painel fora do ar durante a prova**: a prova é estendida enquanto o
  painel não atende, até 30 minutos contados da troca. Passado o teto, o
  programa novo é revertido mesmo que a culpa seja do painel; a versão
  revertida não volta sozinha àquele servidor — no painel, "Tentar de novo"
  (ou, no backoffice, "Retomar e tentar de novo" para a onda inteira). O
  painel não conta esse tipo de falha contra a versão.
- **Windows: a janela entre os dois renames.** No Windows o `.exe` em uso não
  pode ser substituído, só renomeado: a troca é `arkame-agent.exe` →
  `arkame-agent.anterior.exe` e depois `arkame-agent.novo.exe` →
  `arkame-agent.exe`. Entre os dois o caminho do programa fica vazio. (No
  Linux e no macOS não há essa janela: o anterior é um segundo nome do atual
  e o novo entra por um rename só; o caminho dos dois renames só é usado em
  sistema de arquivos sem link.) O que o agente faz para estreitá-la: tudo o
  que pode falhar ou demorar acontece antes (o registro da troca já está no
  disco, o novo já foi conferido e sincronizado); o segundo rename é tentado
  cinco vezes (um antivírus lendo o arquivo recém-baixado o segura por um
  instante) antes de desfazer o primeiro. **O que resta**: uma queda de
  energia, ou o processo morto, exatamente entre os dois renames deixa o
  serviço sem programa para subir — o SCM não tem o que executar, e nenhum
  código do agente roda para consertar. Os dois arquivos ficam inteiros ao
  lado, e o conserto é um destes: rodar o instalador de novo; renomear
  `arkame-agent.anterior.exe` de volta; ou executar o anterior, que põe o
  programa no lugar a partir do `.novo` ou do `.anterior` que conferem com o
  registro (`arkame-agent.anterior.exe run --config …`, uma vez; depois
  `sc start`). Outro agente da mesma máquina que divida o programa, e o
  comando `update`, fazem a mesma recuperação sozinhos.
- **`service refresh` e a unit editada à mão**: o refresh só reescreve a unit
  do systemd quando ela é um modelo do agente sem edição. Se tem qualquer
  linha que os modelos não gravam (um `Environment=`, um limite de CPU, uma
  pasta a mais em `ReadWritePaths=`), fica como está, e a pasta do programa é
  liberada num drop-in ao lado
  (`/etc/systemd/system/<serviço>.service.d/10-arkame-atualizacao.conf`), que
  o systemd soma à unit. Quem tirou o `ProtectSystem=` da unit, ou já liberou
  a pasta, não ganha drop-in.
- **Docker, entre conferir e executar**: o lançador abre o programa do volume
  uma vez, confere o SHA-256 pelo descritor e o executa pelo descritor
  (`/proc/self/fd/N`): trocar o arquivo do caminho no meio não muda o que
  roda. Quem já pode gravar *dentro* do arquivo aberto (root no volume) não é
  barrado por isso.
