#Requires -Version 5.1
<#
.SYNOPSIS
    Test for scripts/doctor (H10, issue #309).

.DESCRIPTION
    scripts/doctor is POSIX sh, invoked here through Git Bash's `sh` the same
    way it runs on this machine, in the dev image's busybox sh, and on a
    GitHub-hosted runner. Two of the ten checks describe failures a real
    Docker install will not produce on demand (an unwritable socket, a daemon
    that answers `docker info` but cannot start a container), so `docker` is
    faked through a PATH shim for those two - an extensionless sh script with
    a shebang, which this test proved resolves and runs correctly as `docker`
    under `sh` on Windows without a chmod (MSYS reports every file as
    executable regardless of any bit PowerShell can set). Everything else
    runs against the real Docker on this machine.

    The build-cache-volume test uses a random volume name via DOCTOR_VOLUME,
    never the real `inventory-go-build-cache` - every worktree on this
    machine shares that one as the Go build cache, and this test deletes what
    it creates.

    Run:  powershell -NoProfile -File scripts\tests\doctor.test.ps1
    Needs Docker (real, unshimmed, for the structural and --fix tests) and
    `sh` on PATH (Git for Windows). Takes a few seconds.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$script:failures = 0

function Assert {
    param([bool]$Condition, [string]$Message)
    if ($Condition) { Write-Host "  PASS  $Message" } else { Write-Host "  FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}

# docker, output discarded; Windows PowerShell 5.1 turns any stderr output of
# a native command into a terminating error under 'Stop'.
function Dk {
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & docker @args 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $previous }
}

$doctorScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'doctor'

# Run scripts/doctor with the given args and environment overrides, restoring
# the environment afterwards regardless of outcome.
function Invoke-Doctor {
    param([string[]]$DoctorArgs = @(), [hashtable]$Env = @{})
    $saved = @{}
    foreach ($k in $Env.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        Set-Item -Path "Env:$k" -Value $Env[$k]
    }
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try {
        $out = (& sh $doctorScript @DoctorArgs 2>&1 | Out-String)
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
        foreach ($k in $Env.Keys) {
            if ($null -eq $saved[$k]) { Remove-Item -Path "Env:$k" -ErrorAction SilentlyContinue } else { Set-Item -Path "Env:$k" -Value $saved[$k] }
        }
    }
    return [pscustomobject]@{ Out = $out; Exit = $code }
}

# A directory prepended to PATH holding a fake `docker`: an extensionless sh
# script, not a .cmd - `sh`'s own PATH lookup does not know PATHEXT, so a
# `docker.cmd` would never be found as `docker` from inside scripts/doctor.
function New-ShShim {
    param([string]$Name, [string]$Body)
    $dir = Join-Path $env:TEMP "doctorshim-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    [IO.File]::WriteAllText((Join-Path $dir $Name), ($Body -replace "`r`n", "`n"))
    return $dir
}
function Invoke-WithShim {
    param([string]$ShimDir, [scriptblock]$Body)
    $saved = $env:PATH
    $env:PATH = "$ShimDir;$saved"
    try { return & $Body } finally { $env:PATH = $saved; Remove-Item -Recurse -Force $ShimDir -ErrorAction SilentlyContinue }
}
$pathBefore = $env:PATH
function Test-PathRestored {
    param([string]$When)
    Assert ($env:PATH -eq $pathBefore) "PATH is exactly as it was $When (no shim outlives its block)"
}

try {
    Write-Host "Structural: --json lists all ten checks"
    $r = Invoke-Doctor -DoctorArgs @('--json')
    $parsed = $r.Out | ConvertFrom-Json
    Assert ($parsed.Count -eq 10) "exactly ten checks, got $($parsed.Count)"
    $shapeOk = $true
    foreach ($c in $parsed) {
        if (-not $c.PSObject.Properties['check'] -or -not $c.PSObject.Properties['status'] -or -not $c.PSObject.Properties['remediation']) { $shapeOk = $false }
        if (@('ok', 'warn', 'FAIL') -notcontains $c.status) { $shapeOk = $false }
    }
    Assert $shapeOk "every check has check/status/remediation, and status is ok, warn or FAIL"

    Write-Host "Human output: ten numbered lines"
    $r2 = Invoke-Doctor
    $numbered = @($r2.Out -split "`r?`n" | Where-Object { $_ -match '^\s*\d+\.' })
    Assert ($numbered.Count -eq 10) "ten numbered check lines in the human-readable output, got $($numbered.Count)"

    Write-Host "Socket unwritable (#309 acceptance: PATH-shim stub)"
    $sockDir = Join-Path $env:TEMP "doctorsock-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $sockDir | Out-Null
    $sockPath = Join-Path $sockDir 'docker.sock'
    Set-Content -Path $sockPath -Value 'x' -Encoding ASCII
    (Get-Item $sockPath).IsReadOnly = $true
    $infoFailsShim = New-ShShim -Name 'docker' -Body @"
#!/bin/sh
case "`$1" in
  info) exit 1 ;;
  *) exit 0 ;;
esac
"@
    $r = Invoke-WithShim $infoFailsShim { Invoke-Doctor -Env @{ DOCTOR_SOCKET = $sockPath } }
    Assert ($r.Exit -eq 1) "an unwritable socket: scripts/doctor exits 1, got $($r.Exit)"
    Assert ($r.Out -match "add the user to the socket's group") "check 1 gives the group remediation, not the generic 'daemon not running' one"
    Assert ($r.Out -notmatch 'docker info failed - start Docker Desktop') "the generic remediation is NOT what fires when the socket is the reason"
    (Get-Item $sockPath).IsReadOnly = $false
    Remove-Item -Recurse -Force $sockDir
    Test-PathRestored 'after the socket-unwritable shim'

    Write-Host "Daemon cannot run containers (#309 acceptance: PATH-shim stub)"
    $cannotRunShim = New-ShShim -Name 'docker' -Body @"
#!/bin/sh
case "`$1" in
  info) exit 0 ;;
  image) exit 1 ;;
  run) exit 1 ;;
  *) exit 0 ;;
esac
"@
    $r = Invoke-WithShim $cannotRunShim { Invoke-Doctor }
    Assert ($r.Exit -eq 1) "a daemon that cannot start containers: scripts/doctor exits 1, got $($r.Exit)"
    Assert ($r.Out -match 'container start\s+FAIL') "check 3 (container start) is FAIL"
    Assert ($r.Out -match 'answers docker info but cannot start containers') "check 3 carries its own message, distinct from check 1's"
    Assert ($r.Out -notmatch 'docker daemon\s+FAIL') "check 1 (docker daemon) is NOT the one blamed - docker info itself succeeded"
    Test-PathRestored 'after the cannot-run-containers shim'

    Write-Host "--fix creates the build-cache volume (an isolated one, never the real inventory-go-build-cache)"
    $testVolume = "doctor-test-$([guid]::NewGuid().ToString('N').Substring(0, 12))"
    Dk volume rm $testVolume | Out-Null
    Assert ((Dk volume inspect $testVolume) -ne 0) "the random test volume does not exist yet"
    try {
        # Indexed by position (check 4 of 10, fixed order - see scripts/doctor's
        # own check sequence), not filtered by name: a Where-Object filter here
        # intermittently came back as a one-element array whose .status then
        # projected as an array too (a Windows PowerShell 5.1 pipeline quirk),
        # which made `-eq` return an array instead of a bool and crashed Assert's
        # [bool] parameter binding. Indexing sidesteps it entirely.
        $r = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_VOLUME = $testVolume }
        $before = [array]($r.Out | ConvertFrom-Json)
        Assert ($before[3].check -eq 'build cache volume') "check 4 is still 'build cache volume' (index assumption holds)"
        Assert ([string]$before[3].status -eq 'FAIL') "without --fix and the volume absent, the check is FAIL, got $($before[3].status)"

        $r = Invoke-Doctor -DoctorArgs @('--fix', '--json') -Env @{ DOCTOR_VOLUME = $testVolume }
        Assert ((Dk volume inspect $testVolume) -eq 0) "--fix actually created the volume"
        $after = [array]($r.Out | ConvertFrom-Json)
        Assert ([string]$after[3].status -eq 'ok') "with --fix, the same run reports ok, got $($after[3].status)"

        $r = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_VOLUME = $testVolume }
        $rerun = [array]($r.Out | ConvertFrom-Json)
        Assert ([string]$rerun[3].status -eq 'ok') "a re-run (no --fix needed this time) is ok, got $($rerun[3].status)"
    } finally {
        Dk volume rm $testVolume | Out-Null
    }

    Write-Host "No .env value ever appears in the output (#309 acceptance)"
    $envDir = Join-Path $env:TEMP "doctorenv-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $envDir | Out-Null
    $envFile = Join-Path $envDir '.env'
    $marker = "MARKER_$([guid]::NewGuid().ToString('N'))"
    Set-Content -Path $envFile -Encoding ASCII -Value @"
HTTP_PORT=18123
TRAEFIK_PORT=19123
GEMINI_API_KEY=$marker
SERPAPI_KEY=also-$marker
"@
    $r = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_ENV_FILE = $envFile }
    Assert ($r.Out -notmatch [regex]::Escape($marker)) "the marker secret value from .env never appears in --json output"
    $r2 = Invoke-Doctor -Env @{ DOCTOR_ENV_FILE = $envFile }
    Assert ($r2.Out -notmatch [regex]::Escape($marker)) "the marker secret value from .env never appears in human-readable output either"
    Assert ($r2.Out -match '18123' -and $r2.Out -match '19123') "HTTP_PORT/TRAEFIK_PORT VALUES do appear - reading and displaying them is the point of the check, not a leak"
    Remove-Item -Recurse -Force $envDir
}
finally {
    Write-Host "Cleaning up"
}

if ($script:failures -gt 0) { Write-Host "`n$($script:failures) assertion(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "`nAll assertions passed."
