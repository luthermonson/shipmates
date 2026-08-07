param(
    [string]$Report = $env:SHIPMATES_TEST_REPORT
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($Report)) {
    $Report = Join-Path ([System.IO.Path]::GetTempPath()) "shipmates-fleet-autonomous.json"
}
$reportDir = Split-Path -Parent $Report
if ($reportDir) {
    New-Item -ItemType Directory -Force -Path $reportDir | Out-Null
}

Push-Location $repoRoot
try {
    & go test -json -count=1 -timeout=90s -run '^TestAutonomousFleet' ./internal/fleet |
        Tee-Object -FilePath $Report
    $testStatus = $LASTEXITCODE
} finally {
    Pop-Location
}

Write-Host "Fleet autonomous report: $Report"
exit $testStatus
