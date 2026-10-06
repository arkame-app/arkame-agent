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
anterior (`arkame-agent.anterior`, ao lado) volta ao lugar sozinho.

O cosign keyless (`checksums.txt.sig`/`.pem`) continua no release, para
auditoria humana. O agente não o confere: exigiria rede (TUF/Rekor) e uma
biblioteca pesada, e falharia atrás de proxy.

## Gerar e guardar a chave de produção (uma vez)

Feito por quem é dono do repositório, numa máquina de confiança. Nada disto é
automático, e a chave privada **nunca** entra no repositório.

1. Gere o par:

   ```sh
   go run ./cmd/assinar-release gerar
   ```

   Saem duas linhas em base64: a **privada** (o seed, 44 caracteres) e a
   **pública** (44 caracteres). Nada é gravado em disco. Não cole a privada em
   chat, issue ou histórico de shell compartilhado.

2. Guarde a privada em dois lugares:
   - **GitHub** → repositório `arkame-app/arkame-agent` → *Settings* →
     *Environments* → **New environment** `release`. Em *Deployment protection
     rules*, marque **Required reviewers** (você) e, em *Deployment branches
     and tags*, restrinja a tags `v*`. Em *Environment secrets*, crie
     `ARKAME_RELEASE_ED25519_KEY` com a linha da chave privada.
   - **Fora do GitHub**: uma cópia offline (gerenciador de senhas ou papel em
     cofre). Sem ela, perder o acesso ao GitHub é perder a capacidade de
     publicar atualização que os agentes aceitem — só restaria a atualização
     manual de cada servidor.

3. Embuta a pública: em `internal/atualizacao/chaves.go`, preencha
   `chavePublicaAtual` com a linha da chave pública. Commit normal — a chave
   pública não é segredo.

4. Confira antes da primeira tag:

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

`.github/workflows/release.yml`, ao receber uma tag `v*`:

1. Job `testes` — inclui `go test -tags release ./internal/atualizacao/`.
2. Job `goreleaser`, no environment `release` (espera a sua aprovação):
   compila com `-tags release`, gera o `checksums.txt`, assina com o cosign e
   com `go run -tags release ./cmd/assinar-release assinar <versão>
   checksums.txt` (grava `checksums.txt.ed25519`). A ferramenta recusa assinar
   com uma chave que não seja uma das embutidas.
3. Passo final — `assinar-release verificar`: a assinatura publicada confere
   com as chaves que os agentes têm.

Depois do release, publique a versão no backoffice (Agente → Versões): cole o
`checksums.txt` do release. É dali que o painel tira o SHA-256 que manda aos
agentes — o segundo canal.

## Trocar a chave (rotação)

As duas posições existem para a troca não exigir atualização manual:

1. Gere a chave nova e ponha a pública em `chavePublicaProxima`. Publique uma
   versão (assinada ainda com a chave atual).
2. Espere os agentes chegarem a essa versão — eles passam a conhecer as duas.
3. Troque o segredo `ARKAME_RELEASE_ED25519_KEY` pela privada nova; no código,
   passe a pública nova para `chavePublicaAtual` e esvazie
   `chavePublicaProxima`. Do release seguinte em diante, quem assina é a nova.

Agentes que ficaram numa versão anterior ao passo 1 não aceitam releases
assinados só com a chave nova: atualizam à mão uma vez (o instalador).

Se a chave **vazar**: troque o segredo, publique uma versão com a pública nova
em `chavePublicaAtual` assinada pela chave antiga (os agentes ainda a aceitam),
e retire no backoffice as versões que não forem suas. Quem tem a chave vazada
ainda precisaria do painel para empurrar um programa (o SHA-256 vem de lá).

## Ensaiar sem a chave de produção

O build de desenvolvimento aceita uma chave de teste, e só ela, quando a
variável de build a habilita:

```sh
go run ./cmd/assinar-release gerar                      # um par só para o ensaio
PUB=<pública do ensaio>; PRIV=<privada do ensaio>
PKG=github.com/arkame-app/agent
go build -ldflags "-X $PKG/pkg/version.Version=0.9.1 -X $PKG/internal/atualizacao.chavePublicaDeTeste=$PUB" \
  -o /tmp/ensaio/v0.9.1/arkame-agent_linux_amd64 ./cmd/arkame-agent
(cd /tmp/ensaio/v0.9.1 && sha256sum arkame-agent_linux_amd64 > checksums.txt)
ARKAME_RELEASE_ED25519_KEY=$PRIV ARKAME_RELEASE_ED25519_PUB=$PUB \
  go run ./cmd/assinar-release assinar 0.9.1 /tmp/ensaio/v0.9.1/checksums.txt
```

Sirva `/tmp/ensaio` por HTTP em `127.0.0.1` (o agente só aceita `http` para a
própria máquina) e publique a versão num painel local com esse endereço como
base. No build de release (`-tags release`) a variável da chave de teste não
existe: um programa publicado nunca aceita chave de teste.

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
- **Painel fora do ar durante a prova**: sem heartbeat aceito em 5 minutos o
  programa novo é revertido, mesmo que a culpa seja do painel. A versão
  revertida não volta sozinha àquele servidor; no painel, "Tentar de novo".
