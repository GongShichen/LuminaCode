[CmdletBinding()]
param(
    [string]$AppRoot = $(if ($env:LUMINA_APP_ROOT) { $env:LUMINA_APP_ROOT } else { Join-Path $HOME ".lumina" }),
    [string]$RepoUrl = $(if ($env:LUMINA_ARXIV_MCP_REPO) { $env:LUMINA_ARXIV_MCP_REPO } else { "https://github.com/kelvingao/arxiv-mcp.git" })
)

$ErrorActionPreference = "Stop"

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

function Merge-McpConfig {
    param(
        [Parameter(Mandatory = $true)][string]$ConfigPath,
        [Parameter(Mandatory = $true)][string]$ManagedPath,
        [Parameter(Mandatory = $true)][string]$ArxivCommand,
        [Parameter(Mandatory = $true)][string]$RunnerFile,
        [Parameter(Mandatory = $true)][string]$SourceDir
    )
    New-Item -ItemType Directory -Path (Split-Path -Parent $ConfigPath) -Force | Out-Null
    $server = [ordered]@{
        command = $ArxivCommand
        args = @($RunnerFile)
        env = [ordered]@{ TRANSPORT = "stdio" }
        cwd = $SourceDir
    }
    if (Test-Path $ConfigPath) {
        $data = Get-Content -LiteralPath $ConfigPath -Raw | ConvertFrom-Json -AsHashtable
    } else {
        $data = @{}
    }
    if (-not $data.ContainsKey("mcpServers")) {
        $data["mcpServers"] = @{}
    }
    if (Test-Path $ManagedPath) {
        $managed = Get-Content -LiteralPath $ManagedPath -Raw | ConvertFrom-Json -AsHashtable
    } else {
        $managed = @{}
    }
    $existing = $(if ($data["mcpServers"].ContainsKey("arxiv")) { $data["mcpServers"]["arxiv"] } else { $null })
    $managedExisting = $(if ($managed.ContainsKey("mcpServers") -and $managed["mcpServers"].ContainsKey("arxiv")) { $managed["mcpServers"]["arxiv"] } else { $null })
    $owned = ($null -eq $existing) -or (($null -ne $managedExisting) -and (($existing | ConvertTo-Json -Depth 12) -eq ($managedExisting | ConvertTo-Json -Depth 12))) -or ([string]$existing.command -like "*\.lumina\mcp\arxiv-mcp*")
    if (-not $owned) {
        Write-Host "arXiv MCP already exists in mcp.json; leaving user config unchanged."
        return
    }
    $data["mcpServers"]["arxiv"] = $server
    $data | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $ConfigPath -Encoding UTF8
    if (-not $managed.ContainsKey("mcpServers")) {
        $managed["mcpServers"] = @{}
    }
    $managed["mcpServers"]["arxiv"] = $server
    $managed | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $ManagedPath -Encoding UTF8
}

function Patch-SourceCompatibility {
    param([Parameter(Mandatory = $true)][string]$SourceDir)

    $serverPy = Join-Path $SourceDir "src\server.py"
    if (-not (Test-Path $serverPy)) {
        Write-Warning "arxiv-mcp server.py not found at $serverPy; skipping compatibility patch."
        return
    }

    $text = Get-Content -LiteralPath $serverPy -Raw
    $old = '    description="MCP server for retrieving papers from arXiv based on keywords",' + "`n"
    if ($text.Contains($old)) {
        $text = $text.Replace($old, "")
        Set-Content -LiteralPath $serverPy -Value $text -Encoding UTF8NoBOM
        Write-Host "Patched arxiv-mcp FastMCP description compatibility."
    } else {
        Write-Host "arxiv-mcp FastMCP compatibility patch already applied or unnecessary."
    }
}

function Write-ArxivRunner {
    param(
        [Parameter(Mandatory = $true)][string]$RunnerFile,
        [Parameter(Mandatory = $true)][string]$SourceDir
    )
    New-Item -ItemType Directory -Path (Split-Path -Parent $RunnerFile) -Force | Out-Null
    $escapedSource = $SourceDir.Replace("\", "\\")
    $content = @"
import asyncio
import pathlib
import sys

source = pathlib.Path(r"$escapedSource")
sys.path.insert(0, str(source / "src"))

from server import main

asyncio.run(main())
"@
    Set-Content -LiteralPath $RunnerFile -Value $content -Encoding UTF8NoBOM
}

$mcpRoot = Join-Path $AppRoot "mcp\arxiv-mcp"
$sourceDir = Join-Path $mcpRoot "source"
$venvDir = Join-Path $mcpRoot ".venv"
$runnerFile = Join-Path $mcpRoot "run-arxiv-mcp.py"
$configPath = Join-Path $AppRoot "CONFIG\mcp.json"
$managedPath = Join-Path $AppRoot "CONFIG\managed-mcp.json"

Assert-Command git
Assert-Command python

$version = & python -c "import sys; print('%d.%d' % sys.version_info[:2])"
if ([version]$version -lt [version]"3.11") {
    throw "Python 3.11+ is required for arXiv MCP, got $version."
}

if (-not (Get-Command uv -ErrorAction SilentlyContinue)) {
    Invoke-Native "install uv" { & python -m pip install --user uv }
}
Assert-Command uv

New-Item -ItemType Directory -Path $mcpRoot -Force | Out-Null
if (Test-Path (Join-Path $sourceDir ".git")) {
    try {
        Invoke-Native "update arxiv-mcp" { & git -C $sourceDir pull --ff-only }
    } catch {
        Write-Warning "Could not update existing arxiv-mcp checkout; continuing with local source at $sourceDir. $($_.Exception.Message)"
    }
} elseif (Test-Path $sourceDir) {
    throw "$sourceDir exists but is not a git checkout."
} else {
    Invoke-Native "clone arxiv-mcp" { & git clone $RepoUrl $sourceDir }
}
Patch-SourceCompatibility -SourceDir $sourceDir

if (-not (Test-Path $venvDir)) {
    Invoke-Native "create arxiv-mcp venv" { & python -m venv $venvDir }
}
$venvPython = Join-Path $venvDir "Scripts\python.exe"
Invoke-Native "install arxiv-mcp" { & uv pip install --python $venvPython -e $sourceDir }
Write-ArxivRunner -RunnerFile $runnerFile -SourceDir $sourceDir
Merge-McpConfig -ConfigPath $configPath -ManagedPath $managedPath -ArxivCommand $venvPython -RunnerFile $runnerFile -SourceDir $sourceDir

Write-Host "arXiv MCP source: $sourceDir"
Write-Host "arXiv MCP python: $venvPython"
Write-Host "arXiv MCP command: $venvPython $runnerFile"
Write-Host "MCP config: $configPath"
