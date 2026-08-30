#!/bin/sh
# Instalador do agent Arkame para Linux e macOS.
#
#   curl -fsSL https://get.arkame.app/install.sh | sh
#   curl -fsSL https://get.arkame.app/install.sh | sh -s -- --token=atk_...
#
# Sem --token, apenas instala o binário; o cadastro no painel fica para depois
# (`arkame-agent install --token=...`). Com --token, já registra o servidor e
# deixa o agent rodando como serviço.
#
# Variáveis reconhecidas:
#   ARKAME_VERSION        versão a instalar (padrão: a mais recente)
#   ARKAME_BIN_DIR        onde instalar (padrão: /usr/local/bin, ou ~/.local/bin sem root)
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
INSTALL_SERVICE="true"

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
Instalador do agent Arkame.

Uso:
  curl -fsSL https://get.arkame.app/install.sh | sh
  curl -fsSL https://get.arkame.app/install.sh | sh -s -- --token=atk_xxx

Opções:
  --token=TOKEN         token de instalação gerado no painel (em Agentes → Novo)
  --panel-url=URL       painel a usar (padrão: https://save.arkame.app)
  --service-name=NOME   nome do serviço (use um por credencial de storage)
  --service-scope=X     system (todo o host, exige sudo) ou user (sem sudo)
  --no-service          só instala o binário, sem registrar serviço
  --version=vX.Y.Z      instala uma versão específica
  --download-base=URL   espelho de onde baixar (exige --version)
  --help                mostra esta ajuda
USAGE
}

for arg in "$@"; do
  case "$arg" in
    --token=*)         TOKEN="${arg#*=}" ;;
    --panel-url=*)     PANEL_URL="${arg#*=}" ;;
    --service-name=*)  SERVICE_NAME="${arg#*=}" ;;
    --service-scope=*) SERVICE_SCOPE="${arg#*=}" ;;
    --version=*)       ARKAME_VERSION="${arg#*=}" ;;
    --download-base=*) DOWNLOAD_BASE="${arg#*=}" ;;
    --no-service)      INSTALL_SERVICE="false" ;;
    --help|-h)         usage; exit 0 ;;
    *) die "opção desconhecida: $arg (use --help)" ;;
  esac
done

# ── plataforma ───────────────────────────────────────────────────────────────
detect_platform() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)

  case "$os" in
    linux|darwin) ;;
    mingw*|msys*|cygwin*)
      die "no Windows, baixe o .zip em https://github.com/$REPO/releases e rode: arkame-agent.exe install --token=..." ;;
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

# ── instalação ───────────────────────────────────────────────────────────────
main() {
  printf '\n%s\n\n' "${BOLD}Instalador do agent Arkame${RESET}"

  detect_platform
  VERSION=$(resolve_version)
  info "Versão:     $VERSION"
  info "Plataforma: $OS/$ARCH"

  # Onde instalar: com root vai para /usr/local/bin; sem root, ~/.local/bin.
  if [ -n "${ARKAME_BIN_DIR:-}" ]; then
    BIN_DIR="$ARKAME_BIN_DIR"
  elif [ "$(id -u)" = "0" ]; then
    BIN_DIR="/usr/local/bin"
  else
    BIN_DIR="$HOME/.local/bin"
  fi
  mkdir -p "$BIN_DIR" || die "não consegui criar $BIN_DIR"
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
  if fetch "$base/checksums.txt" "$TMP/checksums.txt" 2>/dev/null; then
    expected=$(grep " $archive\$" "$TMP/checksums.txt" | awk '{print $1}' | head -n 1)
    actual=$(sha256_of "$TMP/$archive")
    if [ -z "$actual" ]; then
      warn "sem sha256sum/shasum nesta máquina — não deu para conferir o checksum"
    elif [ -z "$expected" ]; then
      warn "checksum de $archive não está no checksums.txt — seguindo sem conferir"
    elif [ "$expected" != "$actual" ]; then
      die "checksum não confere para $archive.
     esperado: $expected
     obtido:   $actual
     Não instalei nada. Tente de novo; se persistir, avise suporte@arkame.app."
    else
      ok "Checksum conferido"
    fi
  else
    warn "não consegui baixar checksums.txt — seguindo sem conferir a integridade"
  fi

  tar -xzf "$TMP/$archive" -C "$TMP" || die "falha ao extrair $archive"
  [ -f "$TMP/arkame-agent" ] || die "o pacote não contém o binário arkame-agent"

  chmod +x "$TMP/arkame-agent"
  # mv entre sistemas de arquivos diferentes falha; cat preserva o destino se
  # o binário estiver em uso (texto ocupado), então removemos antes.
  rm -f "$BIN_DIR/arkame-agent" 2>/dev/null || true
  cp "$TMP/arkame-agent" "$BIN_DIR/arkame-agent" || die "não consegui escrever em $BIN_DIR (tente com sudo, ou defina ARKAME_BIN_DIR)"
  ok "Instalado: $("$BIN_DIR"/arkame-agent version 2>/dev/null || echo "$BIN_DIR/arkame-agent")"

  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) warn "$BIN_DIR não está no seu PATH. Adicione: export PATH=\"\$PATH:$BIN_DIR\"" ;;
  esac

  if [ -z "$TOKEN" ]; then
    printf '\n'
    info "Próximo passo — registre este servidor no painel:"
    printf '\n'
    info "  ${BOLD}arkame-agent install --token=SEU_TOKEN${RESET}"
    printf '\n'
    info "O token aparece em $PANEL_URL/agents/new."
    printf '\n'
    return 0
  fi

  printf '\n'
  info "Registrando este servidor no painel…"
  set -- install --token="$TOKEN" --panel-url="$PANEL_URL" --service-name="$SERVICE_NAME"
  [ -n "$SERVICE_SCOPE" ] && set -- "$@" --service-scope="$SERVICE_SCOPE"
  [ "$INSTALL_SERVICE" = "false" ] && set -- "$@" --install-service=false

  "$BIN_DIR/arkame-agent" "$@"
}

main "$@"
