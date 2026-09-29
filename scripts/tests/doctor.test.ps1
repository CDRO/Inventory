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
    Assert ($r.Exit -eq 0) "a clean run on this machine (all ok/documented warn) exits 0, got $($r.Exit) (acceptance criterion #1)"
    $parsed = $r.Out | ConvertFrom-Json
    Assert ($parsed.Count -eq 10) "exactly ten checks, got $($parsed.Count)"
    $shapeOk = $true
    $remediationContractOk = $true
    foreach ($c in $parsed) {
        if (-not $c.PSObject.Properties['check'] -or -not $c.PSObject.Properties['status'] -or -not $c.PSObject.Properties['remediation']) { $shapeOk = $false }
        if (@('ok', 'warn', 'FAIL') -notcontains $c.status) { $shapeOk = $false }
        if ($c.status -eq 'ok' -and [string]$c.remediation -ne '') { $remediationContractOk = $false }
    }
    Assert $shapeOk "every check has check/status/remediation, and status is ok, warn or FAIL"
    Assert $remediationContractOk "every 'ok' check carries remediation \"\" (the documented --json contract, scripts/doctor's own header)"

    Write-Host "Human output: ten numbered lines"
    $r2 = Invoke-Doctor
    Assert ($r2.Exit -eq 0) "the human-readable run also exits 0 on this machine, got $($r2.Exit)"
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
    # Both invocations share one shim block - Invoke-WithShim deletes its
    # directory in its own `finally`, so a second call reusing an already-
    # unwound shim would silently fall through to the REAL docker on PATH
    # instead (this was caught by the --json assertions below coming back
    # against a healthy real daemon: status "ok", not "FAIL").
    Invoke-WithShim $infoFailsShim {
        $r = Invoke-Doctor -Env @{ DOCTOR_SOCKET = $sockPath }
        Assert ($r.Exit -eq 1) "an unwritable socket: scripts/doctor exits 1, got $($r.Exit)"
        Assert ($r.Out -match "add the user to the socket's group") "check 1 gives the group remediation, not the generic 'daemon not running' one"
        Assert ($r.Out -notmatch 'docker info failed - start Docker Desktop') "the generic remediation is NOT what fires when the socket is the reason"

        # Same scenario through --json: $sockPath is a real Windows path,
        # backslash-heavy (e.g. C:\Users\...\docker.sock), embedded verbatim in
        # this check's remediation - the one case in the whole suite that puts
        # a backslash through json_escape. If escaping were broken (or
        # deleted), this is where it would show: ConvertFrom-Json would throw
        # or come back garbled, never silently pass like the backslash-free
        # fixtures elsewhere in this file do.
        $rj = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_SOCKET = $sockPath }
        $parsedShim = [array]($rj.Out | ConvertFrom-Json)
        Assert ($parsedShim.Count -eq 10) "the --json output is still valid, parseable JSON with a backslash-heavy path embedded in it"
        Assert ($parsedShim[0].status -eq 'FAIL') "check 1 is FAIL in the --json output too"
        Assert ($parsedShim[0].remediation.Contains($sockPath)) "the backslash-heavy socket path round-trips through json_escape intact: $($parsedShim[0].remediation)"
    } | Out-Null

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

    Write-Host "Compose version boundary (#309 acceptance: spec 01's Compose >= 2.24)"
    foreach ($case in @(
            @{ Version = '2.20.0'; Expect = 'FAIL'; What = 'below the floor (same major, minor < 24)' },
            @{ Version = '2.24.0'; Expect = 'ok'; What = 'exactly at the floor' },
            @{ Version = '3.1.0'; Expect = 'ok'; What = 'a newer major' }
        )) {
        # This machine has a real standalone docker-compose (Docker Desktop
        # ships one) that meets the floor on its own - a shim covering only
        # `docker` would let that real binary confirm "ok" underneath a
        # deliberately-stale plugin version and defeat the "below the floor"
        # case. Shimming docker-compose too, always failing, isolates the
        # plugin path these three cases mean to exercise; the mixed-source
        # scenario right after this loop is what tests both paths together.
        $dir = Join-Path $env:TEMP "doctorshim-$([guid]::NewGuid().ToString('N'))"
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        [IO.File]::WriteAllText((Join-Path $dir 'docker'), (@"
#!/bin/sh
case "`$*" in
  "compose version --short") echo "$($case.Version)" ;;
  info) exit 0 ;;
  *) exit 0 ;;
esac
"@ -replace "`r`n", "`n"))
        [IO.File]::WriteAllText((Join-Path $dir 'docker-compose'), (@"
#!/bin/sh
exit 1
"@ -replace "`r`n", "`n"))
        $r = Invoke-WithShim $dir { Invoke-Doctor -DoctorArgs @('--json') }
        $parsed = [array]($r.Out | ConvertFrom-Json)
        Assert ($parsed[1].check -eq 'compose version') "check 2 is still 'compose version' (index assumption holds)"
        Assert ([string]$parsed[1].status -eq $case.Expect) "compose $($case.Version) ($($case.What)): expected $($case.Expect), got $($parsed[1].status)"
    }
    Test-PathRestored 'after the compose-version shims'

    Write-Host "Compose version: a stale bundled plugin does not shadow a newer standalone docker-compose (the Synology Container Manager case, #309 review finding)"
    $mixedDir = Join-Path $env:TEMP "doctorshim-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $mixedDir | Out-Null
    [IO.File]::WriteAllText((Join-Path $mixedDir 'docker'), (@"
#!/bin/sh
case "`$*" in
  "compose version --short") echo "2.20.1" ;;
  info) exit 0 ;;
  *) exit 0 ;;
esac
"@ -replace "`r`n", "`n"))
    [IO.File]::WriteAllText((Join-Path $mixedDir 'docker-compose'), (@"
#!/bin/sh
case "`$*" in
  "version --short") echo "2.31.0" ;;
  version) exit 0 ;;
  *) exit 0 ;;
esac
"@ -replace "`r`n", "`n"))
    $r = Invoke-WithShim $mixedDir { Invoke-Doctor -DoctorArgs @('--json') }
    $parsed = [array]($r.Out | ConvertFrom-Json)
    Assert ([string]$parsed[1].status -eq 'ok') "a stale bundled plugin (2.20.1) alongside a newer standalone docker-compose (2.31.0) is ok, got $($parsed[1].status)"

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
    $portsParsed = [array]($r.Out | ConvertFrom-Json)
    Assert ($portsParsed[4].check -like 'ports (*') "check 5 is still the ports check (index assumption holds)"
    Assert ([string]$portsParsed[4].status -eq 'ok') "the two free fixture ports (18123/19123) actually got checked and reported ok, got $($portsParsed[4].status)"
    $r2 = Invoke-Doctor -Env @{ DOCTOR_ENV_FILE = $envFile }
    Assert ($r2.Out -notmatch [regex]::Escape($marker)) "the marker secret value from .env never appears in human-readable output either"
    Assert ($r2.Out -match '18123' -and $r2.Out -match '19123') "HTTP_PORT/TRAEFIK_PORT VALUES do appear - reading and displaying them is the point of the check, not a leak"
    Remove-Item -Recurse -Force $envDir

    Write-Host "check 5 does not FAIL a port already published by the operator's own compose stack (#342)"
    $portEnvDir = Join-Path $env:TEMP "doctorports-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $portEnvDir | Out-Null
    $portEnvFile = Join-Path $portEnvDir '.env'
    Set-Content -Path $portEnvFile -Encoding ASCII -Value @"
HTTP_PORT=27391
TRAEFIK_PORT=27392
"@
    # `docker run -p` always fails (simulating both ports bound), but
    # `docker compose ps` reports only HTTP_PORT as published by a running
    # service - the project's own stack. TRAEFIK_PORT has no such match, so
    # it must still be reported as a genuine conflict.
    $portShimDir = New-ShShim -Name 'docker' -Body @"
#!/bin/sh
case "`$1" in
  info) exit 0 ;;
  run) exit 1 ;;
  compose)
    if [ "`$2" = "ps" ]; then
      printf 'NAME IMAGE COMMAND SERVICE CREATED STATUS PORTS\n'
      printf 'proj-app-1 x x app x Up 0.0.0.0:27391->27391/tcp\n'
      exit 0
    fi
    exit 0 ;;
  *) exit 0 ;;
esac
"@
    $r = Invoke-WithShim $portShimDir { Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_ENV_FILE = $portEnvFile } }
    $parsedPorts = [array]($r.Out | ConvertFrom-Json)
    Assert ($parsedPorts[4].check -like 'ports (*') "check 5 is still the ports check (index assumption holds)"
    Assert ([string]$parsedPorts[4].status -eq 'FAIL') "a port with no matching compose service still FAILs, got $($parsedPorts[4].status)"
    Assert ($parsedPorts[4].remediation -match 'TRAEFIK_PORT=27392') "the FAIL names the genuinely unexplained port (TRAEFIK_PORT)"
    Assert ($parsedPorts[4].remediation -notmatch 'HTTP_PORT=27391') "the port already published by the operator's own compose stack is NOT reported as a conflict"
    Test-PathRestored 'after the own-stack port shim'
    Remove-Item -Recurse -Force $portEnvDir -ErrorAction SilentlyContinue

    Write-Host "DOCTOR_ROOT drives check 7 against a synthetic CRLF fixture, not this repo's own tree (#342)"
    $rootFixture = Join-Path $env:TEMP "doctorroot-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $rootFixture | Out-Null
    Set-Content -Path (Join-Path $rootFixture '.gitattributes') -Encoding ASCII -Value "sample.sh text eol=lf"
    $sampleFixture = Join-Path $rootFixture 'sample.sh'
    try {
        [IO.File]::WriteAllText($sampleFixture, "#!/bin/sh`r`necho hi`r`n")
        $r = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_ROOT = $rootFixture }
        $parsedRoot = [array]($r.Out | ConvertFrom-Json)
        Assert ($parsedRoot[6].check -eq 'git identity + line endings') "check 7 is still index 6 (index assumption holds)"
        Assert ([string]$parsedRoot[6].status -eq 'FAIL') "a synthetic CRLF fixture under DOCTOR_ROOT is caught as FAIL, got $($parsedRoot[6].status)"
        Assert ($parsedRoot[6].remediation -match 'sample\.sh') "the FAIL remediation names the offending fixture file: $($parsedRoot[6].remediation)"

        # Fix the fixture to real LF endings under the same override - proves
        # the check actually reads DOCTOR_ROOT rather than coincidentally
        # reporting FAIL for an unrelated reason.
        [IO.File]::WriteAllText($sampleFixture, "#!/bin/sh`necho hi`n")
        $r2 = Invoke-Doctor -DoctorArgs @('--json') -Env @{ DOCTOR_ROOT = $rootFixture }
        $parsedRoot2 = [array]($r2.Out | ConvertFrom-Json)
        Assert ([string]$parsedRoot2[6].status -eq 'ok') "the same fixture with real LF endings reports ok, got $($parsedRoot2[6].status)"

        # No override: check 7 must still read this repository's own,
        # LF-clean tree - the fixture must never leak into the default path.
        $r3 = Invoke-Doctor -DoctorArgs @('--json')
        $parsedRoot3 = [array]($r3.Out | ConvertFrom-Json)
        Assert ([string]$parsedRoot3[6].status -eq 'ok') "with no DOCTOR_ROOT override, check 7 reads this repo's own tree and reports ok, got $($parsedRoot3[6].status)"
    } finally {
        Remove-Item -Recurse -Force $rootFixture -ErrorAction SilentlyContinue
    }

    Write-Host "json_escape escapes embedded newline and tab, not just backslash/quote (#342)"
    $normalizedDoctor = (Get-Content -Raw $doctorScript) -replace "`r`n", "`n"
    $funcMatch = [regex]::Match($normalizedDoctor, '(?ms)^json_escape\(\) \{.*?\n\}')
    Assert $funcMatch.Success "json_escape() function extracted from scripts/doctor for direct testing"
    $escHarness = @'

input=$(printf 'a"b\\c\nd\te')
result=$(json_escape "$input")
printf '%s' "$result"
'@
    $escFixture = Join-Path $env:TEMP "jsonescape-$([guid]::NewGuid().ToString('N')).sh"
    [IO.File]::WriteAllText($escFixture, (($funcMatch.Value + $escHarness) -replace "`r`n", "`n"))
    $escOut = & sh $escFixture
    Assert ($escOut -eq 'a\"b\\c\nd\te') "json_escape round-trips backslash, quote, an embedded newline and an embedded tab into valid JSON escapes, got '$escOut'"
    $escJson = "{`"remediation`": `"$escOut`"}"
    $escParsed = $escJson | ConvertFrom-Json
    Assert ($escParsed.remediation -eq "a`"b\c`nd`te") "the escaped text parses back through ConvertFrom-Json to the original raw string"
    Remove-Item -Force $escFixture -ErrorAction SilentlyContinue
}
finally {
    Write-Host "Cleaning up"
}

if ($script:failures -gt 0) { Write-Host "`n$($script:failures) assertion(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "`nAll assertions passed."
