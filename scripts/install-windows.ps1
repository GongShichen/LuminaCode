[CmdletBinding()]
param(
    [string]$InstallDir = $(if ($env:LUMINA_INSTALL_DIR) { $env:LUMINA_INSTALL_DIR } elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "LuminaCode\bin" } else { Join-Path $HOME ".local\bin" }),
    [string]$AppRoot = $(if ($env:LUMINA_APP_ROOT) { $env:LUMINA_APP_ROOT } else { Join-Path $HOME ".lumina" }),
    [string]$ApiKey = $(if ($env:LUMINA_API_KEY) { $env:LUMINA_API_KEY } elseif ($env:LLM_API_KEY) { $env:LLM_API_KEY } else { "" }),
    [string]$BaseUrl = $(if ($env:LUMINA_API_BASE_URL) { $env:LUMINA_API_BASE_URL } elseif ($env:LLM_BASE_URL) { $env:LLM_BASE_URL } else { "" }),
    [string]$Model = $(if ($env:LUMINA_API_MODEL) { $env:LUMINA_API_MODEL } elseif ($env:LLM_DEFAULT_MODEL) { $env:LLM_DEFAULT_MODEL } else { "" }),
    [ValidateSet("openai_compatible", "anthropic", "auto")]
    [string]$ApiType = $(if ($env:LUMINA_API_TYPE) { $env:LUMINA_API_TYPE } elseif ($env:LLM_API_TYPE) { $env:LLM_API_TYPE } else { "openai_compatible" }),
    [int]$MaxTokens = $(if ($env:LUMINA_API_MAX_TOKENS) { [int]$env:LUMINA_API_MAX_TOKENS } else { 1000000 }),
    [switch]$ConfigureApi,
    [switch]$WriteDefaults,
    [switch]$SkipNpmInstall,
    [switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"

if ($env:OS -and $env:OS -ne "Windows_NT") {
    throw "install-windows.ps1 must be run on Windows."
}

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

function Copy-DirectoryContents {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination
    )
    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    Get-ChildItem -LiteralPath $Source -Force | ForEach-Object {
        Copy-Item -LiteralPath $_.FullName -Destination $Destination -Recurse -Force
    }
}

function Normalize-PathSegment {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) {
        return ""
    }
    try {
        return [IO.Path]::GetFullPath($Path).TrimEnd([char[]]@('\', '/')).ToLowerInvariant()
    } catch {
        return $Path.TrimEnd([char[]]@('\', '/')).ToLowerInvariant()
    }
}

function Test-PathListContains {
    param(
        [string]$PathList,
        [string]$Path
    )
    $target = Normalize-PathSegment $Path
    foreach ($segment in ($PathList -split ';')) {
        if ((Normalize-PathSegment $segment) -eq $target) {
            return $true
        }
    }
    return $false
}

function Add-UserPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathListContains -PathList $current -Path $Path) {
        if (-not (Test-PathListContains -PathList $env:Path -Path $Path)) {
            $env:Path = "$Path;$env:Path"
        }
        return $false
    }
    if ([string]::IsNullOrWhiteSpace($current)) {
        [Environment]::SetEnvironmentVariable("Path", $Path, "User")
    } else {
        [Environment]::SetEnvironmentVariable("Path", "$current;$Path", "User")
    }
    if (-not (Test-PathListContains -PathList $env:Path -Path $Path)) {
        $env:Path = "$Path;$env:Path"
    }
    return $true
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
    $config = [ordered]@{
        api_key = $ApiKey
        api_base_url = $BaseUrl
        api_model = $Model
        api_type = $ApiType
        api_max_tokens = $MaxTokens
        api_stream_idle_timeout_seconds = 180.0
        web_search_enabled = $true
        web_search_provider = "searxng"
        web_search_base_url = "http://127.0.0.1:8888"
        web_search_max_results = 10
        web_search_timeout_seconds = 20.0
        web_fetch_enabled = $true
        web_fetch_require_search_result = $true
        web_fetch_max_chars = 80000
        web_fetch_timeout_seconds = 20.0
        web_fetch_user_agent = "LuminaCode/1.0"
        session_dir = "~/.Lumina/sessions"
        session_memory_enabled = $true
        auto_memory_enabled = $true
        skills_enabled = $true
        bundled_skills_dir = ".Lumina/SKILLS"
        system_prompt_path = ".Lumina/SYSTEM/system-prompt.md"
        memory_extraction_prompt_path = ".Lumina/SYSTEM/extraction_system.md"
        worktree_base_ref = "HEAD"
        worktree_dir = ".Lumina/worktrees"
    }
    $config | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $Path -Encoding UTF8
}

function Merge-WebDefaults {
    param([Parameter(Mandatory = $true)][string]$Path)
    New-Item -ItemType Directory -Path (Split-Path -Parent $Path) -Force | Out-Null
    if (Test-Path $Path) {
        $data = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable
    } else {
        $data = @{}
    }
    $defaults = [ordered]@{
        api_stream_idle_timeout_seconds = 180.0
        web_search_enabled = $true
        web_search_provider = "searxng"
        web_search_base_url = "http://127.0.0.1:8888"
        web_search_max_results = 10
        web_search_timeout_seconds = 20.0
        web_fetch_enabled = $true
        web_fetch_require_search_result = $true
        web_fetch_max_chars = 80000
        web_fetch_timeout_seconds = 20.0
        web_fetch_user_agent = "LuminaCode/1.0"
    }
    foreach ($key in $defaults.Keys) {
        if (-not $data.ContainsKey($key)) {
            $data[$key] = $defaults[$key]
        }
    }
    $data | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $Path -Encoding UTF8
}

function Write-LuminaLauncher {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$AppRoot
    )
    $launcher = @"
@echo off
setlocal
set "LUMINA_BACKEND_BIN=%~dp0lumina-backend.exe"
if not defined LUMINA_RESOURCE_ROOT set "LUMINA_RESOURCE_ROOT=$AppRoot"
if not defined LUMINA_API_KEY if defined LLM_API_KEY set "LUMINA_API_KEY=%LLM_API_KEY%"
if not defined LUMINA_API_BASE_URL if defined LLM_BASE_URL set "LUMINA_API_BASE_URL=%LLM_BASE_URL%"
if not defined LUMINA_API_MODEL if defined LLM_DEFAULT_MODEL set "LUMINA_API_MODEL=%LLM_DEFAULT_MODEL%"
if not defined LUMINA_API_TYPE if defined LLM_API_TYPE set "LUMINA_API_TYPE=%LLM_API_TYPE%"
if not defined LUMINA_API_TYPE set "LUMINA_API_TYPE=openai_compatible"
node "%LUMINA_RESOURCE_ROOT%\frontend\dist\index.js" %*
"@
    Set-Content -LiteralPath $Path -Value $launcher -Encoding ASCII
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$tmpDir = Join-Path $repoRoot "tmp"
$backendBuildPath = Join-Path $tmpDir "lumina-backend.exe"
$frontendDist = Join-Path $repoRoot "frontend\dist\index.js"
$defaultsPath = Join-Path $AppRoot "CONFIG\defaults.json"
$installedBackend = Join-Path $InstallDir "lumina-backend.exe"
$installedLauncher = Join-Path $InstallDir "lumina.cmd"
$frontendInstallRoot = Join-Path $AppRoot "frontend"

Write-Host "LuminaCode Windows install"
Write-Host "Repo:        $repoRoot"
Write-Host "Install dir: $InstallDir"
Write-Host "App root:    $AppRoot"

Assert-Command go
Assert-Command node
Assert-Command npm
$cCompilerName = $(if ($env:CC) { $env:CC } else { "gcc" })
if (-not (Get-Command $cCompilerName -ErrorAction SilentlyContinue)) {
    throw "A C compiler is required for local memory embeddings. Install MinGW-w64 gcc or set CC to a compatible compiler before running the installer."
}
$env:CGO_ENABLED = "1"

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
    if (-not $SkipNpmInstall) {
        Invoke-Native "install frontend dependencies" { & npm --prefix frontend install }
    }
    Invoke-Native "build frontend" { & npm --prefix frontend run build }
    if (-not (Test-Path $frontendDist)) {
        throw "Frontend build output was not created: $frontendDist"
    }
    Invoke-Native "build Go backend" { & go build -o $backendBuildPath . }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    New-Item -ItemType Directory -Path $AppRoot -Force | Out-Null

    $preservedDefaults = $null
    if (Test-Path $defaultsPath) {
        $preservedDefaults = Join-Path $tmpDir ("defaults." + [guid]::NewGuid().ToString("N") + ".json")
        Copy-Item -LiteralPath $defaultsPath -Destination $preservedDefaults -Force
    }

    Copy-DirectoryContents -Source (Join-Path $repoRoot ".Lumina") -Destination $AppRoot
    if (-not (Test-Path $defaultsPath)) {
        Copy-Item -LiteralPath (Join-Path $AppRoot "CONFIG\defaults.json.example") -Destination $defaultsPath -Force
    }
    Copy-Item -LiteralPath (Join-Path $repoRoot "setup-searxng.sh") -Destination (Join-Path $AppRoot "setup-searxng.sh") -Force
    if ($preservedDefaults -and -not $WriteDefaults) {
        New-Item -ItemType Directory -Path (Split-Path -Parent $defaultsPath) -Force | Out-Null
        $mergedDefaults = Get-Content -LiteralPath $defaultsPath -Raw | ConvertFrom-Json -AsHashtable
        $userDefaults = Get-Content -LiteralPath $preservedDefaults -Raw | ConvertFrom-Json -AsHashtable
        foreach ($key in $userDefaults.Keys) {
            $mergedDefaults[$key] = $userDefaults[$key]
        }
        $mergedDefaults | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $defaultsPath -Encoding UTF8
    }

    if (Test-Path $frontendInstallRoot) {
        Remove-Item -LiteralPath $frontendInstallRoot -Recurse -Force
    }
    New-Item -ItemType Directory -Path $frontendInstallRoot -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot "frontend\dist") -Destination $frontendInstallRoot -Recurse -Force
    Copy-Item -LiteralPath (Join-Path $repoRoot "frontend\node_modules") -Destination $frontendInstallRoot -Recurse -Force
    Copy-Item -LiteralPath (Join-Path $repoRoot "frontend\package.json") -Destination $frontendInstallRoot -Force

    Copy-Item -LiteralPath $backendBuildPath -Destination $installedBackend -Force
    Write-LuminaLauncher -Path $installedLauncher -AppRoot $AppRoot

    if ($WriteDefaults) {
        Write-LuminaDefaults -Path $defaultsPath -ApiKey $ApiKey -BaseUrl $BaseUrl -Model $Model -ApiType $ApiType -MaxTokens $MaxTokens
        Write-Host "Wrote defaults: $defaultsPath"
    } else {
        Merge-WebDefaults -Path $defaultsPath
        Write-Host "Ensured WebSearch defaults: $defaultsPath"
    }

    try {
        & (Join-Path $repoRoot "scripts\setup-arxiv-mcp-windows.ps1") -AppRoot $AppRoot
    } catch {
        Write-Warning "arXiv MCP setup failed: $($_.Exception.Message). Run scripts\setup-arxiv-mcp-windows.ps1 manually."
    }

    try {
        & (Join-Path $repoRoot "scripts\setup-memory-embedding-windows.ps1") -Action install -AppRoot $AppRoot
    } catch {
        Write-Warning "Memory embedding setup failed: $($_.Exception.Message). Run scripts\setup-memory-embedding-windows.ps1 manually."
    }

    $pathUpdated = $false
    if (-not $NoPathUpdate) {
        $pathUpdated = Add-UserPath -Path $InstallDir
    }

    Invoke-Native "installed launcher help" { & $installedLauncher --help }
} finally {
    Pop-Location
}

Write-Host ""
Write-Host "Installed lumina to $installedLauncher"
Write-Host "Installed lumina-backend to $installedBackend"
Write-Host "Installed resources to $AppRoot"
if ($NoPathUpdate) {
    Write-Host "PATH was not updated because -NoPathUpdate was set."
} elseif ($pathUpdated) {
    Write-Host "Added install dir to the user PATH."
} else {
    Write-Host "Install dir is already in the user PATH."
}
Write-Host ""
Write-Host "To use 'lumina' by name, open a new PowerShell window."
Write-Host "To use it in this PowerShell window immediately, run:"
Write-Host '  $env:Path = "$env:LOCALAPPDATA\LuminaCode\bin;$env:Path"'
Write-Host "Or run the launcher directly:"
Write-Host "  & `"$installedLauncher`" --help"
