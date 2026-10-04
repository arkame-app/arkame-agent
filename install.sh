#!/bin/sh
# Instalador do agente Arkame para Linux e macOS.
#
#   curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=atk_...
#
# Com --token (o código de instalação do painel), faz tudo num comando: baixa,
# pergunta a chave do bucket, testa no bucket, registra o servidor e deixa o
# agente rodando como serviço. As perguntas vão ao terminal (/dev/tty), porque
# a entrada deste script é o próprio curl. Sem --token, só instala o binário.
#
# Variáveis reconhecidas:
#   ARKAME_VERSION        versão a instalar (padrão: a mais recente)
#   ARKAME_BIN_DIR        onde instalar (padrão: /usr/local/bin — ou /opt/arkame/bin,
#                         quando /usr/local/bin não é só do root —, ou
#                         ~/.local/bin sem root)
#   PANEL_URL             painel a usar (padrão: https://save.arkame.app)
#   ARKAME_DOWNLOAD_BASE  espelho de onde baixar os pacotes (padrão: releases do
#                         GitHub). Serve a parceiros whitelabel e a redes que
#                         bloqueiam o github.com.
#
# Este script é POSIX sh de propósito: roda igual em Debian, Alpine, RHEL e
# macOS, sem depender de bash.
set -eu

REPO="arkame-app/arkame-agent"
PANEL_URL="${PANEL_URL:-https://save.arkame.app}"
DOWNLOAD_BASE="${ARKAME_DOWNLOAD_BASE:-}"
TOKEN=""
SERVICE_NAME="arkame-agent"
SERVICE_SCOPE=""
CONFIG_FILE=""
INSTALL_SERVICE="true"
SKIP_CHECKSUM="false"

# ── saída ────────────────────────────────────────────────────────────────────
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m'); GREEN=$(printf '\033[32m')
  YELLOW=$(printf '\033[33m'); RESET=$(printf '\033[0m')
else
  BOLD=''; RED=''; GREEN=''; YELLOW=''; RESET=''
fi

info() { printf '%s\n' "  $*"; }
ok()   { printf '%s\n' "  ${GREEN}✓${RESET} $*"; }
warn() { printf '%s\n' "  ${YELLOW}!${RESET} $*" >&2; }
die()  { printf '%s\n' "  ${RED}✗${RESET} $*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Instalador do agente Arkame.

Uso:
  curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=atk_xxx
  curl -fsSL https://get.arkame.app/install.sh | sh -s -- --token=atk_xxx
                                     (sem sudo: serviço só do seu usuário)
  curl -fsSL https://get.arkame.app/install.sh | sh        (só o binário)

Opções:
  --token=CODIGO        código de instalação do painel (Servidores → Novo servidor)
  --panel-url=URL       painel a usar (padrão: https://save.arkame.app)
  --service-name=NOME   nome do serviço (use um por credencial de storage)
  --config=ARQUIVO      arquivo de configuração deste agente (padrão:
                        /etc/arkame/agent.env; sem sudo, no escopo user,
                        ~/.config/arkame/agent.env). Com mais de um agente na
                        máquina, um arquivo por agente, junto de --service-name
  --service-scope=X     system (todo o host, exige sudo) ou user (sem sudo;
                        padrão quando o script não roda como root)
  --no-service          só instala o binário, sem registrar serviço
  --version=vX.Y.Z      instala uma versão específica
  --download-base=URL   espelho de onde baixar (exige --version)
  --skip-checksum       instala sem conferir o SHA-256 do pacote (só quando o
                        checksums.txt não pode ser baixado ou conferido)
  --help                mostra esta ajuda
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --token=*)         TOKEN="${arg#*=}" ;;
    --panel-url=*)     PANEL_URL="${arg#*=}" ;;
    --service-name=*)  SERVICE_NAME="${arg#*=}" ;;
    --service-scope=*) SERVICE_SCOPE="${arg#*=}" ;;
    --config=*)        CONFIG_FILE="${arg#*=}" ;;
    --version=*)       ARKAME_VERSION="${arg#*=}" ;;
    --download-base=*) DOWNLOAD_BASE="${arg#*=}" ;;
    --no-service)      INSTALL_SERVICE="false" ;;
    --skip-checksum)   SKIP_CHECKSUM="true" ;;
    --help|-h)         usage; exit 0 ;;
    *) die "opção desconhecida: $arg (use --help)" ;;
  esac
done

# O código vai para a linha de comando do agente: só a forma que o painel gera.
if [ -n "$TOKEN" ] && ! printf '%s' "$TOKEN" | grep -Eq '^atk_[A-Za-z0-9_-]{16,}$'; then
  die "código de instalação inválido. Copie de novo o comando do painel."
fi

# ── plataforma ───────────────────────────────────────────────────────────────
detect_platform() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)

  case "$os" in
    linux|darwin) ;;
    mingw*|msys*|cygwin*)
      # O mesmo comando do painel: o setup se eleva, se copia para Program
      # Files e entra em "Aplicativos instalados"; o install de um .zip não.
      codigo="${TOKEN:-<código>}"
      die "no Windows, use o comando do painel (Windows + R, colar, Enter):

     cmd /c \"curl -fsSLo \"%TEMP%\\arkame-agent.exe\" https://get.arkame.app/agente.exe && \"%TEMP%\\arkame-agent.exe\" setup --token=$codigo || pause\"" ;;
    *) die "sistema não suportado: $os" ;;
  esac

  case "$arch" in
    x86_64|amd64)  arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "arquitetura não suportada: $arch (suportadas: x86_64, aarch64)" ;;
  esac

  OS="$os"; ARCH="$arch"
}

# ── download ─────────────────────────────────────────────────────────────────
have() { command -v "$1" >/dev/null 2>&1; }

fetch() { # fetch <url> <destino>
  if have curl; then
    curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"
  elif have wget; then
    wget -q -O "$2" "$1"
  else
    die "preciso de curl ou wget para baixar o agent"
  fi
}

fetch_stdout() {
  if have curl; then
    curl -fsSL --retry 3 --retry-delay 2 "$1"
  elif have wget; then
    wget -q -O - "$1"
  else
    die "preciso de curl ou wget"
  fi
}

resolve_version() {
  if [ -n "${ARKAME_VERSION:-}" ]; then
    printf '%s' "$ARKAME_VERSION"
    return
  fi
  if [ -n "$DOWNLOAD_BASE" ]; then
    die "com ARKAME_DOWNLOAD_BASE definido, informe também a versão (--version=vX.Y.Z ou ARKAME_VERSION)"
  fi
  # A API de releases devolve JSON; o tag_name é o suficiente e evita depender
  # de jq numa máquina recém-instalada.
  v=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$v" ] || die "não consegui descobrir a versão mais recente. Tente de novo, ou fixe uma com --version=vX.Y.Z"
  printf '%s' "$v"
}

sha256_of() { # sha256_of <arquivo>
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    printf ''
  fi
}

# ── destino ──────────────────────────────────────────────────────────────────
BIN_DIR_ROOT="/usr/local/bin"
BIN_DIR_ALTERNATIVO="/opt/arkame/bin"

# so_do_root <pasta>: a pasta e cada uma acima dela (as que existem) são do
# root e não aceitam escrita dos outros nem do grupo (salvo o grupo do root,
# gid 0). É o que o agente exige do programa do serviço do sistema
# (service.conferirPrograma): quem troca o arquivo vira root.
so_do_root() {
  d=$1
  while :; do
    if [ -e "$d" ]; then
      linha=$(ls -ldnL "$d" 2>/dev/null) || return 1
      perm=$(printf '%s\n' "$linha" | awk '{print $1}')
      dono=$(printf '%s\n' "$linha" | awk '{print $3}')
      grupo=$(printf '%s\n' "$linha" | awk '{print $4}')
      [ "$dono" = "0" ] || return 1
      case "$perm" in
        ????????w*) return 1 ;; # escrita dos outros
        ?????w*) [ "$grupo" = "0" ] || return 1 ;; # do grupo, só se for o do root
      esac
    fi
    [ "$d" = "/" ] && return 0
    d=$(dirname "$d")
  done
}

# bin_dir_de_root: onde instalar com root. O /usr/local/bin, se for só do
# root; senão o /opt/arkame/bin. No Mac Intel com Homebrew, o /usr/local/bin é
# do usuário, o agente recusava o programa ali, e o comando do painel (o
# mesmo `| sudo sh`) falhava sempre.
bin_dir_de_root() {
  if so_do_root "$BIN_DIR_ROOT"; then
    printf '%s' "$BIN_DIR_ROOT"
  else
    printf '%s' "$BIN_DIR_ALTERNATIVO"
  fi
}

# ── os agentes que rodam o programa ──────────────────────────────────────────
# Depois do mv, o processo de cada agente segue com o programa antigo (o mv
# troca o inode) até reiniciar. Os que rodam o programa são anotados antes da
# troca e reiniciados depois dela; sem isso, um segundo agente (arkame-agent-oci)
# ficava na versão antiga, sem aviso, até o próximo boot.
LAUNCHD_DIR_SISTEMA="/Library/LaunchDaemons"
LAUNCHD_DIR_USUARIO="${HOME:-/nenhum}/Library/LaunchAgents"

# caminho_real <arquivo>: o caminho com a pasta resolvida (links), para comparar.
caminho_real() {
  _cr_d=$(cd -P "$(dirname "$1")" 2>/dev/null && pwd) || { printf '%s' "$1"; return 0; }
  printf '%s/%s' "${_cr_d%/}" "$(basename "$1")"
}

# programa_do_execstart: o path= da saída de `systemctl show -p ExecStart`
# já sem o prefixo ExecStart= ({ path=/usr/local/bin/arkame-agent ; argv[]=… }),
# lida da entrada. Sem --value: ele só existe a partir do systemd 230, e no
# CentOS 7 (219) o show com --value falhava calado e nenhum agente era achado.
programa_do_execstart() {
  sed -n 's/^[[:space:]]*{[[:space:]]*path=\([^;]*[^;[:space:]]\)[[:space:]]*;.*/\1/p' | head -n 1
}

# programa_do_plist <arquivo>: o primeiro item do ProgramArguments.
programa_do_plist() {
  sed -n '/<key>ProgramArguments<\/key>/,/<\/array>/s/.*<string>\(.*\)<\/string>.*/\1/p' "$1" 2>/dev/null \
    | head -n 1 | sed 's/&lt;/</g; s/&gt;/>/g; s/&quot;/"/g; s/&apos;/'"'"'/g; s/&amp;/\&/g'
}

# label_launchd <nome>: arkame-agent-aws → app.arkame.agent-aws (como o agente).
label_launchd() {
  case "$1" in
    app.arkame.*) printf '%s' "$1" ;;
    *) printf 'app.arkame.%s' "${1#arkame-}" ;;
  esac
}

# alvo_launchd <escopo> <label>: system/<label> ou gui/<uid>/<label>.
alvo_launchd() {
  if [ "$1" = "system" ]; then printf 'system/%s' "$2"; else printf 'gui/%s/%s' "$(id -u)" "$2"; fi
}

# units_ativas [--user]: todas as units de serviço ativas, com o .service. Sem
# padrão de nome: até a v0.4.3 o install aceitava qualquer nome (backup-oci),
# e quem decide se é agente é o programa do ExecStart.
units_ativas() {
  systemctl "$@" list-units --type=service --state=active --no-legend --plain 2>/dev/null \
    | awk '{for (i = 1; i <= NF; i++) if ($i ~ /\.service$/) { print $i; break }}'
}

# agentes_do_programa <programa>: os agentes rodando agora que chamam
# <programa>, qualquer que seja o nome do serviço, um por linha: "<escopo> <nome>" (o nome da unit sem
# .service no systemd; o label no launchd).
agentes_do_programa() {
  _ap_alvo=$(caminho_real "$1")
  case "$OS" in
    linux)
      have systemctl || return 0
      for _ap_escopo in system user; do
        _ap_flag=""
        [ "$_ap_escopo" = "user" ] && _ap_flag="--user"
        for _ap_u in $(units_ativas $_ap_flag); do
          _ap_p=$(systemctl $_ap_flag show -p ExecStart "$_ap_u" 2>/dev/null | sed 's/^ExecStart=//' | programa_do_execstart)
          [ -n "$_ap_p" ] || continue
          [ "$(caminho_real "$_ap_p")" = "$_ap_alvo" ] || continue
          printf '%s %s\n' "$_ap_escopo" "${_ap_u%.service}"
        done
      done ;;
    darwin)
      have launchctl || return 0
      for _ap_escopo in system user; do
        if [ "$_ap_escopo" = "system" ]; then _ap_dir=$LAUNCHD_DIR_SISTEMA; else _ap_dir=$LAUNCHD_DIR_USUARIO; fi
        for _ap_f in "$_ap_dir"/app.arkame.*.plist; do
          [ -f "$_ap_f" ] || continue
          _ap_label=$(basename "$_ap_f" .plist)
          _ap_p=$(programa_do_plist "$_ap_f")
          [ -n "$_ap_p" ] || continue
          [ "$(caminho_real "$_ap_p")" = "$_ap_alvo" ] || continue
          launchctl print "$(alvo_launchd "$_ap_escopo" "$_ap_label")" 2>/dev/null | grep -q 'state = running' || continue
          printf '%s %s\n' "$_ap_escopo" "$_ap_label"
        done
      done ;;
  esac
  return 0
}

# reiniciar_agente <escopo> <nome>
reiniciar_agente() {
  case "$OS" in
    linux)
      if [ "$1" = "user" ]; then systemctl --user restart "$2"; else systemctl restart "$2"; fi ;;
    darwin)
      launchctl kickstart -k "$(alvo_launchd "$1" "$2")" >/dev/null ;;
    *) return 1 ;;
  esac
}

# reiniciar_agentes <lista> [<escopo> <nome> a pular]: reinicia cada
# "<escopo> <nome>" da lista (a de agentes_do_programa). Os que falham saem no
# aviso "Continuam na versão antiga".
reiniciar_agentes() {
  _ra_falharam=""
  while read -r _ra_escopo _ra_nome; do
    [ -n "$_ra_nome" ] || continue
    if [ "$_ra_escopo" = "${2:-}" ] && [ "$_ra_nome" = "${3:-}" ]; then
      continue
    fi
    if reiniciar_agente "$_ra_escopo" "$_ra_nome" </dev/null; then
      ok "Serviço $_ra_nome reiniciado com a versão nova"
    else
      warn "não consegui reiniciar o serviço $_ra_nome"
      _ra_falharam="$_ra_falharam${_ra_falharam:+, }$_ra_nome"
    fi
  done <<LISTA
$1
LISTA
  if [ -n "$_ra_falharam" ]; then
    warn "Continuam na versão antiga: $_ra_falharam"
    if [ "$OS" = "darwin" ]; then
      warn "  Reinicie-os (sudo launchctl kickstart -k system/<label>) para a versão nova valer."
    else
      warn "  Reinicie-os (sudo systemctl restart <serviço>, ou systemctl --user restart <serviço>) para a versão nova valer."
    fi
  fi
  return 0
}

# ── instalação ───────────────────────────────────────────────────────────────
main() {
  printf '\n%s\n\n' "${BOLD}Instalador do agente Arkame${RESET}"

  detect_platform
  VERSION=$(resolve_version)
  info "Versão:     $VERSION"
  info "Plataforma: $OS/$ARCH"

  # Onde instalar: com root vai para /usr/local/bin (ou /opt/arkame/bin, se
  # ele não for só do root); sem root, ~/.local/bin.
  if [ -n "${ARKAME_BIN_DIR:-}" ]; then
    BIN_DIR="$ARKAME_BIN_DIR"
  elif [ "$(id -u)" = "0" ]; then
    BIN_DIR=$(bin_dir_de_root)
    if [ "$BIN_DIR" != "$BIN_DIR_ROOT" ]; then
      info "$BIN_DIR_ROOT não é só do root (o do Homebrew, por exemplo): o serviço do"
      info "sistema roda como root e recusa programa que outro usuário pode trocar."
    fi
  else
    BIN_DIR="$HOME/.local/bin"
  fi
  # 0755: a pasta criada para o root não aceita escrita de mais ninguém.
  (umask 022 && mkdir -p "$BIN_DIR") || die "não consegui criar $BIN_DIR"
  info "Destino:    $BIN_DIR/arkame-agent"

  TMP=$(mktemp -d)
  # shellcheck disable=SC2064  # queremos expandir $TMP agora, não na saída
  trap "rm -rf '$TMP'" EXIT INT TERM

  archive="arkame-agent_${OS}_${ARCH}.tar.gz"
  if [ -n "$DOWNLOAD_BASE" ]; then
    base="${DOWNLOAD_BASE%/}"
  else
    base="https://github.com/$REPO/releases/download/$VERSION"
  fi

  printf '\n'
  info "Baixando $archive…"
  fetch "$base/$archive" "$TMP/$archive" || die "falha ao baixar $base/$archive"

  # Integridade: o checksums.txt vem do mesmo release e cobre todos os archives.
  # Sem conferir, para (como o setup do Windows): um proxy que bloqueasse só o
  # checksums.txt fazia o binário ser instalado sem conferência, com um aviso
  # que ninguém lê num curl | sh. Seguir sem conferir só com --skip-checksum.
  sem_conferir="Não instalei nada. Tente de novo; para instalar assim mesmo, sem conferir, repita com --skip-checksum."
  if [ "$SKIP_CHECKSUM" = "true" ]; then
    warn "--skip-checksum: instalando sem conferir o checksum de $archive"
  else
    fetch "$base/checksums.txt" "$TMP/checksums.txt" 2>/dev/null \
      || die "não consegui baixar $base/checksums.txt, e sem ele não dá para conferir o pacote.
     $sem_conferir"
    expected=$(grep " $archive\$" "$TMP/checksums.txt" | awk '{print $1}' | head -n 1)
    [ -n "$expected" ] || die "o checksums.txt não tem a linha de $archive.
     $sem_conferir"
    actual=$(sha256_of "$TMP/$archive")
    [ -n "$actual" ] || die "sem sha256sum nem shasum nesta máquina, não dá para conferir o checksum.
     $sem_conferir"
    if [ "$expected" != "$actual" ]; then
      die "checksum não confere para $archive.
     esperado: $expected
     obtido:   $actual
     Não instalei nada. Tente de novo; se persistir, avise contato@arkame.app."
    fi
    ok "Checksum conferido"
  fi

  tar -xzf "$TMP/$archive" -C "$TMP" || die "falha ao extrair $archive"
  [ -f "$TMP/arkame-agent" ] || die "o pacote não contém o binário arkame-agent"

  chmod +x "$TMP/arkame-agent"
  # O novo é copiado ao lado (o $TMP pode estar em outro sistema de arquivos)
  # e entra no lugar por mv, atômico na mesma pasta e sem "texto ocupado" com
  # o agente rodando. Se a cópia falhar (disco cheio), sai só o temporário e o
  # programa antigo fica: apagado antes, o serviço não subia mais no próximo
  # reinício.
  novo="$BIN_DIR/.arkame-agent.novo.$$"
  escrever="não consegui escrever em $BIN_DIR (tente com sudo, ou defina ARKAME_BIN_DIR); o programa que estava lá continua"
  if ! cp "$TMP/arkame-agent" "$novo"; then
    rm -f "$novo" 2>/dev/null || true
    die "$escrever"
  fi
  chmod 755 "$novo" 2>/dev/null || true
  # O novo vai ao disco antes do mv, e a pasta depois: em XFS (e em ext4 ou
  # btrfs, conforme a montagem) o rename pode chegar ao disco antes dos dados,
  # e uma queda de energia logo depois deixava o programa com 0 bytes e o
  # serviço sem subir. O sync com argumento é do coreutils 8.24+; nos
  # sistemas mais antigos (e no macOS), o sync sem argumento faz o mesmo.
  sync "$novo" 2>/dev/null || sync
  # Os agentes que rodam o programa agora: reiniciados depois da troca.
  ANTES=$(agentes_do_programa "$BIN_DIR/arkame-agent")
  if ! mv -f "$novo" "$BIN_DIR/arkame-agent"; then
    rm -f "$novo" 2>/dev/null || true
    die "$escrever"
  fi
  sync "$BIN_DIR" 2>/dev/null || sync
  ok "Instalado: $("$BIN_DIR"/arkame-agent version 2>/dev/null || echo "$BIN_DIR/arkame-agent")"

  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) warn "$BIN_DIR não está no seu PATH. Adicione: export PATH=\"\$PATH:$BIN_DIR\"" ;;
  esac

  if [ -z "$TOKEN" ]; then
    # Atualização de um servidor já instalado: todos os agentes do programa,
    # inclusive o principal, reiniciam com a versão nova, e não há o que
    # registrar.
    if [ -n "$ANTES" ]; then
      printf '\n'
      reiniciar_agentes "$ANTES"
      printf '\n'
      info "Atualização concluída: este servidor já está registrado no painel."
      printf '\n'
      return 0
    fi
    printf '\n'
    info "Próximo passo — registre este servidor no painel:"
    printf '\n'
    case "$BIN_DIR/" in
      "${HOME:-/nenhum}"/*)
        # Programa no home: o serviço do sistema roda como root, e qualquer
        # processo deste usuário trocaria o arquivo e viraria root no próximo
        # reinício (o agente recusa). Sem sudo, o serviço é só deste usuário.
        info "  ${BOLD}$BIN_DIR/arkame-agent install --token=SEU_CODIGO${RESET}"
        printf '\n'
        info "Sem sudo: o agente roda como o seu usuário e só lê o que você lê."
        info "Para o servidor inteiro (como root), instale de novo com sudo, que põe o"
        info "programa em /usr/local/bin:"
        printf '\n'
        info "  ${BOLD}curl -fsSL https://get.arkame.app/install.sh | sudo sh -s -- --token=SEU_CODIGO${RESET}"
        ;;
      *)
        # Caminho completo: o sudo do RHEL/Fedora não procura em /usr/local/bin.
        info "  ${BOLD}sudo $BIN_DIR/arkame-agent install --token=SEU_CODIGO${RESET}"
        ;;
    esac
    printf '\n'
    info "O código aparece em $PANEL_URL/agents/new."
    printf '\n'
    return 0
  fi

  # Os outros agentes do programa reiniciam já; o do --service-name, o
  # install reinicia ao instalar o serviço (com --no-service, não: entra junto).
  proprio_escopo=$SERVICE_SCOPE
  if [ -z "$proprio_escopo" ]; then
    if [ "$(id -u)" = "0" ]; then proprio_escopo="system"; else proprio_escopo="user"; fi
  fi
  proprio_nome=""
  if [ "$INSTALL_SERVICE" = "true" ]; then
    proprio_nome=$SERVICE_NAME
    if [ "$OS" = "darwin" ]; then proprio_nome=$(label_launchd "$SERVICE_NAME"); fi
  fi
  if [ -n "$ANTES" ]; then
    printf '\n'
    reiniciar_agentes "$ANTES" "$proprio_escopo" "$proprio_nome"
  fi

  # O agente pergunta a chave do bucket, testa e só então registra.
  printf '\n'
  set -- install --token="$TOKEN" --panel-url="$PANEL_URL" --service-name="$SERVICE_NAME"
  [ -n "$SERVICE_SCOPE" ] && set -- "$@" --service-scope="$SERVICE_SCOPE"
  [ -n "$CONFIG_FILE" ] && set -- "$@" --config="$CONFIG_FILE"
  [ "$INSTALL_SERVICE" = "false" ] && set -- "$@" --install-service=false

  status=0
  "$BIN_DIR/arkame-agent" "$@" || status=$?
  # O install que não termina (chave errada, código vencido) não chega a
  # reiniciar o serviço: ele seguiria no programa antigo.
  if [ "$status" -ne 0 ] && [ -n "$proprio_nome" ] &&
     printf '%s\n' "$ANTES" | grep -qxF "$proprio_escopo $proprio_nome"; then
    printf '\n'
    warn "A instalação não terminou, mas o programa já foi trocado."
    reiniciar_agentes "$proprio_escopo $proprio_nome"
  fi
  return "$status"
}

# ARKAME_INSTALL_SEM_MAIN: só define as funções (para os testes as chamarem).
[ -n "${ARKAME_INSTALL_SEM_MAIN:-}" ] || main "$@"
