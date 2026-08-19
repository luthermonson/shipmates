#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
.SYNOPSIS
    Run the Go test suite (or a single package) under a memory watchdog that
    kills any runaway *.test.exe child before it can take the machine down.

.DESCRIPTION
    `go test ./...` spawns one child test binary (`<pkg>.test.exe`) per package.
    A test that allocates without bound blows up in that CHILD, not in the parent
    `go` process, so watching only `go` misses the runaway entirely. On 2026-08-16
    a `server.test.exe` reached ~1 TB of commit charge and the whole desktop died
    with E_OUTOFMEMORY; an earlier incident hit ~236 GB via an infinite
    filepath.Dir loop.

    This wrapper launches `go test`, then polls the commit charge
    (PagedMemorySize64) of every *.test.exe process descended from that `go`
    invocation (only ours -- sibling `go test` runs elsewhere are ignored). It
    records a per-package peak. If any child crosses the cap, it kills that child
    AND the parent `go` process tree, prints which package tripped and the tail of
    the output, and exits non-zero. A modest wall-clock timeout is enforced too.

.PARAMETER Target
    The package pattern to test. Default: ./...

.PARAMETER CapMB
    Per-test-process commit-charge cap in MB. Default: 2000. Healthy shipmates
    packages peak near ~100 MB, so a few hundred MB already signals trouble.

.PARAMETER TimeoutMinutes
    Wall-clock timeout for the whole run. Default: 15.

.PARAMETER PollSeconds
    How often to sample each test child. Default: 1.

.PARAMETER TailLines
    Lines of captured output to print on failure. Default: 40.

.PARAMETER GoTestArgs
    Any remaining arguments are passed through to `go test` (e.g. -run, -v).

.EXAMPLE
    pwsh scripts/safe-test.ps1

.EXAMPLE
    pwsh scripts/safe-test.ps1 -Target ./internal/permissions/... -CapMB 500

.EXAMPLE
    pwsh scripts/safe-test.ps1 -Target ./internal/server/... -- -run TestRingBuffer -v
#>
[CmdletBinding()]
param(
    [string]$Target = './...',
    [int]$CapMB = 2000,
    [int]$TimeoutMinutes = 15,
    [double]$PollSeconds = 1.0,
    [int]$TailLines = 40,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$GoTestArgs
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$capBytes = [int64]$CapMB * 1MB
$deadline = (Get-Date).AddMinutes($TimeoutMinutes)

# Capture combined output in the OS temp dir. Never inside the repo, so it can
# never be committed regardless of .gitignore.
$stamp   = [guid]::NewGuid().ToString('N')
$outFile = Join-Path ([System.IO.Path]::GetTempPath()) "shipmates-safe-test-$stamp.out"
$errFile = Join-Path ([System.IO.Path]::GetTempPath()) "shipmates-safe-test-$stamp.err"
New-Item -ItemType File -Path $outFile -Force | Out-Null
New-Item -ItemType File -Path $errFile -Force | Out-Null

# -count=1 defeats the test cache so a green run means the tests actually ran,
# matching CI.
$goArgs = @('test', '-count=1')
if ($GoTestArgs) { $goArgs += $GoTestArgs }
$goArgs += $Target

Write-Host "safe-test: go $($goArgs -join ' ')"
Write-Host "safe-test: cap=$CapMB MB  timeout=$TimeoutMinutes min  poll=$PollSeconds s"
Write-Host ("-" * 60)

$go = Start-Process -FilePath 'go' -ArgumentList $goArgs -PassThru -NoNewWindow `
    -RedirectStandardOutput $outFile -RedirectStandardError $errFile
$goPid = $go.Id
# Touch .Handle so the object caches the process handle; without this the
# Start-Process wrapper often returns $null from .ExitCode after the process ends.
$null = $go.Handle

# Live-tail the captured stdout so the user still sees progress while we poll.
$outReader = [System.IO.StreamReader]::new(
    [System.IO.FileStream]::new($outFile, [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite))

function Flush-Live {
    $chunk = $outReader.ReadToEnd()
    if ($chunk) { [Console]::Out.Write($chunk) }
}

# Collect the PIDs descended from $root (children, grandchildren, ...) so we only
# ever act on the process tree WE started -- a sibling agent's `go test` running
# concurrently must not be touched.
function Get-TreeSnapshot {
    $procs = Get-CimInstance Win32_Process -Property ProcessId, ParentProcessId, Name -ErrorAction SilentlyContinue
    $childrenOf = @{}
    $byId = @{}
    foreach ($p in $procs) {
        $id = [int]$p.ProcessId
        $pp = [int]$p.ParentProcessId
        $byId[$id] = $p
        if (-not $childrenOf.ContainsKey($pp)) {
            $childrenOf[$pp] = [System.Collections.Generic.List[int]]::new()
        }
        $childrenOf[$pp].Add($id)
    }
    return @{ ChildrenOf = $childrenOf; ById = $byId }
}

function Get-Descendants($snapshot, [int]$root) {
    $desc = [System.Collections.Generic.HashSet[int]]::new()
    $stack = [System.Collections.Generic.Stack[int]]::new()
    $stack.Push($root)
    while ($stack.Count -gt 0) {
        $cur = $stack.Pop()
        if ($snapshot.ChildrenOf.ContainsKey($cur)) {
            foreach ($c in $snapshot.ChildrenOf[$cur]) {
                if ($desc.Add($c)) { $stack.Push($c) }
            }
        }
    }
    return $desc
}

$peak     = @{}    # package name -> peak commit bytes
$tripped  = $null  # [pscustomobject] Package/Pid/Commit on breach
$timedOut = $false

try {
    while (-not $go.HasExited) {
        Flush-Live

        if ((Get-Date) -gt $deadline) { $timedOut = $true; break }

        $snap = Get-TreeSnapshot
        $desc = Get-Descendants $snap $goPid

        foreach ($id in $desc) {
            $proc = $snap.ById[$id]
            if ($null -eq $proc -or $proc.Name -notlike '*.test.exe') { continue }

            $gp = Get-Process -Id $id -ErrorAction SilentlyContinue
            if ($null -eq $gp) { continue }

            # PagedMemorySize64 is the process's committed (page-file-backed)
            # memory -- the commit charge, which is what the OOM was about. Working
            # set (physical RAM) would have stayed small while commit ran to a TB.
            $commit = [int64]$gp.PagedMemorySize64
            $pkg = $proc.Name -replace '\.test\.exe$', ''

            if (-not $peak.ContainsKey($pkg) -or $commit -gt $peak[$pkg]) {
                $peak[$pkg] = $commit
            }
            if ($commit -gt $capBytes) {
                $tripped = [pscustomobject]@{ Package = $pkg; Pid = $id; Commit = $commit }
                break
            }
        }

        if ($tripped) { break }
        Start-Sleep -Seconds $PollSeconds
    }

    if ($tripped -or $timedOut) {
        # Kill the offending child first, then the whole `go` tree. Done natively
        # with Stop-Process rather than taskkill: a native command's stderr (e.g.
        # "process not found" when go already exited) trips ErrorAction=Stop and
        # would abort before the report prints.
        if ($tripped) {
            Stop-Process -Id $tripped.Pid -Force -ErrorAction SilentlyContinue
        }
        $killSnap = Get-TreeSnapshot
        foreach ($id in (Get-Descendants $killSnap $goPid)) {
            Stop-Process -Id $id -Force -ErrorAction SilentlyContinue
        }
        Stop-Process -Id $goPid -Force -ErrorAction SilentlyContinue
        try { $go.WaitForExit(10000) | Out-Null } catch { }
    }
    else {
        $go.WaitForExit()
    }

    Flush-Live
}
finally {
    if ($outReader) { $outReader.Dispose() }
}

# Final flush of any stderr the panic/OOM left behind.
$errText = ''
if (Test-Path $errFile) { $errText = Get-Content -Raw -Path $errFile -ErrorAction SilentlyContinue }

$goExit = 0
try {
    $goExit = $go.ExitCode
    if ($null -eq $goExit) { $goExit = 0 }
} catch { $goExit = 0 }

Write-Host ""
Write-Host ("-" * 60)
Write-Host "safe-test: per-package peak commit charge"
if ($peak.Count -eq 0) {
    Write-Host "  (no *.test.exe child observed -- packages may have had no tests, or ran faster than one poll)"
}
else {
    foreach ($kv in ($peak.GetEnumerator() | Sort-Object Value -Descending)) {
        $mb = [math]::Round($kv.Value / 1MB, 1)
        Write-Host ("  {0,8:N1} MB  {1}" -f $mb, $kv.Key)
    }
}
Write-Host ("-" * 60)

# Capture the tail BEFORE deleting the temp files.
$tailLinesCaptured = @()
if (Test-Path $outFile) { $tailLinesCaptured += Get-Content -Path $outFile -Tail $TailLines -ErrorAction SilentlyContinue }
if ($errText) { $tailLinesCaptured += ($errText -split "`n" | Select-Object -Last $TailLines) }
$tailLinesCaptured = @($tailLinesCaptured | Select-Object -Last $TailLines)

function Show-Tail {
    Write-Host "safe-test: last $TailLines line(s) of output:"
    if ($tailLinesCaptured.Count -eq 0) {
        Write-Host "  | (no output captured)"
    }
    else {
        $tailLinesCaptured | ForEach-Object { Write-Host "  | $_" }
    }
}

# Clean up the temp capture files.
Remove-Item -Path $outFile, $errFile -Force -ErrorAction SilentlyContinue

if ($tripped) {
    $mb = [math]::Round($tripped.Commit / 1MB, 1)
    Write-Host ""
    Write-Host "WATCHDOG: package '$($tripped.Package)' (pid $($tripped.Pid)) crossed the $CapMB MB cap at $mb MB commit charge." -ForegroundColor Red
    Write-Host "WATCHDOG: killed it and the parent 'go' process. This is the runaway -- fix the test before re-running the full suite." -ForegroundColor Red
    Show-Tail
    exit 1
}
if ($timedOut) {
    Write-Host ""
    Write-Host "WATCHDOG: timed out after $TimeoutMinutes minute(s); killed the 'go' process tree." -ForegroundColor Red
    Show-Tail
    exit 1
}
if ($goExit -ne 0) {
    Write-Host ""
    Write-Host "safe-test: go test failed (exit $goExit)." -ForegroundColor Red
    Show-Tail
    exit $goExit
}

Write-Host ""
Write-Host "safe-test: clean pass." -ForegroundColor Green
exit 0
