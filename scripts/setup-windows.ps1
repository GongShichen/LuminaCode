[CmdletBinding()]
param(
    [string]$AppRoot = "",
    [string]$ApiKey = $(if ($env:LUMINA_API_KEY) { $env:LUMINA_API_KEY } elseif ($env:LLM_API_KEY) { $env:LLM_API_KEY } else { "" }),
    [string]$BaseUrl = $(if ($env:LUMINA_API_BASE_URL) { $env:LUMINA_API_BASE_URL } elseif ($env:LLM_BASE_URL) { $env:LLM_BASE_URL } else { "" }),
    [string]$Model = $(if ($env:LUMINA_API_MODEL) { $env:LUMINA_API_MODEL } elseif ($env:LLM_DEFAULT_MODEL) { $env:LLM_DEFAULT_MODEL } else { "" }),
    [ValidateSet("openai_compatible", "anthropic", "auto")]
    [string]$ApiType = $(if ($env:LUMINA_API_TYPE) { $env:LUMINA_API_TYPE } elseif ($env:LLM_API_TYPE) { $env:LLM_API_TYPE } else { "openai_compatible" }),
    [int]$MaxTokens = $(if ($env:LUMINA_API_MAX_TOKENS) { [int]$env:LUMINA_API_MAX_TOKENS } else { 1000000 }),
    [switch]$ConfigureApi,
    [switch]$WriteDefaults,
    [switch]$SkipNpmInstall,
    [switch]$SkipTests,
    [switch]$RunSmokePrompt,
    [string]$SmokePrompt = "ping"
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")
$paths = Get-LuminaPaths -AppRoot $AppRoot
$AppRoot = $paths.Root

function Assert-Command {
    param([Parameter(Mandatory = $true)][string]$Name)

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' was not found in PATH."
    }
}

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)][scriptblock]$Command
    )

    Write-Host "==> $Label"
    & $Command
    if ($LASTEXITCODE -ne 0) {
        throw "$Label failed with exit code $LASTEXITCODE."
    }
}

function Convert-SecureStringToPlainText {
    param([Parameter(Mandatory = $true)][securestring]$Value)

    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Value)
    try {
        [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    } finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    }
}

function Read-OptionalValue {
    param(
        [Parameter(Mandatory = $true)][string]$Prompt,
        [string]$CurrentValue = ""
    )

    if ($CurrentValue) {
        $answer = Read-Host "$Prompt [$CurrentValue]"
        if ([string]::IsNullOrWhiteSpace($answer)) {
            return $CurrentValue
        }
        return $answer.Trim()
    }

    return (Read-Host $Prompt).Trim()
}

function Write-LuminaDefaults {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [string]$ApiKey,
        [string]$BaseUrl,
        [string]$Model,
        [string]$ApiType,
        [int]$MaxTokens
    )

    $dir = Split-Path -Parent $Path
    New-Item -ItemType Directory -Path $dir -Force | Out-Null

    if (Test-Path $Path) {
        $timestamp = Get-Date -Format "yyyyMMdd-HHmmss"
        Copy-Item -LiteralPath $Path -Destination "$Path.bak.$timestamp" -Force
    }

    $config = Read-LuminaJsonHashtable -Path $Path
    if ($ApiKey) { $config["api_key"] = $ApiKey }
    if ($BaseUrl) { $config["api_base_url"] = $BaseUrl }
    if ($Model) { $config["api_model"] = $Model }
    $config["api_type"] = $ApiType
    $config["api_max_tokens"] = $MaxTokens
    Write-LuminaAtomicJson -Path $Path -Value $config
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$tmpDir = Join-Path $repoRoot "tmp"
$backendPath = Join-Path $tmpDir "lumina-backend.exe"
$launcherPath = Join-Path $tmpDir "lumina.cmd"
$frontendDist = Join-Path $repoRoot "frontend\dist\index.js"
$defaultsPath = $paths.Settings

Write-Host "LuminaCode Windows setup"
Write-Host "Repo: $repoRoot"

Assert-Command go
Assert-Command node
Assert-Command npm

if ($ConfigureApi) {
    if (-not $ApiKey) {
        $secureKey = Read-Host "API key" -AsSecureString
        $ApiKey = Convert-SecureStringToPlainText $secureKey
    }
    $BaseUrl = Read-OptionalValue -Prompt "API base URL" -CurrentValue $BaseUrl
    $Model = Read-OptionalValue -Prompt "API model" -CurrentValue $Model
    $ApiType = Read-OptionalValue -Prompt "API type (openai_compatible, anthropic, auto)" -CurrentValue $ApiType
    if ($ApiType -notin @("openai_compatible", "anthropic", "auto")) {
        throw "Invalid API type '$ApiType'."
    }
    $WriteDefaults = $true
}

New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

Push-Location $repoRoot
try {
    Invoke-Native "go version" { & go version }
    Invoke-Native "node version" { & node --version }
    Invoke-Native "npm version" { & npm --version }

    if (-not $SkipNpmInstall) {
        Invoke-Native "install frontend dependencies" { & npm --prefix frontend install }
    }

    Invoke-Native "build frontend" { & npm --prefix frontend run build }

    if (-not (Test-Path $frontendDist)) {
        throw "Frontend build output was not created: $frontendDist"
    }

    Invoke-Native "build Go backend" { & go build -o $backendPath . }

    $launcher = @'
@echo off
setlocal
set "REPO=%~dp0.."
set "LUMINA_BACKEND_BIN=%~dp0lumina-backend.exe"
set "LUMINA_RESOURCE_ROOT=%REPO%"
if not defined LUMINA_API_KEY if defined LLM_API_KEY set "LUMINA_API_KEY=%LLM_API_KEY%"
if not defined LUMINA_API_BASE_URL if defined LLM_BASE_URL set "LUMINA_API_BASE_URL=%LLM_BASE_URL%"
if not defined LUMINA_API_MODEL if defined LLM_DEFAULT_MODEL set "LUMINA_API_MODEL=%LLM_DEFAULT_MODEL%"
if not defined LUMINA_API_TYPE if defined LLM_API_TYPE set "LUMINA_API_TYPE=%LLM_API_TYPE%"
if not defined LUMINA_API_TYPE set "LUMINA_API_TYPE=openai_compatible"
node "%REPO%\frontend\dist\index.js" %*
'@
    Set-Content -LiteralPath $launcherPath -Value $launcher -Encoding ASCII

    if ($WriteDefaults) {
        Write-LuminaDefaults `
            -Path $defaultsPath `
            -ApiKey $ApiKey `
            -BaseUrl $BaseUrl `
            -Model $Model `
            -ApiType $ApiType `
            -MaxTokens $MaxTokens
        Write-Host "Wrote defaults: $defaultsPath"
    }

    if (-not $SkipTests) {
        Invoke-Native "frontend typecheck" { & npm --prefix frontend test }
    }

    Invoke-Native "launcher help" { & $launcherPath --help }

    if ($RunSmokePrompt) {
        Invoke-Native "smoke prompt" { & $launcherPath -p $SmokePrompt }
    }
} finally {
    Pop-Location
}

Write-Host ""
Write-Host "Done."
Write-Host "Try: $launcherPath --help"
Write-Host "Run: $launcherPath -p `"Summarize this repository`""
if (-not $WriteDefaults) {
    Write-Host "API config was not written. Re-run with -ConfigureApi or set LUMINA_API_* env vars before running prompts."
}
