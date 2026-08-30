<#
.SYNOPSIS
  Instalador do agent Arkame para Windows.

.DESCRIPTION
  Baixa o binário da versão mais recente, confere o checksum SHA-256 e instala
  em C:\Program Files\Arkame. Com um token, também registra o servidor no
  painel e cria o serviço do Windows.

.EXAMPLE
  irm https://get.arkame.app/install.ps1 | iex

.EXAMPLE
  $env:ARKAME_TOKEN = 'atk_xxx'
  irm https://get.arkame.app/install.ps1 | iex

.NOTES
  Registrar o serviço exige PowerShell como Administrador — no Windows não há
  equivalente ao serviço por usuário do Linux.
#>

[CmdletBinding()]
param(
    [string]$Token       = $env:ARKAME_TOKEN,
    [string]$PanelUrl    = $(if ($env:ARKAME_PANEL_URL) { $env:ARKAME_PANEL_URL } else { 'https://save.arkame.app' }),
    [string]$Version     = $env:ARKAME_VERSION,
    [string]$ServiceName = 'arkame-agent',
    [switch]$NoService
)

$ErrorActionPreference = 'Stop'
$Repo = 'arkame-app/arkame-agent'

function Write-Info { param([string]$Message) Write-Host "  $Message" }
function Write-Ok   { param([string]$Message) Write-Host "  [ok] $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "  [!] $Message"  -ForegroundColor Yellow }
function Stop-WithError {
    param([string]$Message)
    Write-Host "  [x] $Message" -ForegroundColor Red
    exit 1
}

function Test-Administrator {
    $identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

Write-Host ""
Write-Host "Instalador do agent Arkame" -ForegroundColor Cyan
Write-Host ""

# TLS 1.2 para o Windows Server 2016/2019, onde não é o padrão.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ── plataforma ───────────────────────────────────────────────────────────────
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Stop-WithError "arquitetura não suportada: $($env:PROCESSOR_ARCHITECTURE) (suportadas: AMD64, ARM64)" }
}

# ── versão ───────────────────────────────────────────────────────────────────
if (-not $Version) {
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing
        $Version = $release.tag_name
    } catch {
        Stop-WithError "não consegui descobrir a versão mais recente: $($_.Exception.Message). Informe uma com -Version vX.Y.Z"
    }
}

Write-Info "Versao:     $Version"
Write-Info "Plataforma: windows/$arch"

$installDir = Join-Path $env:ProgramFiles 'Arkame'
$exePath    = Join-Path $installDir 'arkame-agent.exe'
Write-Info "Destino:    $exePath"

if (-not (Test-Administrator)) {
    Stop-WithError "abra o PowerShell como Administrador: instalar em '$installDir' e criar o servico exigem privilegio elevado."
}

# ── download ─────────────────────────────────────────────────────────────────
$archive = "arkame-agent_windows_$arch.zip"
$base    = "https://github.com/$Repo/releases/download/$Version"
$tmp     = Join-Path ([IO.Path]::GetTempPath()) ("arkame-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    Write-Host ""
    Write-Info "Baixando $archive..."
    $zipPath = Join-Path $tmp $archive
    Invoke-WebRequest -Uri "$base/$archive" -OutFile $zipPath -UseBasicParsing

    # Integridade: checksums.txt cobre todos os pacotes do mesmo release.
    try {
        $sumsPath = Join-Path $tmp 'checksums.txt'
        Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sumsPath -UseBasicParsing

        $expected = (Get-Content $sumsPath |
            Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } |
            Select-Object -First 1) -split '\s+' | Select-Object -First 1
        $actual = (Get-FileHash -Path $zipPath -Algorithm SHA256).Hash.ToLower()

        if (-not $expected) {
            Write-Warn "checksum de $archive nao esta no checksums.txt - seguindo sem conferir"
        } elseif ($expected.ToLower() -ne $actual) {
            Stop-WithError "checksum nao confere para $archive.`n     esperado: $expected`n     obtido:   $actual`n     Nao instalei nada."
        } else {
            Write-Ok "Checksum conferido"
        }
    } catch {
        Write-Warn "nao consegui baixar checksums.txt - seguindo sem conferir a integridade"
    }

    Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
    $extracted = Join-Path $tmp 'arkame-agent.exe'
    if (-not (Test-Path $extracted)) {
        Stop-WithError "o pacote nao contem arkame-agent.exe"
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null

    # Um binário em uso não pode ser sobrescrito: paramos o serviço antes.
    $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($existing -and $existing.Status -eq 'Running') {
        Write-Info "Parando o servico $ServiceName para atualizar o binario..."
        Stop-Service -Name $ServiceName -Force
        Start-Sleep -Seconds 2
    }

    Copy-Item -Path $extracted -Destination $exePath -Force
    Write-Ok "Instalado: $exePath"

    # PATH da máquina, para o comando ficar disponível em novos terminais.
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if ($machinePath -notlike "*$installDir*") {
        [Environment]::SetEnvironmentVariable('Path', "$machinePath;$installDir", 'Machine')
        Write-Info "Adicionado ao PATH (abra um novo terminal para usar 'arkame-agent')"
    }

    if (-not $Token) {
        Write-Host ""
        Write-Info "Proximo passo - registre este servidor no painel:"
        Write-Host ""
        Write-Info "  & '$exePath' install --token=SEU_TOKEN"
        Write-Host ""
        Write-Info "O token aparece em $PanelUrl/agents/new."
        Write-Host ""
        exit 0
    }

    Write-Host ""
    Write-Info "Registrando este servidor no painel..."
    $agentArgs = @('install', "--token=$Token", "--panel-url=$PanelUrl", "--service-name=$ServiceName")
    if ($NoService) { $agentArgs += '--install-service=false' }

    & $exePath @agentArgs
    if ($LASTEXITCODE -ne 0) {
        Stop-WithError "o registro falhou (codigo $LASTEXITCODE). Rode novamente com o token do painel."
    }
} finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
