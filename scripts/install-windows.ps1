[CmdletBinding()]
param(
    [string]$InstallDir = $(if ($env:LUMINA_INSTALL_DIR) { $env:LUMINA_INSTALL_DIR } elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "LuminaCode\bin" } else { Join-Path $HOME ".local\bin" }),
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
    [switch]$SkipManagedComponents,
    [switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")
$paths = Get-LuminaPaths -AppRoot $AppRoot
$AppRoot = $paths.Root

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

function Convert-SecureStringToPlainText {
    param([Parameter(Mandatory = $true)][securestring]$Value)
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Value)
    try {
        return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    } finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    }
}

function Read-OptionalValue {
    param([Parameter(Mandatory = $true)][string]$Prompt, [string]$CurrentValue = "")
    $label = $(if ($CurrentValue) { "$Prompt [$CurrentValue]" } else { $Prompt })
    $answer = (Read-Host $label).Trim()
    return $(if ([string]::IsNullOrWhiteSpace($answer)) { $CurrentValue } else { $answer })
}

function Normalize-PathSegment {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return "" }
    try { return [IO.Path]::GetFullPath($Path).TrimEnd([char[]]@('\', '/')).ToLowerInvariant() }
    catch { return $Path.TrimEnd([char[]]@('\', '/')).ToLowerInvariant() }
}

function Test-PathListContains {
    param([string]$PathList, [string]$Path)
    $target = Normalize-PathSegment $Path
    foreach ($segment in ($PathList -split ';')) {
        if ((Normalize-PathSegment $segment) -eq $target) { return $true }
    }
    return $false
}

function Add-UserPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    if (Test-PathListContains -PathList $current -Path $Path) {
        if (-not (Test-PathListContains -PathList $env:Path -Path $Path)) { $env:Path = "$Path;$env:Path" }
        return $false
    }
    $next = $(if ([string]::IsNullOrWhiteSpace($current)) { $Path } else { "$current;$Path" })
    [Environment]::SetEnvironmentVariable("Path", $next, "User")
    if (-not (Test-PathListContains -PathList $env:Path -Path $Path)) { $env:Path = "$Path;$env:Path" }
    return $true
}

function Write-ExplicitSettings {
    param([Parameter(Mandatory = $true)][string]$Path)
    $settings = Read-LuminaJsonHashtable -Path $Path
    if ($ApiKey) { $settings["api_key"] = $ApiKey }
    if ($BaseUrl) { $settings["api_base_url"] = $BaseUrl }
    if ($Model) { $settings["api_model"] = $Model }
    $settings["api_type"] = $ApiType
    $settings["api_max_tokens"] = $MaxTokens
    Write-LuminaAtomicJson -Path $Path -Value $settings
}

function Write-LuminaLauncher {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$AppRoot)
    $launcher = @"
@echo off
setlocal
set "LUMINA_BACKEND_BIN=%~dp0lumina-backend.exe"
if not defined LUMINA_APP_ROOT set "LUMINA_APP_ROOT=$AppRoot"
if not defined LUMINA_RESOURCE_ROOT set "LUMINA_RESOURCE_ROOT=%LUMINA_APP_ROOT%\app\resources"
if not defined LUMINA_API_KEY if defined LLM_API_KEY set "LUMINA_API_KEY=%LLM_API_KEY%"
if not defined LUMINA_API_BASE_URL if defined LLM_BASE_URL set "LUMINA_API_BASE_URL=%LLM_BASE_URL%"
if not defined LUMINA_API_MODEL if defined LLM_DEFAULT_MODEL set "LUMINA_API_MODEL=%LLM_DEFAULT_MODEL%"
if not defined LUMINA_API_TYPE if defined LLM_API_TYPE set "LUMINA_API_TYPE=%LLM_API_TYPE%"
if not defined LUMINA_API_TYPE set "LUMINA_API_TYPE=openai_compatible"
node "%LUMINA_APP_ROOT%\app\frontend\dist\index.js" %*
"@
    Set-Content -LiteralPath $Path -Value $launcher -Encoding ASCII
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$tmpDir = Join-Path $repoRoot "tmp"
$backendBuildPath = Join-Path $tmpDir "lumina-backend.exe"
$frontendDist = Join-Path $repoRoot "frontend\dist\index.js"
$installedBackend = Join-Path $InstallDir "lumina-backend.exe"
$installedLauncher = Join-Path $InstallDir "lumina.cmd"
$appNew = Join-Path $AppRoot "app.new"
$appOld = Join-Path $AppRoot "app.old"
$swapped = $false
$version = "dev"
$installStage = "startup"
$installLogRoot = Join-Path $tmpDir "install-logs"
New-Item -ItemType Directory -Path $installLogRoot -Force | Out-Null
$installLog = Join-Path $installLogRoot "install-$((Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'))-$PID.log"
Start-Transcript -LiteralPath $installLog -Force | Out-Null

try {
    $installStage = "hardware and model preflight"
    Write-Host "LuminaCode Windows install"
    Write-Host "Repo:        $repoRoot"
    Write-Host "Install dir: $InstallDir"
    Write-Host "App root:    $AppRoot"
    Write-Host "Install log: $installLog"

    Assert-Command go
    Assert-Command node
    Assert-Command npm
    Assert-Command curl.exe
    $cCompilerName = $(if ($env:CC) { $env:CC } else { "gcc" })
    Assert-Command $cCompilerName
    $env:CGO_ENABLED = "1"

    $processor = Get-CimInstance Win32_Processor | Select-Object -First 1
    $video = @(Get-CimInstance Win32_VideoController | Select-Object -ExpandProperty Name)
    $memoryBytes = (Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
    Write-Host "Hardware preflight"
    Write-Host "  platform: Windows/$env:PROCESSOR_ARCHITECTURE"
    Write-Host "  processor: $($processor.Name.Trim())"
    Write-Host "  accelerator: $(if ($video) { $video -join ', ' } else { 'none detected' })"
    Write-Host "  memory: $([math]::Floor($memoryBytes / 1MB)) MiB"

    if ($SkipManagedComponents) {
        Write-Host "  managed memory runtime: skipped (SkipManagedComponents)"
    } elseif ($env:SKIP_MEMORY_MODELS -eq "1") {
        & (Join-Path $PSScriptRoot "setup-memory-models-windows.ps1") -Action preflight-installed -AppRoot $AppRoot
        if ($LASTEXITCODE -ne 0) { throw "Preinstalled BGE-M3 preflight failed." }
    } else {
        & (Join-Path $PSScriptRoot "setup-memory-models-windows.ps1") -Action preflight -AppRoot $AppRoot
        if ($LASTEXITCODE -ne 0) { throw "BGE-M3 install preflight failed." }
    }

    $installStage = "configuration"
    if ($ConfigureApi) {
        if (-not $ApiKey) {
            $ApiKey = Convert-SecureStringToPlainText (Read-Host "API key" -AsSecureString)
        }
        $BaseUrl = Read-OptionalValue -Prompt "API base URL" -CurrentValue $BaseUrl
        $Model = Read-OptionalValue -Prompt "API model" -CurrentValue $Model
        $ApiType = Read-OptionalValue -Prompt "API type (openai_compatible, anthropic, auto)" -CurrentValue $ApiType
        if ($ApiType -notin @("openai_compatible", "anthropic", "auto")) { throw "Invalid API type '$ApiType'." }
        $WriteDefaults = $true
    }

    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null
    Push-Location $repoRoot
    try {
        $installStage = "frontend build"
        if (-not $SkipNpmInstall) { Invoke-Native "install frontend dependencies" { & npm --prefix frontend install } }
        Invoke-Native "build frontend" { & npm --prefix frontend run build }
        if (-not (Test-Path $frontendDist)) { throw "Frontend build output was not created: $frontendDist" }
        $installStage = "Go backend build"
	    Invoke-Native "build Go backend" { & go build -o $backendBuildPath . }
	    if (-not $SkipManagedComponents) {
            $installStage = "memory model installation"
		    if ($env:SKIP_MEMORY_MODELS -eq "1") {
			    Invoke-Native "verify preinstalled memory models" { & (Join-Path $PSScriptRoot "setup-memory-models-windows.ps1") -Action doctor -AppRoot $AppRoot -Backend $backendBuildPath }
		    } else {
			    Invoke-Native "install memory models" { & (Join-Path $PSScriptRoot "setup-memory-models-windows.ps1") -Action install -AppRoot $AppRoot -Backend $backendBuildPath }
		    }
	    }
    try {
        $described = & git describe --tags --always --dirty 2>$null
        if ($LASTEXITCODE -eq 0 -and $described) { $version = ([string]$described).Trim() }
    } catch {}

        $installStage = "atomic application deployment"
        New-Item -ItemType Directory -Path $AppRoot -Force | Out-Null
    if ((Test-Path $appNew) -or (Test-Path $appOld)) {
        throw "Stale app.new/app.old exists under $AppRoot; inspect it before retrying."
    }
    foreach ($directory in @("frontend", "resources\defaults", "resources\system", "resources\skills", "resources\teams", "extensions", "scripts")) {
        New-Item -ItemType Directory -Path (Join-Path $appNew $directory) -Force | Out-Null
    }
    Copy-DirectoryContents -Source (Join-Path $repoRoot ".Lumina\SYSTEM") -Destination (Join-Path $appNew "resources\system")
    Copy-DirectoryContents -Source (Join-Path $repoRoot ".Lumina\SKILLS") -Destination (Join-Path $appNew "resources\skills")
    Copy-DirectoryContents -Source (Join-Path $repoRoot ".Lumina\TEAM") -Destination (Join-Path $appNew "resources\teams")
    Copy-Item -LiteralPath (Join-Path $repoRoot ".Lumina\CONFIG\defaults.json.example") -Destination (Join-Path $appNew "resources\defaults\settings.example.json")
    Copy-Item -LiteralPath (Join-Path $repoRoot "frontend\dist") -Destination (Join-Path $appNew "frontend") -Recurse
    Copy-Item -LiteralPath (Join-Path $repoRoot "frontend\package.json"), (Join-Path $repoRoot "frontend\package-lock.json") -Destination (Join-Path $appNew "frontend")
	Copy-Item -LiteralPath (Join-Path $PSScriptRoot "app-paths.ps1"), (Join-Path $PSScriptRoot "setup-arxiv-mcp-windows.ps1"), (Join-Path $PSScriptRoot "setup-memory-models-windows.ps1"), (Join-Path $PSScriptRoot "memory-models.lock") -Destination (Join-Path $appNew "scripts")
    Push-Location (Join-Path $appNew "frontend")
    try { Invoke-Native "install production frontend dependencies" { & npm ci --omit=dev } } finally { Pop-Location }
    Remove-Item -LiteralPath (Join-Path $appNew "frontend\package-lock.json") -Force

    $env:LUMINA_APP_ROOT = $AppRoot
    & $backendBuildPath shutdown 2>$null
    Invoke-Native "migrate AppRoot layout" { & $backendBuildPath layout migrate --apply --project-root $repoRoot --packaged-resources (Join-Path $appNew "resources") --installed-version $version }

    if (Test-Path $paths.Extensions) {
        Copy-DirectoryContents -Source $paths.Extensions -Destination (Join-Path $appNew "extensions")
    }
    if (Test-Path $paths.App) { Move-Item -LiteralPath $paths.App -Destination $appOld }
    Move-Item -LiteralPath $appNew -Destination $paths.App
    $swapped = $true
    if (-not (Test-Path (Join-Path $paths.Frontend "dist\index.js"))) { throw "Installed frontend health check failed." }
    if (-not (Test-Path (Join-Path $paths.Resources "system\system-prompt.md"))) { throw "Installed resource health check failed." }
    Invoke-Native "AppRoot health check" { & $backendBuildPath layout doctor --json }
    Remove-Item -LiteralPath $appOld -Recurse -Force -ErrorAction SilentlyContinue
    $swapped = $false

        $installStage = "launcher installation"
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -LiteralPath $backendBuildPath -Destination $installedBackend -Force
    Write-LuminaLauncher -Path $installedLauncher -AppRoot $AppRoot

    if ($WriteDefaults) {
        Write-ExplicitSettings -Path $paths.Settings
        Write-Host "Wrote explicit settings: $($paths.Settings)"
    }
    Set-LuminaPrivateAcl -Path @($paths.Config, $paths.Data, $paths.State)

    if (-not $SkipManagedComponents) {
        try { & (Join-Path $paths.App "scripts\setup-arxiv-mcp-windows.ps1") -Action install -AppRoot $AppRoot }
        catch { Write-Warning "arXiv MCP setup failed: $($_.Exception.Message)" }
    }

    $pathUpdated = $false
    if (-not $NoPathUpdate) { $pathUpdated = Add-UserPath -Path $InstallDir }
    Invoke-Native "installed launcher help" { & $installedLauncher --help }
    } catch {
        if ($swapped) {
            Remove-Item -LiteralPath $paths.App -Recurse -Force -ErrorAction SilentlyContinue
            if (Test-Path $appOld) { Move-Item -LiteralPath $appOld -Destination $paths.App }
        }
        throw
    } finally {
        Remove-Item -LiteralPath $appNew -Recurse -Force -ErrorAction SilentlyContinue
        Pop-Location
    }

    $installStage = "complete"
    Write-Host ""
    Write-Host "Installed LuminaCode $version"
    Write-Host "Launcher: $installedLauncher"
    Write-Host "Backend:  $installedBackend"
    Write-Host "AppRoot:  $AppRoot"
    Write-Host "Log:      $installLog"
    if ($NoPathUpdate) { Write-Host "PATH was not updated." }
    elseif ($pathUpdated) { Write-Host "Added install dir to the user PATH." }
    else { Write-Host "Install dir is already in the user PATH." }
} catch {
    Write-Error @"
LuminaCode installation failed.
  stage: $installStage
  error: $($_.Exception.Message)
  log: $installLog
  application: app.new was cleaned and a swapped application was restored automatically
Fix the reported error and run the installer again.
"@
    exit 1
} finally {
    try { Stop-Transcript | Out-Null } catch {}
}
