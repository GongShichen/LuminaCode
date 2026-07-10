[CmdletBinding()]
param(
    [string]$InstallDir = $(if ($env:LUMINA_INSTALL_DIR) { $env:LUMINA_INSTALL_DIR } elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "LuminaCode\bin" } else { Join-Path $HOME ".local\bin" }),
    [string]$AppRoot = $(if ($env:LUMINA_APP_ROOT) { $env:LUMINA_APP_ROOT } else { Join-Path $HOME ".lumina" }),
    [switch]$KeepResources,
    [switch]$RemovePath
)

$ErrorActionPreference = "Stop"

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

function Remove-UserPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    $target = Normalize-PathSegment $Path
    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    if ([string]::IsNullOrWhiteSpace($current)) {
        return $false
    }
    $kept = @()
    $removed = $false
    foreach ($segment in ($current -split ';')) {
        if ([string]::IsNullOrWhiteSpace($segment)) {
            continue
        }
        if ((Normalize-PathSegment $segment) -eq $target) {
            $removed = $true
            continue
        }
        $kept += $segment
    }
    if ($removed) {
        [Environment]::SetEnvironmentVariable("Path", ($kept -join ';'), "User")
    }
    return $removed
}

function Stop-LuminaBackend {
    param(
        [Parameter(Mandatory = $true)][string]$BackendPath,
        [Parameter(Mandatory = $true)][string]$EndpointPath
    )

    $pids = @()
    if (Test-Path $EndpointPath) {
        try {
            $endpoint = Get-Content -Raw -LiteralPath $EndpointPath | ConvertFrom-Json
            if ($endpoint.pid) {
                $pids += [int]$endpoint.pid
            }
        } catch {
        }
    }

    $target = Normalize-PathSegment $BackendPath
    Get-Process -ErrorAction SilentlyContinue | Where-Object {
        $_.Path -and (Normalize-PathSegment $_.Path) -eq $target
    } | ForEach-Object {
        $pids += $_.Id
    }

    foreach ($pidValue in ($pids | Sort-Object -Unique)) {
        $process = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
        if (-not $process) {
            continue
        }
        try {
            Stop-Process -Id $pidValue -Force -ErrorAction Stop
            Write-Host "Stopped lumina-backend process $pidValue"
        } catch {
            Write-Host "Could not stop lumina-backend process ${pidValue}: $($_.Exception.Message)"
        }
    }
}

$launcher = Join-Path $InstallDir "lumina.cmd"
$backend = Join-Path $InstallDir "lumina-backend.exe"
$endpoint = Join-Path $HOME ".lumina\run\backend.json"

Stop-LuminaBackend -BackendPath $backend -EndpointPath $endpoint

foreach ($path in @($launcher, $backend, $endpoint)) {
    if (Test-Path $path) {
        Remove-Item -LiteralPath $path -Force
        Write-Host "Removed $path"
    }
}

$embeddingRoot = Join-Path $AppRoot "models\memory"
if (Test-Path $embeddingRoot) {
    Remove-Item -LiteralPath $embeddingRoot -Recurse -Force
    Write-Host "Removed $embeddingRoot"
}

if (-not $KeepResources -and (Test-Path $AppRoot)) {
    Remove-Item -LiteralPath $AppRoot -Recurse -Force
    Write-Host "Removed $AppRoot"
}

if ($RemovePath) {
    if (Remove-UserPath -Path $InstallDir) {
        Write-Host "Removed install dir from the user PATH."
    } else {
        Write-Host "Install dir was not present in the user PATH."
    }
}

Write-Host "Uninstall complete."
