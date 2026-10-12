# Escala do agente

Como o agente se comporta com centenas de milhares a milhões de arquivos, o que foi
medido e como medir de novo. O protocolo com o painel (índice em lotes e delta) está no
contrato do monorepo do painel; aqui fica o lado do agente.

## O que mudou (de `f52fb25` para cá)

Até a v0.4.31, a cada backup o agente relia e re-hasheava todos os arquivos, fazia um
`HeadObject` por arquivo, um arquivo por vez, guardava o índice inteiro na memória e o
mandava num único `POST /complete`. Medido em produção simulada: 285 MB de RSS com 133 mil
arquivos, 2,1 GB com 947 mil, ~1 h para um backup sem mudança de 947 mil.

| Peça | Antes | Agora |
|---|---|---|
| Arquivo que não mudou | lido inteiro + 1 `HeadObject` | nem lido nem consultado (estado local); 32 versões sorteadas conferidas por execução |
| Envio | 1 arquivo por vez | N trabalhadores (padrão `min(4, CPUs)`), no máximo 2 multiparts |
| Limite de banda | um limitador por leitor | um por execução |
| Índice | mapa inteiro na memória, 1 POST | lotes de até 5.000 entradas / 2 MiB durante o backup; delta |
| Sondagem do bucket | lista o bucket inteiro de hora em hora | acima de 100 mil versões, 1 vez por dia |

## Estado local

Um arquivo [bbolt](https://github.com/etcd-io/bbolt) por agente + armazenamento, em
`ARKAME_STATE_DIR` (padrão por sistema: ver `docs/USAGE.pt-BR.md`), com um bucket por
plano. Guarda, por arquivo: tamanho, data de modificação (ns), ctime, sha256, `version_id`
no bucket, o dia da próxima reconferência e a "geração" (sessão) que gravou a entrada.

É um acelerador derivável. A restauração não depende dele; apagá-lo é seguro.

O arquivo é de um agente + armazenamento + bucket + **endereço do armazenamento**
(`STORAGE_ENDPOINT`). Qualquer um diferente e ele é descartado: o mesmo armazenamento do
painel reapontado para outro provedor mantém o id e o nome do bucket, mas as versões
registradas não existem lá.

**A pasta é só do administrador.** O estado decide o que o backup lê e o que ele afirma ao
painel, e guarda o nome de todos os arquivos do servidor. A pasta é criada fechada — 0700; no
Windows, com a DACL protegida de Administradores e SYSTEM, herdada pelo que for gravado
dentro (o 0700 lá não restringe nada, e `%ProgramData%` deixa qualquer usuário ler e criar
arquivos). Pasta que **já existia em nome de outro usuário** (no Windows, também a que é
uma junção ou um link) não é usada, nem a que teria de ser criada dentro da pasta de outro
usuário: o estado vai para `<pasta da configuração>/estado` e, se essa também não servir, o
backup roda sem estado. A permissão de uma pasta que já existia não é alterada; o `.db`, a
medição de ocupação e o temporário do índice são protegidos um a um. Arquivo de estado de
outro dono é descartado. A pasta da configuração também é o destino quando a principal não
aceita escrita (`/host` só para leitura); com o estado **em uso por outro processo**, o
backup roda sem estado em vez de abrir um segundo.

**Quando entra em uso.** Só quando o painel oferece o índice em lotes (`index.mode =
"batches"` no `/sessions/start`). Contra um painel de antes disso o agente é a v0.4.31: lê
e confere tudo a cada backup e não cria nada na pasta do estado (nem a medição de ocupação
do bucket fica no disco). Assim o agente novo pode ir a campo antes do painel novo sem
mudar de comportamento. `ARKAME_STATE=always` força o estado também contra o painel antigo
(era o comportamento até `8cfb9d1`, e é o que as linhas "depois / inline" da medição
abaixo mostram); `ARKAME_STATE=off` o desliga de vez. O envio em paralelo e a medição de
ocupação uma vez por dia em bucket grande não dependem do painel.

**Atalho.** Tamanho + mtime + ctime iguais aos da entrada → o arquivo não é lido e o bucket
não é consultado. Não servem de atalho:

- entrada de uma sessão que o painel não confirmou (ver "duas fases");
- entrada de **data suspeita**: a data de modificação estava a menos de 2 s do início da
  leitura — uma alteração logo depois pode cair na mesma data;
- entrada que venceu na **reconferência rotativa**: cada arquivo é relido e conferido com
  `HeadObject` uma vez a cada 30 dias (`ARKAME_VERIFY_DAYS`), em dia espalhado por um hash
  da chave, com teto por execução: a fração do ciclo que passou desde a última execução
  confirmada, com 10% de folga — 1/30 dos arquivos num plano diário, 7/30 num semanal, o
  ciclo inteiro depois de 30 dias parado —, no mínimo 1.000 (o que não coube fica para a
  execução seguinte). A contagem de arquivos do plano é atualizada também em sessão
  parcial. Data de reconferência além de hoje + o ciclo (o relógio do servidor esteve
  adiantado) conta como vencida. É o que pega conteúdo alterado com as mesmas datas
  (contado em `changed_same_mtime`) e objeto que sumiu do bucket;
- entrada que recebeu **baixa** (ver "Expurgo e restauração");
- nada, quando a **amostra** não confere ou o estado é de **outra época** (abaixo);
- tudo, com `ARKAME_REHASH_ALL=1` (enquanto a variável estiver ligada) ou `run
  --rehash-all` (a flag vale para UM backup de cada plano depois da partida; os seguintes
  voltam ao atalho — num serviço a flag ficaria na linha de comando para sempre).

No Windows o Go não dá o ctime: lá o atalho é tamanho + mtime, e "mudou sem mudar a data"
depende só da reconferência rotativa.

**Amostra no bucket.** O atalho afirma, sem consultar o bucket, que a versão registrada
continua lá — e deixa de ser verdade quando alguém mexe no bucket por fora (versões
apagadas, bucket recriado com o mesmo nome, regra de ciclo de vida). Por isso toda execução
que vai usar o atalho começa conferindo 32 entradas sorteadas do estado: `HeadObject` com o
`VersionId` registrado, que tem de devolver o mesmo sha256 e o mesmo tamanho. **Uma que
falhe — versão que sumiu, outro conteúdo, bucket que não respondeu — e a execução inteira lê
e confere tudo**, como a v0.4.31: o que sumiu sobe de novo, o que está lá não é reenviado. A
amostra é fixa (não cresce com o plano) e sorteada de novo a cada execução. Bucket
esvaziado, recriado ou trocado derruba todas as entradas e é pego com certeza; perda parcial
é pega com a probabilidade da fração perdida (10% perdidos → 97% numa execução; 1% → 28% por
execução, 96% em dez), e a reconferência rotativa cobre o resto. Não há objeto-sentinela:
ele só pegaria os casos em que todas as entradas caem, que a amostra já pega, e exigiria
gravar fora do prefixo dos dados (permissão a mais; regra de ciclo de vida que o apagasse
viraria alarme falso para sempre).

**Época.** O atalho só vale se a última sessão confirmada do estado é a que o painel
devolve como `base_session_id`. Se não é — cópia antiga do arquivo restaurada (rollback de
snapshot, VM clonada), painel que perdeu sessões ou religou os lotes —, a execução lê e
confere tudo e manda o inventário inteiro. Contra o painel sem lotes
(`ARKAME_STATE=always`) não há base para comparar, e vale só a amostra.

**Duas fases.** A entrada nasce com a geração da sessão que a gravou. A geração só passa a
valer com o `/complete` 200 daquela sessão. `kill -9`, queda de energia, painel que recusa:
as entradas ficam no arquivo, mas a execução seguinte as trata como ausentes (lê, confere no
bucket — sem reenviar o que já subiu — e leva ao índice de novo). Uma exceção, que evita
reler tudo quando só a resposta se perdeu: se o painel devolve como `base_session_id` a
sessão pendente **e o agente a tinha marcado como pronta** antes de mandar o `/complete`,
ela está confirmada. A marca só é gravada com o estado inteiro e alinhado ao índice: quando
o agente decide não confirmar (o estado falhou no meio, o inventário de sessão parcial não
pôde ser alinhado), a sessão nunca é adotada, e a execução seguinte cai na regra da época.
O índice refeito numa sessão nova leva a marca com o id da sessão nova.

**Removidos e fora da seleção.** Só depois de uma varredura completa e sem erro (nenhuma
pasta ilegível, nenhum arquivo que falhou, nenhum pulado por estar só na nuvem). O que o
estado conhecia e não foi visto vira `removed` (ou `unselected`, se saiu das pastas ou caiu
numa exclusão do plano). A entrada vira uma lápide e só some quando o painel confirmar a
sessão que levou a remoção; senão, a remoção sai de novo.

**Quando o estado não serve.** Arquivo ausente, cortado, adulterado (cada entrada tem CRC
próprio), de outro formato, de outro armazenamento ou de outro prefixo de chave → o backup
roda em modo completo (hash de tudo + `HeadObject`, o comportamento de sempre), manda o
inventário inteiro e reconstrói o estado. Nenhum arquivo igual é reenviado: o
`HeadObject` acha o mesmo sha256 e reaproveita a versão.

**Expurgo e restauração.** Depois de `purge.Run`, as entradas cuja versão foi apagada
recebem baixa. O mesmo quando uma restauração não acha a versão no bucket (`not_found`) ou
baixa um conteúdo que não confere com o índice (tamanho ou sha256). A entrada não é apagada:
deixa de afirmar versão e de servir de atalho. O backup seguinte lê o arquivo, o confere —
ou envia — e o leva ao índice de novo; se o arquivo saiu da origem nesse meio-tempo, é da
entrada que sai o `removed`. Com um backup em curso, a baixa espera ele fechar. Ela vai a
todos os arquivos de estado do agente + armazenamento (o da pasta principal e o da pasta da
configuração, se os dois existirem) e os abre como estão, sem conferir de quem são — a
identidade do arquivo é assunto do backup.

**Inventário de sessão parcial.** Quando a sessão vai ao painel como inventário (`full`,
ou inline) sem a varredura ter sido completa, as entradas que ela não viu saem do estado
antes do `/complete`: no painel esses arquivos ficam fora da sessão, e o delta seguinte tem
de mandá-los de novo.

**Desinstalação.** `uninstall` apaga o estado deste agente (ele guarda os nomes dos
arquivos do servidor): o do armazenamento de hoje e o de armazenamentos anteriores, na pasta
da configuração (`ARKAME_STATE_DIR`), na padrão do sistema, em `<pasta da
configuração>/estado` e na `ARKAME_STATE_DIR` posta só na unit do systemd. O que a
configuração de outro agente da máquina usa fica.

**Memória.** O bbolt lê por mapeamento de memória. No Linux as páginas mapeadas são
devolvidas ao sistema a cada 20 mil consultas (`madvise`), para o arquivo de estado (~250 B
por arquivo) não contar na memória do processo. Fora do Linux elas ficam mapeadas até o fim
da execução (memória de arquivo, que o sistema recolhe sob pressão).

## Índice em lotes e delta

O agente declara `index_batches` e `index_delta`. Se a resposta do `/sessions/start` traz
`index.mode = "batches"`, as entradas vão por `POST …/sessions/{sid}/files` durante o backup
e o `/complete` leva só estatísticas e a contagem. Sem `index` na resposta (painel de antes
do contrato), o `/complete` é o de sempre, com o `version_map` inteiro — byte a byte o da
v0.4.31 (conferido: 32.434.029 bytes nos dois, ver a medição).

- Cada lote fechado vai para um arquivo temporário (gzip) na pasta do estado, e o envio lê
  de lá: a memória fica no lote em montagem; o painel fora do ar não para o backup; e um
  lote que o painel perdeu é reenviado idêntico (`409 seq_gap`, `409 index_mismatch`).
  Os limites do lote que o painel anuncia valem entre 10 e 50.000 entradas e entre 64 KiB e
  16 MiB; na finalização cada lote tem direito a mais 5 tentativas (a conta é por lote).
- **Delta** quando o painel oferece e a última sessão confirmada do estado é a
  `base_session_id`. Arquivo inalterado não gera entrada; sem mudança são zero lotes.
- Recusa que reenviar não resolve (`409 base_mismatch`, `seq_conflict`, `session_closed`):
  a sessão leva `/fail` (`index_rejected`) e o índice vai inteiro numa **sessão nova**,
  tirado do estado local (só os arquivos vistos e guardados nesta execução) — sem reler
  arquivo, sem reenviar, sem repetir os comandos antes/depois do plano. Uma vez só.

## Medição local (10/10/2026)

Máquina de desenvolvimento (12 CPUs, árvore em tmpfs), **S3 falso e painel falso em
processo** (`arkame-bancada medir`): mede o agente, não o provedor nem o painel. 100.000
arquivos sintéticos, 210 MB (lognormal, mediana 1 KiB), semente 42. Um processo do agente
por caso; o tempo vai do disparo do plano ao fechamento da sessão (inclui a partida do
agente e até 1 s de espera pela consulta de planos). RSS = `VmHWM` do agente ao fechar a
sessão. "Antes" = `f52fb25` contra o painel sem lotes; "depois" = este código.

| Caso | Agente / painel | Tempo | CPU | RSS | `HeadObject` | `PutObject` | Índice | Maior POST |
|---|---|---|---|---|---|---|---|---|
| primeira | antes / inline | 97,9 s | 97 s | 189 MB | 100.000 | 100.000 | 1 POST de 32,4 MB | 32,4 MB |
| primeira | depois / lotes | **50,0 s** | 163 s | **45 MB** | 100.000 | 100.000 | 20 lotes, 33,8 MB no total | **1,73 MB** |
| primeira | depois / inline | 58,6 s | 182 s | 198 MB | 100.000 | 100.000 | 1 POST de 32,4 MB | 32,4 MB |
| sem mudança | antes / inline | 51,6 s | 53 s | 189 MB | 100.000 | 0 | 1 POST de 32,4 MB | 32,4 MB |
| sem mudança | depois / lotes | **2,0 s** | 5 s | **32 MB** | **0** | 0 | delta, **0 lotes**; `/complete` de 380 B | 380 B |
| sem mudança | depois / inline | 3,1 s | 4 s | 228 MB | **0** | 0 | 1 POST de 32,4 MB | 32,4 MB |
| 0,1% alterado (100) | antes / inline | 42,0 s | 44 s | 187 MB | 100.000 | 100 | 1 POST de 32,4 MB | 32,4 MB |
| 0,1% alterado (100) | depois / lotes | **2,4 s** | 7 s | **35 MB** | **100** | 100 | delta, 1 lote de 100 entradas, 35 KB | 35 KB |
| 0,1% alterado (100) | depois / inline | 3,3 s | 4 s | 227 MB | **100** | 100 | 1 POST de 32,4 MB | 32,4 MB |
| estado perdido | depois / lotes | 23,4 s | 77 s | 45 MB | 100.000 | **0** | full, 20 lotes | 1,73 MB |

Leituras:

- **Sem mudança:** nenhuma requisição de arquivo ao bucket (as que sobram são o
  `GetBucketVersioning` da sessão e a sondagem da partida). **Desde 12/10** há também a
  amostra: 32 `HeadObject` com `VersionId` por execução, qualquer que seja o tamanho do
  plano (ver "Custo da amostra", abaixo). 20 µs por arquivo: 1 milhão
  ficaria em ~20–40 s com o cache de disco quente; com disco frio manda a varredura
  (`lstat` de cada arquivo), que é o que a bancada na VM tem de medir.
- **Memória:** com lotes, o pico não acompanha o número de arquivos (45 MB na primeira com
  100 mil; no teste em processo, 37–59 MiB com 300 mil). No caminho inline o
  mapa inteiro continua na memória, como antes — é o protocolo antigo, e o painel o limita
  em 150 mil arquivos.
- **Painel antigo:** os três corpos do `/complete` têm exatamente o tamanho dos da
  `f52fb25` (32.434.029, 32.434.016 e 32.435.025 bytes) e nenhum `POST /files` acontece.
- **CPU na primeira execução** sobe (4 trabalhadores assinando e enviando em paralelo por
  HTTP local); o tempo cai à metade. Contra um provedor de verdade o ganho depende da
  latência dele.
- **Estado perdido:** lê e confere tudo (100.000 `HeadObject`), não reenvia nada, e o
  agente conta os arquivos como novos (`files_new`) porque não tem como saber.
- **Sondagem:** o bucket da bancada tem 100.000 versões (no limiar, que é "acima de 100
  mil"), então a ocupação ainda é medida a cada partida; a bancada mata o agente antes de
  a listagem acabar. O caso grande está em `TestOcupacaoDeBucketGrandeEMedidaUmaVezPorDia`.

Teste em processo (`TestEscalaMuitosArquivosPequenos`, caminho inteiro do daemon, bucket e
painel falsos sem memória própria), arquivos de ~48 bytes:

| Arquivos | Primeira | Sem mudança | 0,1% alterado | Pico de RSS (primeira / sem mudança / 0,1%) |
|---|---|---|---|---|
| 20.000 | 9,1 s | 0,24 s | 0,28 s | 48 / 33 / 34 MiB |
| 100.000 | 64,7 s | 2,4 s | 2,6 s | 49 / 35 / 36 MiB |
| 300.000 | 147 s | 4,1 s | 4,1 s | 50 / 37 / 59 MiB |

O pico não acompanha o número de arquivos. Antes do commit `5a9581d` acompanhava: 153 MiB
com 300 mil sem mudança, todos do arquivo de estado mapeado (o bbolt percorria o arquivo
inteiro a cada abertura). 1 milhão não coube nesta máquina (o `/tmp` tem 1.048.576 inodes
e a memória estava no limite): fica para a VM, com `ARKAME_TESTE_ESCALA=1000000`.

## Custo da amostra (12/10/2026)

A conferência da amostra no começo de cada execução custa **32 requisições ao bucket**
(`HeadObject` com `VersionId`; uma por arquivo quando o plano tem menos de 32) e uma passada
pelo estado local para sortear as entradas. Mesma máquina e mesmo teste em processo
(`TestEscalaMuitosArquivosPequenos`, 300.000 arquivos), antes (`05c9506`) e depois:

| Caso | Antes | Depois | Requisições ao bucket (depois) |
|---|---|---|---|
| sem mudança | 4,28 s · 36 MiB | 4,13 s · 38 MiB | `GetBucketVersioning` 1 + amostra **32** |
| 0,1% alterado (300) | 4,22 s · 61 MiB | 4,57 s · 64 MiB | 300 `HeadObject` + 300 `PutObject` + amostra **32** |

A diferença de tempo fica dentro do ruído de uma execução para outra. A passada pelo estado,
medida à parte num arquivo de 1 milhão de entradas (346 MB, cache quente): **0,76 s** —
1,3% dos ~58 s do backup sem mudança de 1 milhão medido na VM. Com o disco frio ela lê o
arquivo de estado em sequência, que a varredura leria de qualquer forma (toda entrada é
consultada). Contra um provedor de verdade somam-se 32 `HeadObject` em até 8 conexões:
uma fração de segundo. Quando a amostra falha, o custo é o do modo completo (um
`HeadObject` e uma leitura por arquivo), que é o que a v0.4.31 fazia em todo backup.

No bucket falso a consulta da amostra é contada à parte (`HeadObjectVersion`), para a coluna
`HeadObject` das tabelas seguir sendo "uma consulta por arquivo".

## Como medir

```bash
go build -o bin/arkame-bancada ./cmd/arkame-bancada
go build -o bin/arkame-agent   ./cmd/arkame-agent

bin/arkame-bancada gerar --raiz /dados/arvore --arquivos 1000000 --mediana 4096 --semente 42
bin/arkame-bancada medir --agente bin/arkame-agent --raiz /dados/arvore \
    --casos primeira,sem-mudanca,alterar:0.001,alterar:0.1,cache-perdido \
    --saida depois.json            # --painel antigo mede o caminho inline; --sem-catalogo acima de ~1 milhão
bin/arkame-bancada mutar --raiz /dados/arvore --mutacao mudar-sem-tocar --quanto 0.001

# o teste de escala em processo, com teto de RSS (padrão 300 MiB)
ARKAME_TESTE_ESCALA=1000000 go test -run TestEscala -timeout 2h -v ./internal/daemon/
```

Mutações: `alterar`, `adicionar`, `remover`, `renomear-pasta`, `tocar-sem-mudar`,
`mudar-sem-tocar`. A última repõe a data de modificação, mas não o ctime (o sistema não
deixa): no Linux e no macOS o atalho já a recusa; o caso "ninguém vê" é o do Windows.

Para o "antes", `git archive f52fb25 | tar -x -C /tmp/antes && (cd /tmp/antes && go build
-o agente-antes ./cmd/arkame-agent)` e `--agente agente-antes --painel antigo`.

O S3 falso e o painel falso respondem em microssegundos: os tempos daqui são o piso do
agente. Contra RustFS e contra o painel de verdade (448 Mi / Postgres 512 Mi) a medição é
a da VM.

## O que falta medir

- 1 e 5 milhões de arquivos (tempo sem mudança com disco frio, RSS, tamanho do estado e do
  arquivo temporário de lotes: ~330 B por entrada antes do gzip).
- Contra o painel de verdade, com os limites de produção: `/files` sob carga, o `/complete`
  em lotes, `base_mismatch` e `seq_gap` reais.
- Windows (sem ctime; pasta do estado em `%ProgramData%`; arquivo temporário que não pode
  ser apagado aberto) e macOS (`madvise` não é usado; ctime vem de `Ctimespec`). **Nada do
  que protege a pasta do estado foi executado em Windows**: a DACL da pasta criada, a
  leitura do dono (SID), a recusa de junção e a comparação de caminhos sem caixa só
  compilam (`GOOS=windows`) e têm teste em Linux com o dono simulado. Conferir numa máquina
  de verdade: `icacls %ProgramData%\Arkame\estado` depois do primeiro backup (só
  Administradores e SYSTEM), e o backup com `%ProgramData%\Arkame` criada antes por um
  usuário comum (o estado tem de ir para a pasta da configuração).
- A amostra contra provedores de verdade: `HeadObject` com `VersionId` na OCI, B2 e R2
  (permissão `s3:GetObjectVersion`; a credencial que não a tiver faz todo backup rodar em
  modo completo, com o aviso no log).
- Provedores de verdade com 4 envios em paralelo (limites de requisição da OCI, B2, R2).
