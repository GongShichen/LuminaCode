function Resolve-LuminaAppRoot {
    param([string]$Override = "")

    $root = $Override
    if ([string]::IsNullOrWhiteSpace($root)) {
        $root = $env:LUMINA_APP_ROOT
    }
    if ([string]::IsNullOrWhiteSpace($root)) {
        if (-not [string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
            $root = Join-Path $env:LOCALAPPDATA "LuminaCode"
        } else {
            $homeRoot = $(if ($env:USERPROFILE) { $env:USERPROFILE } else { $HOME })
            if ([string]::IsNullOrWhiteSpace($homeRoot)) {
                throw "Cannot resolve LuminaCode AppRoot: LOCALAPPDATA and USERPROFILE are empty."
            }
            $root = Join-Path $homeRoot ".lumina"
        }
    }
    if (-not [IO.Path]::IsPathRooted($root)) {
        throw "LUMINA_APP_ROOT must be absolute: $root"
    }
    $root = [IO.Path]::GetFullPath($root).TrimEnd([char[]]@('\', '/'))
    $rootPrefix = [IO.Path]::GetPathRoot($root)
    $rootPrefix = $rootPrefix.TrimEnd([char[]]@('\', '/'))
    if ($root -eq $rootPrefix) {
        throw "Refusing unsafe LuminaCode AppRoot: $root"
    }
    return $root
}

function ConvertTo-LuminaHashtable {
    param($Value)
    if ($null -eq $Value) { return $null }
    if ($Value -is [System.Collections.IDictionary]) {
        $table = @{}
        foreach ($key in $Value.Keys) { $table[[string]$key] = ConvertTo-LuminaHashtable $Value[$key] }
        return $table
    }
    if ($Value -is [PSCustomObject]) {
        $table = @{}
        foreach ($property in $Value.PSObject.Properties) { $table[$property.Name] = ConvertTo-LuminaHashtable $property.Value }
        return $table
    }
    if (($Value -is [System.Collections.IEnumerable]) -and -not ($Value -is [string])) {
        $items = @($Value | ForEach-Object { ConvertTo-LuminaHashtable $_ })
        return ,$items
    }
    return $Value
}

function Read-LuminaJsonHashtable {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return @{} }
    $parsed = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    $value = ConvertTo-LuminaHashtable $parsed
    if ($null -eq $value) { return @{} }
    return $value
}

function Get-LuminaPaths {
    param([string]$AppRoot = "")

    $root = Resolve-LuminaAppRoot -Override $AppRoot
    return [pscustomobject]@{
        Root = $root
        Layout = Join-Path $root "layout.json"
        App = Join-Path $root "app"
        Frontend = Join-Path $root "app\frontend"
        Resources = Join-Path $root "app\resources"
        Extensions = Join-Path $root "app\extensions"
        Config = Join-Path $root "config"
        Settings = Join-Path $root "config\settings.json"
        McpConfig = Join-Path $root "config\mcp.json"
        Data = Join-Path $root "data"
        State = Join-Path $root "state"
        Endpoint = Join-Path $root "state\run\backend.json"
        ManagedMcp = Join-Path $root "state\managed\mcp.json"
        SearxNG = Join-Path $root "state\services\searxng"
        Cache = Join-Path $root "cache"
        MemoryModel = Join-Path $root "cache\models\memory\multilingual-e5-small"
    }
}

function Set-LuminaPrivateAcl {
    param([Parameter(Mandatory = $true)][string[]]$Path)

    if ($env:OS -ne "Windows_NT") {
        return
    }
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    foreach ($item in $Path) {
        if (-not (Test-Path -LiteralPath $item)) {
            continue
        }
        & icacls.exe $item /inheritance:r /grant:r "${identity}:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to restrict ACL for $item"
        }
    }
}

function Write-LuminaAtomicJson {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)]$Value,
        [int]$Depth = 12
    )

    $directory = Split-Path -Parent $Path
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
    $temporary = Join-Path $directory (".lumina-" + [guid]::NewGuid().ToString("N") + ".tmp")
    $encoding = New-Object Text.UTF8Encoding($false)
    $stream = [IO.File]::Open($temporary, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try {
        $writer = New-Object IO.StreamWriter($stream, $encoding)
        try {
            $writer.Write(($Value | ConvertTo-Json -Depth $Depth))
            $writer.Write("`n")
            $writer.Flush()
            $stream.Flush($true)
        } finally {
            $writer.Dispose()
        }
    } finally {
        $stream.Dispose()
    }
    try {
        if (Test-Path -LiteralPath $Path) {
            [IO.File]::Replace($temporary, $Path, $null, $true)
        } else {
            [IO.File]::Move($temporary, $Path)
        }
    } finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}
