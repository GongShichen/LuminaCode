$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "app-paths.ps1")

function Assert-Equal([string]$Actual, [string]$Expected) {
    if ($Actual -ne $Expected) { throw "Expected '$Expected', got '$Actual'" }
}

$savedLocalAppData = $env:LOCALAPPDATA
$savedUserProfile = $env:USERPROFILE
$savedOverride = $env:LUMINA_APP_ROOT
try {
    $fixturePath = Join-Path (Split-Path -Parent $PSScriptRoot) "testdata\app-path-contract.tsv"
    foreach ($line in (Get-Content -LiteralPath $fixturePath)) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith("#")) { continue }
        $fields = $line.Split('|')
        if ($fields.Count -ne 6 -or $fields[1] -ne "windows") { continue }
        $homeValue = $(if ($fields[2] -eq "-") { "" } else { $fields[2] })
        $localValue = $(if ($fields[3] -eq "-") { "" } else { $fields[3] })
        $overrideValue = $(if ($fields[4] -eq "-") { "" } else { $fields[4] })
        $env:USERPROFILE = $homeValue
        $env:LOCALAPPDATA = $localValue
        $env:LUMINA_APP_ROOT = ""
        Assert-Equal (Resolve-LuminaAppRoot -Override $overrideValue) $fields[5]
    }

    $env:LUMINA_APP_ROOT = ""
    $env:LOCALAPPDATA = "C:\Users\Tester\AppData\Local"
    $env:USERPROFILE = "C:\Users\Tester"
    Assert-Equal (Resolve-LuminaAppRoot) "C:\Users\Tester\AppData\Local\LuminaCode"
    $env:LOCALAPPDATA = ""
    Assert-Equal (Resolve-LuminaAppRoot) "C:\Users\Tester\.lumina"
    Assert-Equal (Resolve-LuminaAppRoot -Override "D:\Lumina Root") "D:\Lumina Root"
    try { Resolve-LuminaAppRoot -Override "relative" | Out-Null; throw "Relative root was accepted" } catch {
        if ($_.Exception.Message -eq "Relative root was accepted") { throw }
    }
} finally {
    $env:LOCALAPPDATA = $savedLocalAppData
    $env:USERPROFILE = $savedUserProfile
    $env:LUMINA_APP_ROOT = $savedOverride
}
