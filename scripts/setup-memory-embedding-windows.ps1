[CmdletBinding()]
param(
    [ValidateSet("install", "status", "uninstall")]
    [string]$Action = "install",
    [string]$AppRoot = $(if ($env:LUMINA_APP_ROOT) { $env:LUMINA_APP_ROOT } else { Join-Path $HOME ".lumina" })
)

$ErrorActionPreference = "Stop"
$modelName = "multilingual-e5-small"
$modelDir = Join-Path $AppRoot "models\memory\$modelName"
$modelUrl = "https://modelscope.cn/models/AI-ModelScope/multilingual-e5-small/resolve/master/onnx/model.onnx"
$tokenizerUrl = "https://modelscope.cn/models/AI-ModelScope/multilingual-e5-small/resolve/master/onnx/tokenizer.json"
$modelHash = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
$tokenizerHash = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
$ortRelease = "1.26.0"
$device = $(if ($env:LUMINA_MEMORY_EMBEDDING_DEVICE) { $env:LUMINA_MEMORY_EMBEDDING_DEVICE.ToLowerInvariant() } else { "auto" })
$useCuda = ($device -eq "cuda") -or (($device -eq "auto") -and (Get-Command nvidia-smi -ErrorAction SilentlyContinue))
$ortArchive = $(if ($useCuda) { "onnxruntime-win-x64-gpu-$ortRelease.zip" } else { "onnxruntime-win-x64-$ortRelease.zip" })
$ortHash = $(if ($useCuda) { "1133b1bcb0fb6f82b1c5b470b7cc15f9080a58b27dbc7b579a1fd63125ec2a15" } else { "6ebe99b5564bf4d029b6e93eac9ff423682b6212eade769e9ca3f685eaf500b4" })
$runtimeProvider = $(if ($useCuda) { "cuda" } else { "cpu" })

function Test-Hash {
    param([string]$Path, [string]$Expected)
    return (Test-Path $Path) -and ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() -eq $Expected)
}

function Receive-CheckedFile {
    param([string]$Uri, [string]$Path, [string]$Expected)
    if (Test-Hash -Path $Path -Expected $Expected) { return }
    $temporary = "$Path.download"
    Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    Write-Host "Downloading $Uri"
    Invoke-WebRequest -Uri $Uri -OutFile $temporary -UseBasicParsing
    if (-not (Test-Hash -Path $temporary -Expected $Expected)) {
        throw "SHA-256 mismatch for $Uri"
    }
    Move-Item -LiteralPath $temporary -Destination $Path -Force
}

function Install-Embedding {
    if (-not [Environment]::Is64BitOperatingSystem) {
        throw "The memory embedding runtime requires 64-bit Windows."
    }
    New-Item -ItemType Directory -Path $modelDir -Force | Out-Null
    Receive-CheckedFile -Uri $modelUrl -Path (Join-Path $modelDir "model.onnx") -Expected $modelHash
    Receive-CheckedFile -Uri $tokenizerUrl -Path (Join-Path $modelDir "tokenizer.json") -Expected $tokenizerHash
    $runtimeDir = Join-Path $modelDir "runtime"
    $runtimeLibrary = Join-Path $runtimeDir "onnxruntime.dll"
    $providerFile = Join-Path $runtimeDir "provider"
    $installedProvider = $(if (Test-Path $providerFile) { (Get-Content -LiteralPath $providerFile -Raw).Trim() } else { "" })
    if ((-not (Test-Path $runtimeLibrary)) -or ($installedProvider -ne $runtimeProvider)) {
        $archive = Join-Path $modelDir $ortArchive
        Receive-CheckedFile -Uri "https://github.com/microsoft/onnxruntime/releases/download/v$ortRelease/$ortArchive" -Path $archive -Expected $ortHash
        $extractDir = Join-Path $modelDir ".runtime-extract"
        Remove-Item -LiteralPath $extractDir -Recurse -Force -ErrorAction SilentlyContinue
        Expand-Archive -LiteralPath $archive -DestinationPath $extractDir -Force
        $source = Get-ChildItem -LiteralPath $extractDir -Recurse -Filter "onnxruntime.dll" | Select-Object -First 1
        if (-not $source) { throw "onnxruntime.dll was not found in $ortArchive" }
        New-Item -ItemType Directory -Path $runtimeDir -Force | Out-Null
        Get-ChildItem -LiteralPath $source.DirectoryName -Filter "onnxruntime*.dll" | Copy-Item -Destination $runtimeDir -Force
        Set-Content -LiteralPath $providerFile -Value $runtimeProvider -Encoding ASCII
        Remove-Item -LiteralPath $extractDir -Recurse -Force
        Remove-Item -LiteralPath $archive -Force
    }
    [ordered]@{
        model = $modelName
        source = "ModelScope/AI-ModelScope/multilingual-e5-small"
        model_sha256 = $modelHash
        tokenizer_sha256 = $tokenizerHash
		runtime_provider = $runtimeProvider
        dimensions = 384
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $modelDir "manifest.json") -Encoding UTF8
    Write-Host "Installed memory embedding assets to $modelDir"
}

function Test-Embedding {
    $ready = (Test-Hash -Path (Join-Path $modelDir "model.onnx") -Expected $modelHash) -and
        (Test-Hash -Path (Join-Path $modelDir "tokenizer.json") -Expected $tokenizerHash) -and
        (Test-Path (Join-Path $modelDir "runtime\onnxruntime.dll"))
    if (-not $ready) { throw "Memory embedding assets are missing or invalid: $modelDir" }
    Write-Host "Memory embedding assets ready: $modelDir"
}

switch ($Action) {
    "install" { Install-Embedding }
    "status" { Test-Embedding }
    "uninstall" {
        Remove-Item -LiteralPath $modelDir -Recurse -Force -ErrorAction SilentlyContinue
        Write-Host "Removed memory embedding assets from $modelDir"
    }
}
