[CmdletBinding()]
param(
    [ValidateSet("preflight", "preflight-installed", "install", "status", "doctor", "uninstall")]
    [string]$Action = "install",
    [string]$AppRoot = "",
    [string]$Backend = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")
$paths = Get-LuminaPaths -AppRoot $AppRoot
$AppRoot = $paths.Root
$lockFile = $(if ($env:LUMINA_MEMORY_MODELS_LOCK) { $env:LUMINA_MEMORY_MODELS_LOCK } else { Join-Path $PSScriptRoot "memory-models.lock" })
$endpoint = $(if ($env:LUMINA_MODELSCOPE_ENDPOINT) { $env:LUMINA_MODELSCOPE_ENDPOINT.TrimEnd('/') } else { "https://modelscope.cn" })
$modelsRoot = Join-Path $paths.Cache "models"
$memoryRoot = Join-Path $modelsRoot "memory"
$stagingRoot = Join-Path $modelsRoot ".staging"
$bgeDir = Join-Path $memoryRoot "bge-m3"
$ortRelease = "1.26.0"
if (-not $Backend) { $Backend = Join-Path $paths.App "bin\lumina-backend.exe" }

function Read-ModelLock {
    $entries = @()
    foreach ($line in Get-Content -LiteralPath $lockFile) {
        $line = $line.Trim()
        if (-not $line -or $line.StartsWith("#")) { continue }
        $parts = $line.Split('|')
        if ($parts.Count -notin @(7, 8)) { throw "Invalid model lock line: $line" }
        $profile = $(if ($parts.Count -eq 8 -and $parts[7]) { $parts[7] } else { "common" })
        $entries += [pscustomobject]@{ Model=$parts[0]; Repository=$parts[1]; Revision=$parts[2]; Remote=$parts[3]; Local=$parts[4]; Size=[int64]$parts[5]; SHA=$parts[6]; Profile=$profile }
    }
    return $entries
}

function Test-LockedFile($Path, [int64]$Size, [string]$SHA) {
    return (Test-Path -LiteralPath $Path -PathType Leaf) -and
        ((Get-Item -LiteralPath $Path).Length -eq $Size) -and
        ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant() -eq $SHA)
}

function Get-ModelVariant {
    if ($env:LUMINA_MEMORY_MODEL_VARIANT) {
        $configured = $env:LUMINA_MEMORY_MODEL_VARIANT.ToLowerInvariant()
        if ($configured -notin @("cpu-int8", "accelerator-fp16")) { throw "Unsupported model variant: $configured" }
        return $configured
    }
    $device = $(if ($env:LUMINA_MEMORY_EMBEDDING_DEVICE) { $env:LUMINA_MEMORY_EMBEDDING_DEVICE.ToLowerInvariant() } else { "auto" })
    if ($device -eq "cpu") { return "cpu-int8" }
    if ($device -in @("cuda", "nvidia")) { return "accelerator-fp16" }
    if ($device -notin @("auto", "")) {
        throw "A managed ONNX Runtime is not available for explicit device '$device' on Windows"
    }
    if (Get-Command nvidia-smi -ErrorAction SilentlyContinue) { return "accelerator-fp16" }
    return "cpu-int8"
}

function Test-ModelEndpoint {
    $uri = [Uri]$endpoint
    if ($uri.Scheme -ne "https") {
        $isLoopback = $uri.IsLoopback
        if (-not $isLoopback -or $env:LUMINA_ALLOW_INSECURE_MODEL_ENDPOINT -ne "1") {
            throw "An HTTPS ModelScope endpoint is required."
        }
    }
}

function Get-ModelEntries([string]$Model, [string]$Variant) {
    return @(Read-ModelLock | Where-Object {
        $_.Model -eq $Model -and ($_.Profile -eq "common" -or $_.Profile -eq $Variant)
    })
}

function Test-LockedModel([string]$Model, [string]$Root, [string]$Variant) {
    foreach ($entry in (Get-ModelEntries $Model $Variant)) {
        if (-not (Test-LockedFile (Join-Path $Root $entry.Local) $entry.Size $entry.SHA)) { return $false }
    }
    return $true
}

function Test-ModelManifest([string]$Model, [string]$Root, [string]$Variant) {
    $entry = Read-ModelLock | Where-Object Model -eq $Model | Select-Object -First 1
    $variantEntry = Read-ModelLock | Where-Object { $_.Model -eq $Model -and $_.Profile -eq $Variant } | Select-Object -First 1
    if (-not $variantEntry) { $variantEntry = $entry }
    $path = Join-Path $Root "manifest.json"
    if (-not $entry -or -not (Test-Path -LiteralPath $path -PathType Leaf)) { return $false }
    try { $manifest = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json }
    catch { return $false }
    return ($manifest.model -eq $Model) -and ($manifest.repository -eq $entry.Repository) -and
        ($manifest.revision -eq $entry.Revision) -and ($manifest.variant -eq $Variant) -and
        ($manifest.variant_repository -eq $variantEntry.Repository) -and
        ($manifest.variant_revision -eq $variantEntry.Revision)
}

function Test-BGEHeads([string]$Root) {
    & $Backend models verify-bge-heads --model-dir $Root *> $null
    return $LASTEXITCODE -eq 0
}

function Receive-LockedModel([string]$Model, [string]$Root, [string]$Variant) {
    foreach ($entry in (Get-ModelEntries $Model $Variant)) {
        $target = Join-Path $Root $entry.Local
        if (Test-LockedFile $target $entry.Size $entry.SHA) { continue }
        New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
        $partial = "$target.partial"
        if ((Test-Path $partial) -and ((Get-Item $partial).Length -gt $entry.Size)) { Remove-Item $partial -Force }
        $uri = "$endpoint/models/$($entry.Repository)/resolve/$($entry.Revision)/$($entry.Remote)"
        Write-Host "Downloading $uri ($($entry.Size) bytes)"
        & curl.exe -fL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 20 -C - -o $partial $uri
        if ($LASTEXITCODE -ne 0) { throw "ModelScope download failed: $uri" }
        if (-not (Test-LockedFile $partial $entry.Size $entry.SHA)) { throw "Checksum or size mismatch: $uri" }
        Move-Item -LiteralPath $partial -Destination $target -Force
    }
}

function Install-Runtime([string]$Root, [string]$Variant) {
    $device = $(if ($env:LUMINA_MEMORY_EMBEDDING_DEVICE) { $env:LUMINA_MEMORY_EMBEDDING_DEVICE.ToLowerInvariant() } else { "auto" })
    $useCuda = ($Variant -eq "accelerator-fp16") -and
        (($device -in @("cuda", "nvidia")) -or (($device -eq "auto") -and (Get-Command nvidia-smi -ErrorAction SilentlyContinue)))
    $archiveName = $(if ($useCuda) { "onnxruntime-win-x64-gpu-$ortRelease.zip" } else { "onnxruntime-win-x64-$ortRelease.zip" })
    $archiveSHA = $(if ($useCuda) { "1133b1bcb0fb6f82b1c5b470b7cc15f9080a58b27dbc7b579a1fd63125ec2a15" } else { "6ebe99b5564bf4d029b6e93eac9ff423682b6212eade769e9ca3f685eaf500b4" })
    $provider = $(if ($useCuda) { "cuda" } else { "cpu" })
    $runtimeDir = Join-Path $Root "runtime"
    $providerPath = Join-Path $runtimeDir "provider"
    if ((Test-Path (Join-Path $runtimeDir "onnxruntime.dll")) -and (Test-Path $providerPath) -and
        ((Get-Content $providerPath -Raw).Trim() -eq $provider)) { return }
    $archive = Join-Path $Root $archiveName
    & curl.exe -fL --retry 5 --retry-all-errors --retry-delay 2 --connect-timeout 20 -C - -o $archive "https://github.com/microsoft/onnxruntime/releases/download/v$ortRelease/$archiveName"
    if ($LASTEXITCODE -ne 0 -or (Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $archiveSHA) { throw "ONNX Runtime download failed" }
    $extract = Join-Path $Root ".runtime-extract"
    Remove-Item $extract -Recurse -Force -ErrorAction SilentlyContinue
    Expand-Archive -LiteralPath $archive -DestinationPath $extract -Force
    New-Item -ItemType Directory -Path $runtimeDir -Force | Out-Null
    $source = Get-ChildItem $extract -Recurse -Filter "onnxruntime.dll" | Select-Object -First 1
    if (-not $source) { throw "onnxruntime.dll was not found" }
    Get-ChildItem $source.DirectoryName -Filter "onnxruntime*.dll" | Copy-Item -Destination $runtimeDir -Force
    Set-Content -LiteralPath (Join-Path $runtimeDir "provider") -Value $provider -Encoding ASCII
    Remove-Item $extract -Recurse -Force
    Remove-Item $archive -Force
}

function Test-ModelAssets([string]$Model, [string]$Root, [string]$Variant) {
	$ready = Test-LockedModel $Model $Root $Variant
	if ($ready) {
		$ready = (Test-Path (Join-Path $Root "runtime\onnxruntime.dll")) -and
			(Test-Path (Join-Path $Root "runtime\provider")) -and (Test-BGEHeads $Root)
	}
	return $ready
}

function Prepare-Model([string]$Model, [string]$Final, [string]$Variant) {
	if ((Test-ModelAssets $Model $Final $Variant) -and (Test-ModelManifest $Model $Final $Variant)) {
        Write-Host "$Model ($Variant) is already verified at $Final"
        return [pscustomobject]@{ Model=$Model; Final=$Final; Stage=$null; NeedsPublish=$false }
    }
    $entry = Read-ModelLock | Where-Object Model -eq $Model | Select-Object -First 1
    $stage = Join-Path $stagingRoot "$Model-$($entry.Revision)"
	if (Test-LockedModel $Model $Final $Variant) {
		Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
		New-Item -ItemType Directory -Path $stage -Force | Out-Null
		Copy-Item (Join-Path $Final "*") $stage -Recurse -Force
	} else {
		New-Item -ItemType Directory -Path $stage -Force | Out-Null
	}
	if (-not (Test-LockedModel $Model $stage $Variant)) {
		Receive-LockedModel $Model $stage $Variant
	}
    Remove-Item (Join-Path $stage "onnx\model.onnx_data") -Force -ErrorAction SilentlyContinue
	Install-Runtime $stage $Variant
	& $Backend models prepare-bge-heads --model-dir $stage
	if ($LASTEXITCODE -ne 0) { throw "Could not prepare BGE-M3 heads" }
    $variantEntry = Read-ModelLock | Where-Object { $_.Model -eq $Model -and $_.Profile -eq $Variant } | Select-Object -First 1
    if (-not $variantEntry) { $variantEntry = $entry }
    [ordered]@{ model=$Model; repository=$entry.Repository; revision=$entry.Revision; variant=$Variant;
        variant_repository=$variantEntry.Repository; variant_revision=$variantEntry.Revision;
        endpoint=$endpoint; lock="memory-models.lock" } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $stage "manifest.json") -Encoding UTF8
    if (-not (Test-ModelAssets $Model $stage $Variant) -or -not (Test-ModelManifest $Model $stage $Variant)) {
		throw "$Model staging validation failed"
	}
	return [pscustomobject]@{ Model=$Model; Final=$Final; Stage=$stage; NeedsPublish=$true }
}

function Publish-PreparedModels([array]$Prepared) {
	$publish = @($Prepared | Where-Object NeedsPublish)
	$published = @()
	$backups = @{}
	$lockStage = Join-Path $stagingRoot "models.lock.new"
	$lockBackup = Join-Path $stagingRoot "models.lock.previous"
	Copy-Item $lockFile $lockStage -Force
	try {
		foreach ($item in $publish) {
			$backup = Join-Path $stagingRoot "$($item.Model).previous"
			Remove-Item $backup -Recurse -Force -ErrorAction SilentlyContinue
			if (Test-Path $item.Final) {
				Move-Item $item.Final $backup
				$backups[$item.Model] = $backup
			}
		}
		foreach ($item in $publish) {
			Move-Item $item.Stage $item.Final
			$published += $item
		}
		$installedLock = Join-Path $memoryRoot "models.lock"
		Remove-Item $lockBackup -Force -ErrorAction SilentlyContinue
		if (Test-Path $installedLock) { Move-Item $installedLock $lockBackup }
		Move-Item $lockStage $installedLock
	} catch {
		foreach ($item in $published) { Remove-Item $item.Final -Recurse -Force -ErrorAction SilentlyContinue }
		foreach ($item in $publish) {
			if ($backups.ContainsKey($item.Model)) { Move-Item $backups[$item.Model] $item.Final -Force }
		}
		$installedLock = Join-Path $memoryRoot "models.lock"
		Remove-Item $installedLock -Force -ErrorAction SilentlyContinue
		if (Test-Path $lockBackup) { Move-Item $lockBackup $installedLock }
		throw
	}
	foreach ($backup in $backups.Values) { Remove-Item $backup -Recurse -Force -ErrorAction SilentlyContinue }
	Remove-Item $lockBackup -Force -ErrorAction SilentlyContinue
}

function Install-Models {
    if (-not [Environment]::Is64BitOperatingSystem) { throw "Memory models require 64-bit Windows" }
    if (-not (Test-Path $Backend)) { throw "Built backend is required: $Backend" }
    New-Item -ItemType Directory -Path $memoryRoot,$stagingRoot -Force | Out-Null
	$drive = Get-PSDrive -Name ([IO.Path]::GetPathRoot($modelsRoot).Substring(0,1))
	if ($drive.Free -lt 5GB) { throw "At least 5 GiB free space is required under $modelsRoot" }
    $variant = Get-ModelVariant
    Write-Host "Selected BGE-M3 ONNX profile: $variant"
	$prepared = @(
		Prepare-Model "bge-m3" $bgeDir $variant
	)
	Publish-PreparedModels $prepared
	Remove-Item (Join-Path $memoryRoot "multilingual-e5-small") -Recurse -Force -ErrorAction SilentlyContinue
	Set-LuminaPrivateAcl -Path @($modelsRoot, $memoryRoot, $bgeDir)
}

function Test-PreinstalledModelFiles([string]$Variant) {
    if (-not (Test-LockedModel "bge-m3" $bgeDir $Variant)) { return $false }
    if (-not (Test-ModelManifest "bge-m3" $bgeDir $Variant)) { return $false }
    $providerPath = Join-Path $bgeDir "runtime\provider"
    $runtimePath = Join-Path $bgeDir "runtime\onnxruntime.dll"
    if (-not (Test-Path $providerPath) -or -not (Test-Path $runtimePath)) { return $false }
    $provider = (Get-Content $providerPath -Raw).Trim()
    $expected = $(if ($Variant -eq "accelerator-fp16") { "cuda" } else { "cpu" })
    return $provider -eq $expected
}

function Invoke-ModelPreflight([switch]$RequireInstalled) {
    if (-not [Environment]::Is64BitOperatingSystem) { throw "Memory models require 64-bit Windows" }
    if (-not (Test-Path $lockFile -PathType Leaf)) { throw "Model lock is missing: $lockFile" }
    Test-ModelEndpoint
    $variant = Get-ModelVariant
    if (-not @(Get-ModelEntries "bge-m3" $variant)) {
        throw "Model lock has no BGE-M3 assets for profile $variant"
    }
    $provider = $(if ($variant -eq "accelerator-fp16") { "cuda" } else { "cpu" })
    if (Test-PreinstalledModelFiles $variant) {
        Write-Host "  memory model: BGE-M3 profile=$variant provider=$provider (verified local assets)"
        return
    }
    if ($RequireInstalled) {
        throw "SKIP_MEMORY_MODELS=1 requires verified preinstalled BGE-M3 assets for $variant"
    }
    $probe = $modelsRoot
    while (-not (Test-Path $probe -PathType Container)) {
        $parent = Split-Path -Parent $probe
        if (-not $parent -or $parent -eq $probe) { break }
        $probe = $parent
    }
    $drive = Get-PSDrive -Name ([IO.Path]::GetPathRoot($probe).Substring(0,1))
    if ($drive.Free -lt 5GB) { throw "At least 5 GiB free space is required under $modelsRoot" }
    if ($env:LUMINA_INSTALL_PREFLIGHT_OFFLINE -ne "1") {
        try {
            Invoke-WebRequest -Uri "$endpoint/" -Method Head -TimeoutSec 20 -UseBasicParsing | Out-Null
        } catch {
            throw "ModelScope endpoint is unreachable: $endpoint"
        }
    }
    Write-Host "  memory model: BGE-M3 profile=$variant provider=$provider (download required)"
}

function Test-Models {
    $variant = Get-ModelVariant
	if (-not (Test-LockedModel "bge-m3" $bgeDir $variant)) { throw "BGE-M3 assets are invalid" }
	if (-not (Test-ModelManifest "bge-m3" $bgeDir $variant)) { throw "BGE-M3 manifest is invalid" }
	if (-not (Test-Path (Join-Path $bgeDir "runtime\onnxruntime.dll"))) { throw "BGE-M3 ONNX Runtime is missing" }
    & $Backend models verify-bge-heads --model-dir $bgeDir
    if ($LASTEXITCODE -ne 0) { throw "BGE-M3 heads are invalid" }
	& $Backend models probe-bge --model-dir $bgeDir
	if ($LASTEXITCODE -ne 0) { throw "BGE-M3 inference probe failed" }
    Write-Host "Memory model profile $variant, manifest, linear heads, and inference probe are valid"
}

switch ($Action) {
    "preflight" { Invoke-ModelPreflight }
    "preflight-installed" { Invoke-ModelPreflight -RequireInstalled }
    "install" { Install-Models }
    "status" { Test-Models }
    "doctor" { Test-Models }
    "uninstall" { Remove-Item $memoryRoot,$stagingRoot -Recurse -Force -ErrorAction SilentlyContinue }
}
