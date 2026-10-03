<#
.SYNOPSIS
  Instalador do agent Arkame para Windows.

.DESCRIPTION
  Baixa o binário da versão mais recente, confere o checksum SHA-256 e instala
  em C:\Program Files\Arkame. Com o código de instalação do painel, também
  pergunta a chave do bucket, testa, registra o servidor e cria o serviço.

.EXAMPLE
  # Windows + R, cole e Enter (o comando que o painel mostra):
  powershell -ExecutionPolicy Bypass -Command "&([scriptblock]::Create((irm https://get.arkame.app/install.ps1))) -Token atk_xxx"

.EXAMPLE
  $env:ARKAME_TOKEN = 'atk_xxx'
  irm https://get.arkame.app/install.ps1 | iex

.NOTES
  O painel não indica mais este script (28/09): `powershell -ExecutionPolicy
  Bypass … irm` foi barrado pelo Defender como Trojan:Win32/Commando.A!ml. O
  comando de Windows agora é `cmd /c "curl … get.arkame.app/agente.exe && …
  setup --token=…"`, sem PowerShell. Este fica para quem já o usa.

  Instalar o serviço exige administrador. Quem roda sem ser administrador vê o
  pedido de permissão do Windows e o instalador continua numa janela nova, já
  elevada — ninguém precisa saber abrir o PowerShell como administrador.

  O comando da primeira linha não usa `$`: colado no Executar, no Prompt de
  Comando ou no PowerShell, chega igual ao instalador.
#>

[CmdletBinding()]
param(
    [string]$Token       = $env:ARKAME_TOKEN,
    [string]$PanelUrl    = $(if ($env:ARKAME_PANEL_URL) { $env:ARKAME_PANEL_URL } else { 'https://save.arkame.app' }),
    [string]$Version     = $env:ARKAME_VERSION,
    [string]$ServiceName = 'arkame-agent',
    # Arquivo de configuração deste agente. Com mais de um agente na máquina
    # (um por credencial de bucket), cada um com o seu, junto de -ServiceName.
    [string]$Config      = '',
    [switch]$NoService,
    # Onde este script mora, para se reabrir como administrador.
    [string]$ScriptUrl   = $(if ($env:ARKAME_SCRIPT_URL) { $env:ARKAME_SCRIPT_URL } else { 'https://get.arkame.app/install.ps1' }),
    # Marca a janela reaberta como administrador: ela espera um Enter no fim,
    # senão fecha antes de a pessoa ler o resultado.
    [switch]$Elevated
)

$ErrorActionPreference = 'Stop'
$Repo = 'arkame-app/arkame-agent'

function Write-Info { param([string]$Message) Write-Host "  $Message" }
function Write-Ok   { param([string]$Message) Write-Host "  [ok] $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "  [!] $Message"  -ForegroundColor Yellow }
function Wait-ToClose {
    if ($Elevated) {
        Write-Host ""
        Read-Host "  Pressione Enter para fechar" | Out-Null
    }
}
function Stop-WithError {
    param([string]$Message)
    Write-Host "  [x] $Message" -ForegroundColor Red
    Wait-ToClose
    exit 1
}

function Test-Administrator {
    $identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

Write-Host ""
Write-Host "Instalador do agente Arkame" -ForegroundColor Cyan
Write-Host ""

# O que vai para a linha de comando da janela elevada precisa ter a forma
# esperada: nada de aspas nem espaços que mudem o comando.
if ($Token -and $Token -notmatch '^atk_[A-Za-z0-9_-]{16,}$') {
    Stop-WithError "codigo de instalacao invalido. Copie de novo o comando do painel."
}
if ($PanelUrl -notmatch '^https?://[A-Za-z0-9.:/_-]+$') {
    Stop-WithError "endereco do painel invalido: $PanelUrl"
}
if ($Version -and $Version -notmatch '^v[0-9A-Za-z.+-]+$') {
    Stop-WithError "versao invalida: $Version"
}

# Sem administrador: reabre este instalador elevado. O Windows mostra o pedido
# de permissão; a janela nova baixa o script de novo e segue daqui.
if (-not (Test-Administrator)) {
    if ($ScriptUrl -notmatch '^https://[A-Za-z0-9.:/_-]+$') {
        Stop-WithError "endereco do instalador invalido: $ScriptUrl"
    }
    $partes = @("-ScriptUrl '$ScriptUrl'", "-PanelUrl '$PanelUrl'", "-ServiceName '$ServiceName'", '-Elevated')
    if ($Token)     { $partes += "-Token '$Token'" }
    if ($Config)    {
        # Vai entre aspas simples no comando da janela elevada.
        if ($Config -match "['\r\n]") { Stop-WithError "caminho de configuracao invalido: $Config" }
        $partes += "-Config '$Config'"
    }
    if ($Version)   { $partes += "-Version '$Version'" }
    if ($NoService) { $partes += '-NoService' }
    $comando = "&([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing '$ScriptUrl'))) " + ($partes -join ' ')
    $codificado = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($comando))
    Write-Info "O Windows vai pedir permissao de administrador: clique em Sim."
    Write-Info "A instalacao continua na janela que abrir."
    try {
        Start-Process -FilePath 'powershell.exe' -Verb RunAs -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', $codificado)
    } catch {
        Stop-WithError "sem permissao de administrador, nao da para instalar o servico. Rode de novo e clique em Sim."
    }
    exit 0
}

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

    # Outro serviço (um segundo agente, -ServiceName arkame-agent-oci) pode
    # estar rodando o mesmo exe: o Windows não deixa sobrescrever um exe em
    # uso, mas deixa renomear. O atual sai do caminho (.old) e o novo entra
    # no lugar, como o setup do agente faz. Um .old ainda em uso fica para
    # uma próxima instalação.
    Get-ChildItem -Path $installDir -Filter 'arkame-agent.exe.old*' -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    $oldPath = $null
    if (Test-Path -LiteralPath $exePath) {
        $oldPath = "$exePath.old"
        if (Test-Path -LiteralPath $oldPath) {
            # O .old anterior não saiu (em uso): outro nome.
            $oldPath = "$exePath.old-" + [Guid]::NewGuid().ToString('N')
        }
        Move-Item -LiteralPath $exePath -Destination $oldPath -Force
    }
    try {
        Copy-Item -LiteralPath $extracted -Destination $exePath -Force
    } catch {
        # Devolve o que estava lá: sem exe, os serviços não sobem mais.
        if ($oldPath) {
            Move-Item -LiteralPath $oldPath -Destination $exePath -Force -ErrorAction SilentlyContinue
        }
        throw
    }
    if ($oldPath) {
        Remove-Item -LiteralPath $oldPath -Force -ErrorAction SilentlyContinue
    }
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
        Write-Info "  & '$exePath' install --token=SEU_CODIGO"
        Write-Host ""
        Write-Info "O codigo aparece em $PanelUrl/agents/new."
        Write-Host ""
        Wait-ToClose
        exit 0
    }

    # O agente pergunta a chave do bucket, testa e só então registra.
    Write-Host ""
    $agentArgs = @('install', "--token=$Token", "--panel-url=$PanelUrl", "--service-name=$ServiceName")
    if ($Config)    { $agentArgs += "--config=$Config" }
    if ($NoService) { $agentArgs += '--install-service=false' }

    & $exePath @agentArgs
    if ($LASTEXITCODE -ne 0) {
        Stop-WithError "a instalacao nao terminou (codigo $LASTEXITCODE). Veja a mensagem acima."
    }
    Write-Host ""
    Write-Ok "Pronto. O painel mostra o servidor e o teste do bucket."
    Wait-ToClose
} catch {
    # Download que falha, zip corrompido: a janela elevada não pode fechar
    # antes de a pessoa ler o motivo.
    Stop-WithError $_.Exception.Message
} finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
