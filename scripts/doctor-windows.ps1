[CmdletBinding()]
param(
    [string]$InstallDir = $(if ($env:LUMINA_INSTALL_DIR) { $env:LUMINA_INSTALL_DIR } elseif ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "LuminaCode\bin" } else { Join-Path $HOME ".local\bin" }),
    [string]$AppRoot = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")
$paths = Get-LuminaPaths -AppRoot $AppRoot
$AppRoot = $paths.Root

function Command-Version {
    param([Parameter(Mandatory = $true)][string]$Command)
    $found = Get-Command $Command -ErrorAction SilentlyContinue
    if (-not $found) {
        return "not found"
    }
    try {
        $value = & $Command --version 2>$null | Select-Object -First 1
        if ($LASTEXITCODE -eq 0 -and $value) {
            return $value
        }
    } catch {
    }
    return $found.Source
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

function Status-Line {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Value
    )
    "{0,-18} {1}" -f ($Name + ":"), $Value
}

$launcher = Join-Path $InstallDir "lumina.cmd"
$backend = Join-Path $InstallDir "lumina-backend.exe"
$frontend = Join-Path $paths.Frontend "dist\index.js"
$systemPrompt = Join-Path $paths.Resources "system\system-prompt.md"
$skills = Join-Path $paths.Resources "skills"
$defaults = $paths.Settings
$mcpConfig = $paths.McpConfig
$arxivPython = Join-Path $paths.Extensions "arxiv-mcp\.venv\Scripts\python.exe"
$embeddingModel = Join-Path $paths.MemoryModel "model.onnx"
$embeddingTokenizer = Join-Path $paths.MemoryModel "tokenizer.json"
$embeddingRuntime = Join-Path $paths.MemoryModel "runtime\onnxruntime.dll"
$endpoint = $paths.Endpoint
$command = Get-Command lumina -ErrorAction SilentlyContinue
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")

Status-Line "OS" ([Environment]::OSVersion.VersionString)
Status-Line "Go" (Command-Version go)
Status-Line "Node" (Command-Version node)
Status-Line "npm" (Command-Version npm)
Status-Line "Install dir" $InstallDir
Status-Line "App root" $AppRoot
Status-Line "Current PATH" ($(if (Test-PathListContains -PathList $env:Path -Path $InstallDir) { "install dir is in this PowerShell window" } else { "install dir is not in this PowerShell window" }))
Status-Line "User PATH" ($(if (Test-PathListContains -PathList $userPath -Path $InstallDir) { "install dir is in the user PATH" } else { "install dir is not in the user PATH" }))
Status-Line "Command" ($(if ($command) { $command.Source } else { "not found in current PATH" }))
Status-Line "Launcher" ($(if (Test-Path $launcher) { $launcher } else { "missing" }))
Status-Line "Backend" ($(if (Test-Path $backend) { $backend } else { "missing" }))
Status-Line "Frontend" ($(if (Test-Path $frontend) { $frontend } else { "missing" }))
Status-Line "System prompt" ($(if (Test-Path $systemPrompt) { $systemPrompt } else { "missing" }))
Status-Line "Skills" ($(if (Test-Path $skills) { $skills } else { "missing" }))
Status-Line "Defaults" ($(if (Test-Path $defaults) { $defaults } else { "not configured" }))
if (Test-Path $defaults) {
    try {
        $defaultsJson = Get-Content -LiteralPath $defaults -Raw | ConvertFrom-Json
        $webBase = [string]$defaultsJson.web_search_base_url
        Status-Line "WebSearch" ($(if ($webBase) { $webBase } else { "not configured" }))
        if ($webBase) {
            try {
                Invoke-WebRequest -Uri "$webBase/search?q=lumina&format=json" -UseBasicParsing -TimeoutSec 5 | Out-Null
                Status-Line "SearxNG" "JSON API ready"
            } catch {
                Status-Line "SearxNG" "not reachable or JSON disabled"
            }
        }
    } catch {
        Status-Line "WebSearch" "defaults parse failed"
    }
} else {
    Status-Line "WebSearch" "not configured"
}
Status-Line "arXiv MCP" ($(if ((Test-Path $arxivPython) -and (Test-Path $mcpConfig)) { "installed" } else { "not installed" }))
Status-Line "Embedding" ($(if ((Test-Path $embeddingModel) -and (Test-Path $embeddingTokenizer) -and (Test-Path $embeddingRuntime)) { "installed" } else { "missing" }))
if (Test-Path $backend) {
    try {
        & $backend memory doctor | Out-Null
        Status-Line "Embedding test" "inference ready"
    } catch {
        Status-Line "Embedding test" "failed: $($_.Exception.Message)"
    }
}
Status-Line "Endpoint" ($(if (Test-Path $endpoint) { $endpoint } else { "not running" }))
if (Test-Path $backend) {
    try {
        $env:LUMINA_APP_ROOT = $AppRoot
        & $backend layout doctor --json
    } catch {
        Status-Line "Layout doctor" "failed: $($_.Exception.Message)"
    }
}
