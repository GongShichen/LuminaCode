[CmdletBinding()]
param(
    [string]$InstallDir = $(if ($env:LUMINA_INSTALL_DIR) { $env:LUMINA_INSTALL_DIR } elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "LuminaCode\bin" } else { Join-Path $HOME ".local\bin" }),
    [string]$AppRoot = "",
    [switch]$Purge,
    [switch]$RemovePath
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")
$paths = Get-LuminaPaths -AppRoot $AppRoot
$AppRoot = $paths.Root

function Normalize-PathSegment {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return "" }
    try { return [IO.Path]::GetFullPath($Path).TrimEnd([char[]]@('\', '/')).ToLowerInvariant() }
    catch { return $Path.TrimEnd([char[]]@('\', '/')).ToLowerInvariant() }
}

function Remove-UserPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $target = Normalize-PathSegment $Path
    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    if ([string]::IsNullOrWhiteSpace($current)) { return $false }
    $kept = @()
    $removed = $false
    foreach ($segment in ($current -split ';')) {
        if ([string]::IsNullOrWhiteSpace($segment)) { continue }
        if ((Normalize-PathSegment $segment) -eq $target) { $removed = $true; continue }
        $kept += $segment
    }
    if ($removed) { [Environment]::SetEnvironmentVariable("Path", ($kept -join ';'), "User") }
    return $removed
}

function Stop-LuminaBackend {
    param([Parameter(Mandatory = $true)][string]$BackendPath, [Parameter(Mandatory = $true)][string[]]$EndpointPath)
    $pids = @()
    foreach ($endpointFile in $EndpointPath) {
        if (-not (Test-Path $endpointFile)) { continue }
        try {
            $endpoint = Get-Content -Raw -LiteralPath $endpointFile | ConvertFrom-Json
            if ($endpoint.pid) { $pids += [int]$endpoint.pid }
        } catch {}
    }
    $target = Normalize-PathSegment $BackendPath
    Get-Process -ErrorAction SilentlyContinue | Where-Object {
        try { $_.Path -and (Normalize-PathSegment $_.Path) -eq $target } catch { $false }
    } | ForEach-Object { $pids += $_.Id }
    foreach ($pidValue in ($pids | Sort-Object -Unique)) {
        if (Get-Process -Id $pidValue -ErrorAction SilentlyContinue) {
            Stop-Process -Id $pidValue -Force -ErrorAction SilentlyContinue
            Write-Host "Stopped lumina-backend process $pidValue"
        }
    }
}

$launcher = Join-Path $InstallDir "lumina.cmd"
$backend = Join-Path $InstallDir "lumina-backend.exe"
$legacyEndpoint = Join-Path $(if ($env:USERPROFILE) { $env:USERPROFILE } else { $HOME }) ".lumina\run\backend.json"
Stop-LuminaBackend -BackendPath $backend -EndpointPath @($paths.Endpoint, $legacyEndpoint)

$arxivSetup = Join-Path $paths.App "scripts\setup-arxiv-mcp-windows.ps1"
if (-not (Test-Path $arxivSetup)) { $arxivSetup = Join-Path $PSScriptRoot "setup-arxiv-mcp-windows.ps1" }
if (Test-Path $arxivSetup) {
    try { & $arxivSetup -Action uninstall -AppRoot $AppRoot }
    catch { Write-Warning "Could not remove installer-owned arXiv MCP config: $($_.Exception.Message)" }
}

$modelSetup = Join-Path $paths.App "scripts\setup-memory-models-windows.ps1"
if (-not (Test-Path $modelSetup)) { $modelSetup = Join-Path $PSScriptRoot "setup-memory-models-windows.ps1" }
if (Test-Path $modelSetup) {
    try { & $modelSetup -Action uninstall -AppRoot $AppRoot -Backend $backend }
    catch { Write-Warning "Could not remove managed memory models: $($_.Exception.Message)" }
}


foreach ($path in @($launcher, $backend)) {
    if (Test-Path $path) { Remove-Item -LiteralPath $path -Force; Write-Host "Removed $path" }
}

if ($Purge) {
    Remove-Item -LiteralPath $AppRoot -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "Purged $AppRoot"
} else {
    foreach ($layer in @($paths.App, $paths.Cache, $paths.State)) {
        Remove-Item -LiteralPath $layer -Recurse -Force -ErrorAction SilentlyContinue
    }
    Write-Host "Preserved $($paths.Config), $($paths.Data), and $($paths.Layout)"
}

if ($RemovePath) {
    if (Remove-UserPath -Path $InstallDir) { Write-Host "Removed install dir from the user PATH." }
    else { Write-Host "Install dir was not present in the user PATH." }
}

Write-Host "Uninstall complete."
