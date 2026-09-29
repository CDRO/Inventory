#Requires -Version 5.1
<#
.SYNOPSIS
    Tests for the Docker-facing pieces of wellen-orchestrator.ps1:
    Get-SanitizedProjectName / Set-WorktreeEnvOverrides (#115),
    Stop-PackageStack, Invoke-WaveDockerCleanup and the wave file's
    "dockerCleanup" validation (#140 item 9) - plus Invoke-Wave's parallel
    polling loop's teardown ordering (#188), which touches no Docker at all -
    and, from H11 (#310): the "staleAfterMinutes" schema validation, the
    doctor pre-flight (Invoke-DoctorPreflight, against a stub `sh` script),
    the heartbeat's date-combining and WARN/toast-once-per-hour logic
    (Get-LatestDate, Test-PackageStaleness), its date-parsing functions'
    resilience to garbage native-command output, and the rounds-bookkeeping
    parser (Get-RoundsFromComments) against a fixture comment set.

.DESCRIPTION
    The script under test is a top-to-bottom orchestrator, not a module - its
    own "Main flow" section unconditionally calls Import-WavePlan, runs the
    doctor pre-flight, and (with -Validate) exits, which would make a plain
    dot-source of the whole file run (part of) a real orchestration pass.
    This test instead dot-sources only the portion BEFORE the
    "# Main flow" marker comment - every function definition and the handful
    of side-effect-free variable assignments above it - into its own scope,
    so the real function bodies are under test without any of that.

    Get-SanitizedProjectName's string output is checked with CASE-SENSITIVE
    comparisons (-ceq/-cmatch) - PowerShell's default -eq/-match are
    case-insensitive, which would make every assertion pass even with the
    fix fully reverted, since Compose's rule (the entire point of #115) is
    about case. Set-WorktreeEnvOverrides - the actual call site where #115
    manifested - is driven directly against a real worktree .env, then
    Compose itself is used as the oracle: `docker compose config` against
    the file this function actually wrote, proving Compose accepts it, and
    against the exact unsanitized value #115 reported, proving Compose
    rejects THAT - so this suite fails if the sanitizer's call is ever
    removed, not just if its own logic regresses. Stop-PackageStack is
    tested against a real, disposable Compose project in a scratch directory
    (this repo's own convention for these scripts: see
    wellen-docker-cleanup.test.ps1), confirming it actually stops a running
    stack, is a harmless no-op when nothing is running, and never touches a
    different project's container.

    Invoke-WaveDockerCleanup is driven against a STUB cleanup script rather
    than the real one: what is under test here is the job wrapper - that a
    wave without "dockerCleanup" is not cleaned at all, that the cleanup's
    own output reaches the log at the right level, that a refusal is logged
    and swallowed instead of ending the run, and that a hanging call is
    stopped at the time limit and returns promptly. The real script's own
    behaviour is wellen-docker-cleanup.test.ps1's job. Docker is never
    touched by this part.

    Run:  powershell -NoProfile -File scripts\tests\wellen-orchestrator.test.ps1
    Needs Docker (the busybox image is pulled on first use) and `sh` on PATH
    (Git for Windows - the doctor pre-flight tests run a stub through it, the
    same way scripts/doctor itself runs). Takes about a minute.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$script:failures = 0

function Assert {
    param([bool]$Condition, [string]$Message)
    if ($Condition) { Write-Host "  PASS  $Message" } else { Write-Host "  FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}

function DkOut {
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { return @(& docker @args 2>$null) } finally { $ErrorActionPreference = $previous }
}

function Dk {
    # docker, output discarded; Windows PowerShell 5.1 turns any stderr
    # output of a native command into a terminating error under 'Stop'.
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & docker @args 2>&1 | Out-Null } finally { $ErrorActionPreference = $previous }
}

# --- Load just the function definitions from the real script -------------

$orchestratorScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'wellen-orchestrator.ps1'
$sourceText = Get-Content -LiteralPath $orchestratorScript -Raw
$marker = '# Main flow'
$splitAt = $sourceText.IndexOf($marker)
if ($splitAt -lt 0) {
    throw "Could not find the '# Main flow' marker in $orchestratorScript - has it moved? This test's dot-source split depends on it staying above every side-effecting call (the doctor pre-flight, Import-WavePlan, exit)."
}
# Back up to the start of the "# ---" banner line above "# Main flow" so the
# function block above it (Invoke-Package) is not cut mid-comment; harmless
# either way since PowerShell only cares about statement boundaries, not
# comment framing, but keeps a parse error legible if the marker ever moves.
$functionsOnly = $sourceText.Substring(0, $splitAt)

# $PSScriptRoot is empty for a dynamically created script block (there is no
# originating file, and it turns out not reliably settable from the caller's
# scope either) - the real script's top-level code Join-Paths it
# unconditionally (for $LogFile, $DockerCleanupScript, …), and Join-Path
# throws on an empty -Path. Substituting a literal before creating the
# script block sidesteps the automatic-variable question entirely.
$realScriptsDir = Split-Path $orchestratorScript -Parent
$functionsOnly = $functionsOnly -replace '\$PSScriptRoot', "'$realScriptsDir'"
$sb = [ScriptBlock]::Create($functionsOnly)
. $sb

# Saved once, here, before any test section shadows Invoke-Native (the #188
# section near the end of this file already does, permanently, since a
# `function` statement at this script's top level redefines it for
# everything that runs afterward) - Invoke-DoctorPreflight (H11) needs the
# REAL Invoke-Native to actually run its stub via `sh`, so it restores this
# saved copy first.
$script:RealInvokeNative = (Get-Item function:Invoke-Native).ScriptBlock

# Same reasoning, for Invoke-Package (#372 items 2/3): several sections below
# permanently shadow it with an empty stub to isolate Invoke-Wave's own
# polling logic from it. The restart-floor and staleAfterMinutes-precedence
# tests near the end of this file need the REAL Invoke-Package, so they
# restore this saved copy first.
$script:RealInvokePackage = (Get-Item function:Invoke-Package).ScriptBlock

$script:DryRun = $false
# Redirect away from the real scripts/wellen-orchestrator.log (gitignored,
# but a real orchestrator may be writing to it concurrently) - Write-Log
# resolves $LogFile through the scope it shares with this test, so
# reassigning it here after the dot-source is enough.
$script:LogFile = Join-Path $env:TEMP "wellen-orchestrator-test-$([guid]::NewGuid().ToString('N')).log"

Write-Host "== Get-SanitizedProjectName =="

$cases = @(
    @{ In = 'Inventory-w1-delta-sync'; Out = 'inventory-w1-delta-sync' },   # the exact #115 repro
    @{ In = 'inventory-w1-delta-sync'; Out = 'inventory-w1-delta-sync' },   # already valid: unchanged
    @{ In = 'My Repo-w2-x'; Out = 'my-repo-w2-x' },                        # space is not in [a-z0-9_-]
    @{ In = 'Repo_Name-w3'; Out = 'repo_name-w3' },                        # underscore stays, case folds
    @{ In = '-leading-hyphen'; Out = 'x--leading-hyphen' },                 # must start with a letter/number
    @{ In = '1-starts-with-digit'; Out = '1-starts-with-digit' }            # a leading digit is already valid
)
foreach ($case in $cases) {
    $got = Get-SanitizedProjectName $case.In
    # -ceq/-cmatch, deliberately NOT the default -eq/-match: those are
    # case-INSENSITIVE in PowerShell, so 'Inventory-w1-delta-sync' -eq
    # 'inventory-w1-delta-sync' is $true - which would make every assertion
    # here pass even with .ToLowerInvariant() removed from the function
    # entirely, i.e. with #115 fully reintroduced. Compose's own rule is
    # case-sensitive (it rejects any uppercase letter), so the test has to be.
    Assert ($got -ceq $case.Out) "'$($case.In)' -> '$got' (expected '$($case.Out)')"
    Assert ($got -cmatch '^[a-z0-9][a-z0-9_-]*$') "'$got' matches Compose's own project-name rule"
}

Write-Host "== Set-WorktreeEnvOverrides (the real #115 call site) =="

# Get-SanitizedProjectName in isolation proves nothing about #115 actually
# being fixed if the one call site that matters - Set-WorktreeEnvOverrides,
# scripts/wellen-orchestrator.ps1:465 - stopped calling it. This drives that
# real function against a real worktree .env, then uses Compose itself as
# the oracle: not "does the string look right", but "does Compose actually
# accept the project name that gets written to disk."
$envTestDir = Join-Path $env:TEMP "wotest-envoverride-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $envTestDir | Out-Null
try {
    $envPath = Join-Path $envTestDir '.env'
    Set-Content -Path $envPath -Encoding ASCII -Value "SOME_EXISTING_VALUE=kept`n"
    Set-Content -Path (Join-Path $envTestDir 'docker-compose.yml') -Encoding ASCII -Value @"
services:
  app:
    image: busybox
"@
    # $RepoName and $script:PackagePortIndex are read by Set-WorktreeEnvOverrides
    # from the scope it was defined in (this test's own, via the dot-source
    # above) - the real script populates both before any worktree is ever
    # created; this reproduces #115's own exact repro string, "Inventory-w1-delta-sync".
    $RepoName = 'Inventory'
    $testSlug = 'w1-delta-sync'
    $script:PackagePortIndex[$testSlug] = 0

    Set-WorktreeEnvOverrides -EnvPath $envPath -Slug $testSlug

    Assert ((Get-Content -Raw -LiteralPath $envPath) -match '(?m)^SOME_EXISTING_VALUE=kept$') "Set-WorktreeEnvOverrides preserves an existing .env line it does not own"
    $writtenLine = (Get-Content -LiteralPath $envPath) | Where-Object { $_ -cmatch '^COMPOSE_PROJECT_NAME=' }
    Assert ($null -ne $writtenLine) "Set-WorktreeEnvOverrides wrote a COMPOSE_PROJECT_NAME line"
    $writtenValue = ($writtenLine -split '=', 2)[1]
    Assert ($writtenValue -ceq 'inventory-w1-delta-sync') "the written value is lowercased ('$writtenValue')"

    Push-Location -LiteralPath $envTestDir
    try { Dk compose config } finally { Pop-Location }
    Assert ($LASTEXITCODE -eq 0) "Compose ACCEPTS the value Set-WorktreeEnvOverrides actually wrote to disk"

    # The contrast that makes the assertion above non-vacuous: feed Compose
    # the exact unsanitized value #115 reported ("$RepoName-$Slug" verbatim,
    # bypassing the sanitizer) and confirm Compose itself - not a regex this
    # test invented - rejects it. If Get-SanitizedProjectName's call were
    # ever removed from Set-WorktreeEnvOverrides, the ACCEPTS assertion above
    # would fail exactly like this one currently does.
    Set-Content -Path $envPath -Encoding ASCII -Value "COMPOSE_PROJECT_NAME=$RepoName-$testSlug`n"
    Push-Location -LiteralPath $envTestDir
    $previousEap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { $rejectOutput = (& docker compose config 2>&1 | Out-String) } finally { $ErrorActionPreference = $previousEap; Pop-Location }
    Assert ($LASTEXITCODE -ne 0) "Compose REJECTS the unsanitized value ('$RepoName-$testSlug') - the #115 repro, reproduced live"
    Assert ($rejectOutput -match 'invalid project name') "Compose's own rejection message is the one #115 quoted"
} finally {
    Remove-Item -Recurse -Force -Path $envTestDir -ErrorAction SilentlyContinue
}

Write-Host "== Stop-PackageStack =="

$id = -join ((1..8) | ForEach-Object { '{0:x}' -f (Get-Random -Maximum 16) })
$slug = "ztso-$id"
$otherSlug = "ztso-$id-other"
$root = Join-Path $env:TEMP "wotest-$id"
$dir = Join-Path $root $slug
$otherDir = Join-Path $root $otherSlug

function New-MiniStack {
    param([string]$Dir, [string]$ProjectName)
    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Set-Content -Path (Join-Path $Dir '.env') -Encoding ASCII -Value "COMPOSE_PROJECT_NAME=$ProjectName`n"
    # `data` must actually be MOUNTED, not merely declared: Compose v2 never
    # creates an unreferenced top-level volume on `up -d`, so an unmounted
    # `data:` would leave "the volume was removed by -v" true before -v ever
    # ran - true no matter what Stop-PackageStack does.
    Set-Content -Path (Join-Path $Dir 'docker-compose.yml') -Encoding ASCII -Value @"
services:
  app:
    image: busybox
    command: ["sleep", "3600"]
    volumes:
      - data:/data
volumes:
  data:
"@
    Push-Location -LiteralPath $Dir
    try { Dk compose up -d } finally { Pop-Location }
}

try {
    New-MiniStack -Dir $dir -ProjectName $slug
    New-MiniStack -Dir $otherDir -ProjectName $otherSlug

    $running = @(DkOut ps -q --filter "label=com.docker.compose.project=$slug")
    Assert ($running.Count -eq 1) "the mini stack for '$slug' is running before Stop-PackageStack"
    $otherRunning = @(DkOut ps -q --filter "label=com.docker.compose.project=$otherSlug")
    Assert ($otherRunning.Count -eq 1) "the mini stack for '$otherSlug' is running before Stop-PackageStack"
    $volBefore = @(DkOut volume ls -q --filter "label=com.docker.compose.project=$slug")
    Assert ($volBefore.Count -eq 1) "'$slug' named volume ('data') exists before Stop-PackageStack (proves the assertion below is real, not vacuous)"

    Stop-PackageStack -WorktreePath $dir -Slug $slug

    $afterSame = @(DkOut ps -a -q --filter "label=com.docker.compose.project=$slug")
    Assert ($afterSame.Count -eq 0) "'$slug' has no containers left (running or stopped) after Stop-PackageStack"
    $afterVol = @(DkOut volume ls -q --filter "label=com.docker.compose.project=$slug")
    Assert ($afterVol.Count -eq 0) "'$slug' named volume ('data') was removed by -v"
    $afterOther = @(DkOut ps -q --filter "label=com.docker.compose.project=$otherSlug")
    Assert ($afterOther.Count -eq 1) "the OTHER project ('$otherSlug') is untouched"

    # Calling it again, and against a worktree that never brought anything
    # up, must not throw - a package's issue can close without it ever
    # having run `docker compose up`.
    $threw = $false
    try { Stop-PackageStack -WorktreePath $dir -Slug $slug } catch { $threw = $true }
    Assert (-not $threw) "Stop-PackageStack on an already-stopped project does not throw"

    $neverUpDir = Join-Path $root "$slug-never-up"
    New-Item -ItemType Directory -Force -Path $neverUpDir | Out-Null
    $threw = $false
    try { Stop-PackageStack -WorktreePath $neverUpDir -Slug "$slug-never-up" } catch { $threw = $true }
    Assert (-not $threw) "Stop-PackageStack in a directory with no compose file does not throw"

    # A missing worktree path entirely (a package already closed before this
    # run started, per Invoke-Package's early return) must also not throw,
    # under this script's own `$ErrorActionPreference = 'Stop'`.
    $threw = $false
    try { Stop-PackageStack -WorktreePath (Join-Path $root "$slug-does-not-exist") -Slug "$slug-does-not-exist" } catch { $threw = $true }
    Assert (-not $threw) "Stop-PackageStack against a nonexistent worktree path does not throw"
} finally {
    # Symmetric safety-net teardown for BOTH mini-stacks, not just $otherDir:
    # $dir's is expected to already be gone via the in-band Stop-PackageStack
    # call above, but if that call is exactly the thing broken - the
    # scenario this test exists to catch - an Assert failure does not throw,
    # so execution reaches here with $dir's containers/volume still live.
    # Tearing down by PROJECT LABEL rather than by re-running `docker compose
    # down` in $dir works even if $dir itself (and its compose file) is about
    # to be deleted below, or was never fully written.
    foreach ($s in @($slug, $otherSlug)) {
        $ids = @(DkOut ps -a -q --filter "label=com.docker.compose.project=$s")
        if ($ids.Count -gt 0) { Dk rm -f -v @ids }
        $vols = @(DkOut volume ls -q --filter "label=com.docker.compose.project=$s")
        if ($vols.Count -gt 0) { Dk volume rm -f @vols }
        $nets = @(DkOut network ls -q --filter "label=com.docker.compose.project=$s")
        if ($nets.Count -gt 0) { Dk network rm @nets }
    }
    Remove-Item -Recurse -Force -Path $root -ErrorAction SilentlyContinue
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Import-WavePlan: the dockerCleanup boolean check (#140 item 9) =="

# "dockerCleanup" is the one wave-file field the per-wave Docker cleanup reads,
# and the one whose wrong spelling would silently switch cleaning off: Get-Field
# would turn "yes" or "" into the default. -Validate is the planning session's
# only check, so it has to catch a non-boolean.
$planDir = Join-Path $env:TEMP "wotest-plan-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $planDir | Out-Null
function New-PlanFile {
    param([string]$WaveExtra)
    $path = Join-Path $planDir "plan-$([guid]::NewGuid().ToString('N')).json"
    Set-Content -Path $path -Encoding ASCII -Value @"
{
  "plan":      { "name": "t", "planIssue": 1 },
  "standards": { "model": "claude-sonnet-5", "effort": "high",
                 "consolidationModel": "claude-opus-5", "consolidationEffort": "xhigh" },
  "waves": [
    { "number": 1, "waveIssue": 2, "integrationBranch": "integration/t-1", $WaveExtra
      "packages": [ { "specIssue": 3, "slug": "ztx", "branch": "spec/ztx",
                      "spec": "Spec X", "focus": "f" } ] }
  ]
}
"@
    return $path
}
function Get-PlanError {
    param([string]$WaveExtra)
    try { Import-WavePlan -Path (New-PlanFile $WaveExtra) | Out-Null; return $null }
    catch { return $_.Exception.Message }
}
try {
    Assert ($null -eq (Get-PlanError '"dockerCleanup": true,')) 'a wave with "dockerCleanup": true validates'
    Assert ($null -eq (Get-PlanError '"dockerCleanup": false,')) 'a wave with "dockerCleanup": false validates'
    Assert ($null -eq (Get-PlanError '')) 'a wave with no dockerCleanup at all validates (it defaults to false)'
    foreach ($bad in @('"dockerCleanup": "true",', '"dockerCleanup": "yes",', '"dockerCleanup": 1,')) {
        $err = Get-PlanError $bad
        Assert ($err -match 'dockerCleanup: must be true or false') "a non-boolean dockerCleanup is rejected ($bad)"
    }
    # "" and null are what Get-Field itself turns into the default, so they are
    # the two a naive check would let through.
    Assert ((Get-PlanError '"dockerCleanup": "",') -match 'dockerCleanup: must be true or false') 'an EMPTY dockerCleanup is rejected, not silently defaulted'
} finally {
    Remove-Item -Recurse -Force -Path $planDir -ErrorAction SilentlyContinue
}

Write-Host "== Invoke-WaveDockerCleanup (#140 item 9) =="

# The job wrapper, not the cleanup: $DockerCleanupScript is pointed at a stub
# that returns, refuses or hangs on demand. Nothing here touches Docker.
$jobDir = Join-Path $env:TEMP "wotest-job-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $jobDir | Out-Null
$stubHeader = @'
[CmdletBinding()]
param([int]$Wave = 0, [string]$WaveFile = '', [string]$RepoRoot = '', [string]$GhRepo = '')
'@
function New-Stub {
    param([string]$Name, [string]$Body)
    $path = Join-Path $jobDir "$Name.ps1"
    Set-Content -Path $path -Encoding ASCII -Value ($stubHeader + "`n" + $Body)
    return $path
}
function Get-LogText {
    if (Test-Path -LiteralPath $script:LogFile) { return (Get-Content -Raw -LiteralPath $script:LogFile) }
    return ''
}
function Reset-Log { Set-Content -Path $script:LogFile -Value '' -Encoding ASCII }
function New-TestWave {
    param([bool]$DockerCleanup = $true, [string[]]$Slugs = @('ztx'))
    $wave = [pscustomobject]@{
        number   = 7
        packages = @($Slugs | ForEach-Object { [pscustomobject]@{ slug = $_ } })
    }
    if ($DockerCleanup) { $wave | Add-Member -NotePropertyName 'dockerCleanup' -NotePropertyValue $true }
    return $wave
}

$savedTimeout = $DockerCleanupTimeoutSeconds
try {
    # A wave that did not ask for it must not be cleaned at all - the whole
    # reason followups wave 1 can carry "dockerCleanup": false.
    $script:DockerCleanupScript = New-Stub 'never' 'Write-Output "the stub ran"'
    Reset-Log
    Invoke-WaveDockerCleanup -Wave (New-TestWave -DockerCleanup $false)
    Assert ((Get-LogText) -notmatch 'the stub ran') 'a wave WITHOUT "dockerCleanup" does not run the cleanup script at all'
    Assert ([string]::IsNullOrWhiteSpace((Get-LogText))) 'and logs nothing about it'

    Reset-Log
    Invoke-WaveDockerCleanup -Wave (New-TestWave -DockerCleanup $true -Slugs @())
    Assert ((Get-LogText) -notmatch 'the stub ran') 'a wave with no packages does not run the cleanup script either'

    # -DryRun must name the command instead of running it.
    $script:DryRun = $true
    Reset-Log
    try { Invoke-WaveDockerCleanup -Wave (New-TestWave) } finally { $script:DryRun = $false }
    Assert ((Get-LogText) -match '\[DryRun\] would remove the Docker resources of wave 7') '-DryRun logs what it would do'
    Assert ((Get-LogText) -notmatch 'the stub ran') 'and does not run the cleanup script'

    # Success: the cleanup's own output has to reach the orchestrator's log, and
    # its WARNING lines have to arrive as WARN - that convention is the only way
    # a failed removal is visible in a multi-day run's log.
    $script:DockerCleanupScript = New-Stub 'ok' @'
Write-Output "Wave cleanup for 'stub': ztx"
Write-Output "WARNING: could not remove volume 'stub-vol' (still in use?) - left in place."
Write-Output "Done: 1 container(s), 0 network(s), 1 volume(s), 0 image tag(s) processed; 1 could not be removed."
'@
    Reset-Log
    $threw = $false
    try { Invoke-WaveDockerCleanup -Wave (New-TestWave) } catch { $threw = $true }
    $log = Get-LogText
    Assert (-not $threw) 'a successful cleanup does not throw'
    Assert ($log -match "Removing the Docker resources of wave 7's package worktrees \(ztx\)") 'it announces what it is cleaning'
    # \r?$, not $: the log is written with CRLF endings, and in PowerShell's
    # multiline mode $ matches before the \n with the \r still unconsumed.
    Assert ($log -match "(?m)^\[[^\]]+\] \[INFO\]\s+Wave cleanup for 'stub': ztx\r?$") "the stub's own output is logged at INFO"
    Assert ($log -match "(?m)^\[[^\]]+\] \[WARN\]\s+WARNING: could not remove volume 'stub-vol'") 'a WARNING line from the cleanup is logged at WARN'
    Assert ($log -match '1 could not be removed') 'and the summary line comes through'

    # A refusal (the guard doing its job) must be logged and swallowed: the next
    # wave must not wait on housekeeping.
    $script:DockerCleanupScript = New-Stub 'refuses' 'throw "Refusing to remove anything: the wave is not known to be finished."'
    Reset-Log
    $threw = $false
    try { Invoke-WaveDockerCleanup -Wave (New-TestWave) } catch { $threw = $true }
    $log = Get-LogText
    Assert (-not $threw) 'a refused cleanup does not throw out of the wrapper'
    Assert ($log -match "(?m)^\[[^\]]+\] \[WARN\].*did not run: Refusing to remove anything") 'the refusal is logged at WARN, with the reason'
    Assert ($log -match 'continuing') 'and says the run continues'
    Assert ($log -match 'by hand once it is safe') 'and names the command to run by hand'

    # A hang: a native child process that will not come back on its own, which
    # is the shape a wedged `docker` call has. The wrapper has to stop it at the
    # limit AND return promptly - a 15-minute default that did not actually
    # interrupt the job would stall the whole plan.
    $script:DockerCleanupScript = New-Stub 'hangs' '& cmd /c "ping -n 200 127.0.0.1" | Out-Null'
    $script:DockerCleanupTimeoutSeconds = 2
    Reset-Log
    $threw = $false
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    try { Invoke-WaveDockerCleanup -Wave (New-TestWave) } catch { $threw = $true }
    $watch.Stop()
    $log = Get-LogText
    Assert (-not $threw) 'a hanging cleanup does not throw'
    Assert ($log -match 'did not finish within 2 s and was stopped - continuing') 'the hang is stopped at the time limit and logged'
    Assert ($log -match '-DryRun by hand') 'and names the command to look with by hand'
    # The stub would have run for ~200 s; anything near that means Stop-Job did
    # not interrupt it. The bound is generous because Start-Job spins up a whole
    # PowerShell process first.
    Assert ($watch.Elapsed.TotalSeconds -lt 45) "Stop-Job returns promptly on a hanging call ($([int]$watch.Elapsed.TotalSeconds) s, stub would have taken ~200 s)"
} finally {
    $script:DockerCleanupTimeoutSeconds = $savedTimeout
    Remove-Item -Recurse -Force -Path $jobDir -ErrorAction SilentlyContinue
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Resolve-WaveFilePath (#283): a relative -WaveFile must survive Start-Job's own cwd =="

# Invoke-WaveDockerCleanup hands $WaveFile to Start-Job, which runs in its own
# working directory - a relative path resolves fine in the caller's location
# but not there, so cleanup silently no-op'd on every wave (#283). The fix
# resolves once, at startup, before the value can ever reach a job.
$planDir283 = Join-Path $env:TEMP "wotest-283-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $planDir283 | Out-Null
$planFile283 = Join-Path $planDir283 'wellen.json'
Set-Content -Path $planFile283 -Value '{}' -Encoding ASCII
try {
    $absolute = (Get-Item -LiteralPath $planFile283).FullName
    Assert ((Resolve-WaveFilePath -Path $absolute) -eq $absolute) 'an already-absolute path resolves to itself'

    Push-Location $planDir283
    try {
        $resolvedFromRelative = Resolve-WaveFilePath -Path '.\wellen.json'
        Assert ([System.IO.Path]::IsPathRooted($resolvedFromRelative)) 'a relative -WaveFile resolves to an absolute path'
        Assert ($resolvedFromRelative -eq $absolute) 'the resolved relative path matches the resolved absolute path'
    } finally {
        Pop-Location
    }

    $threw = $false
    try { Resolve-WaveFilePath -Path (Join-Path $planDir283 'does-not-exist.json') | Out-Null } catch { $threw = $true }
    Assert $threw 'a missing wave file fails fast at startup, not silently inside a later Start-Job'

    # The actual #283 failure mode: prove Invoke-WaveDockerCleanup's Start-Job
    # only ever receives the resolved, absolute form - never whatever relative
    # spelling the caller originally passed as -WaveFile - by having the stub
    # report back what it actually received.
    $jobDir = Join-Path $env:TEMP "wotest-283-job-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $jobDir | Out-Null
    $script:DockerCleanupScript = New-Stub 'echoes-wavefile' 'Write-Output "WAVEFILE=$WaveFile"'
    $savedWaveFile = $script:WaveFile
    $script:WaveFile = $absolute
    Reset-Log
    try { Invoke-WaveDockerCleanup -Wave (New-TestWave) } finally { $script:WaveFile = $savedWaveFile }
    $log = Get-LogText
    Assert ($log -match [regex]::Escape("WAVEFILE=$absolute")) 'Invoke-WaveDockerCleanup passes the resolved, absolute WaveFile into the job'
} finally {
    Remove-Item -Recurse -Force -Path $planDir283 -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force -Path $jobDir -ErrorAction SilentlyContinue
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Get-PackagePrompt / Get-ConsolidationPrompt: plan.conventions reaches both =="

# #255 and #246 were both hazards a consolidation session hit that
# plan.conventions was supposed to warn it about - and it turned out
# Get-ConsolidationPrompt never referenced $Plan.conventions at all, only
# Get-PackagePrompt did. That bug shipped with a fully green test suite,
# because nothing here called either prompt builder. A marker string is
# enough: if $Plan.conventions is ever dropped from either prompt again,
# this fails immediately instead of silently shipping the same class of gap
# a second time.
#
# [pscustomobject], not a Hashtable: Get-Field's existence check
# ($Object.PSObject.Properties[$Name]) only works against the shape
# ConvertFrom-Json actually produces (which the real orchestrator always
# passes) - a Hashtable silently fails it and Get-Field always returns the
# default, which would make this test pass even with the interpolation
# missing.
$conventionsPlan = [pscustomobject]@{
    name       = 'Test Plan'
    planIssue  = 999
    conventions = 'CONVENTIONS-MARKER-9f3a'
}
$conventionsStandards = [pscustomobject]@{
    roundLimitPackage       = 2
    roundLimitConsolidation = 4
}
$conventionsWave = [pscustomobject]@{
    number            = 7
    waveIssue         = 231
    integrationBranch = 'integration/x'
    dockerCleanup     = $true
}
$conventionsPackage = [pscustomobject]@{
    spec       = 'Issue 1: test'
    specIssue  = 1
    slug       = 'w7-test'
    branch     = 'followups/w7-test'
    focus      = 'Do the thing.'
}

$packagePrompt = Get-PackagePrompt -Plan $conventionsPlan -Standards $conventionsStandards -Wave $conventionsWave -Package $conventionsPackage
Assert ($packagePrompt -match 'CONVENTIONS-MARKER-9f3a') 'Get-PackagePrompt still carries plan.conventions'

# H19 (#318) extracted the literal prompt text out of Get-PackagePrompt into
# scripts/package-prompt.template, so scripts/agent-loop.sh can render the
# identical prompt. This is the one assertion that guards the refactor: an
# exact, byte-for-byte comparison against what the pre-extraction inline
# here-string produced for these same fixtures, so any drift in the template
# file or the .Replace() chain - a dropped space, a wrong placeholder, a
# missing trailing newline - fails here instead of only ever being caught by
# a human diffing prompts.
$expectedPackagePrompt = "/pickup`n`nWork Issue 1: test (issue #1, wave 7 of Test Plan) through the full ship loop, here in this worktree, with the PR against integration/x instead of main - wave plan #999 takes precedence over the ship skill's default target. Open PRs belonging to other packages belong to parallel sessions: do not touch them, do not ask about them. Do the thing. CONVENTIONS-MARKER-9f3a After the merge, close the spec issue, comment on wave issue #231, and do not switch to main. Stop and report if you hit the round limit (2).`n"
Assert ($packagePrompt -ceq $expectedPackagePrompt) 'Get-PackagePrompt renders byte-identical output via the extracted template (H19, #318)'

$consolidationPrompt = Get-ConsolidationPrompt -Plan $conventionsPlan -Standards $conventionsStandards -Wave $conventionsWave -NextWave $null
Assert ($consolidationPrompt -match 'CONVENTIONS-MARKER-9f3a') 'Get-ConsolidationPrompt carries plan.conventions too (#255, #246)'

# Empty conventions must not leave a stray double space or an empty sentence
# behind in either prompt - both builders guard this the same way
# ("if ($conventions) { $conventions = "$conventions " }"), so one shared
# check for the artifact is enough.
$emptyPlan = [pscustomobject]@{ name = 'Test Plan'; planIssue = 999; conventions = '' }
$packagePromptEmpty = Get-PackagePrompt -Plan $emptyPlan -Standards $conventionsStandards -Wave $conventionsWave -Package $conventionsPackage
$consolidationPromptEmpty = Get-ConsolidationPrompt -Plan $emptyPlan -Standards $conventionsStandards -Wave $conventionsWave -NextWave $null
Assert ($packagePromptEmpty -notmatch '  ') 'empty plan.conventions leaves no double space in Get-PackagePrompt'
Assert ($consolidationPromptEmpty -notmatch '  ') 'empty plan.conventions leaves no double space in Get-ConsolidationPrompt'

Write-Host "== Get-PackagePrompt / Get-ConsolidationPrompt: wave.planIssue overrides plan.planIssue =="

# PR #369 (welle 16) appended a wave onto a plan file whose own plan.planIssue
# (#176) had already closed - review-docs caught, on a real diff, that both
# prompt builders would still tell their sessions to close/comment on #176,
# the wrong (and already-closed) issue, and never mention the wave's own
# tracking issue at all. Same shape of bug as the plan.conventions gap above:
# a field that exists in the data model but that neither prompt builder read.
# No wave.planIssue set anywhere here proves nothing new by itself, so this
# also asserts the ABSENCE case falls back to plan.planIssue, not just that
# the override wins when present.
Assert ($packagePrompt -match 'wave plan #999') 'Get-PackagePrompt falls back to plan.planIssue when the wave sets no override'
Assert ($consolidationPrompt -match 'wave plan #999') 'Get-ConsolidationPrompt falls back to plan.planIssue when the wave sets no override'

$overrideWave = [pscustomobject]@{
    number            = 16
    waveIssue         = 366
    planIssue         = 366
    planName          = 'PLANNAME-MARKER-b7c2'
    integrationBranch = 'integration/x'
    dockerCleanup     = $true
}
$overridePackagePrompt = Get-PackagePrompt -Plan $conventionsPlan -Standards $conventionsStandards -Wave $overrideWave -Package $conventionsPackage
Assert ($overridePackagePrompt -match 'wave plan #366') 'Get-PackagePrompt uses wave.planIssue when set'
Assert ($overridePackagePrompt -notmatch 'wave plan #999') 'Get-PackagePrompt does not also mention plan.planIssue when overridden'
Assert ($overridePackagePrompt -match 'PLANNAME-MARKER-b7c2') 'Get-PackagePrompt uses wave.planName when set'
Assert ($overridePackagePrompt -notmatch 'Test Plan') 'Get-PackagePrompt does not also mention plan.name when overridden'

$overrideConsolidationPromptLast = Get-ConsolidationPrompt -Plan $conventionsPlan -Standards $conventionsStandards -Wave $overrideWave -NextWave $null
Assert ($overrideConsolidationPromptLast -match 'close wave plan #366') 'Get-ConsolidationPrompt (last wave) closes the overriding wave.planIssue, not plan.planIssue'
Assert ($overrideConsolidationPromptLast -notmatch '#999') 'Get-ConsolidationPrompt (last wave) never mentions the stale plan.planIssue when overridden'
# review-go's round-2 finding on PR #369: the planIssue fix alone still left
# $Plan.name interpolated unconditionally, so the generated close-comment
# read "close wave plan #366 - Deferred follow-ups ... is then complete" -
# the wrong plan's name attached to the right issue number. Pin both halves.
Assert ($overrideConsolidationPromptLast -match 'PLANNAME-MARKER-b7c2 is then complete') 'Get-ConsolidationPrompt (last wave) declares the overriding wave.planName complete, not plan.name'
Assert ($overrideConsolidationPromptLast -notmatch 'Test Plan') 'Get-ConsolidationPrompt (last wave) does not also mention plan.name when overridden'

$nextWaveStub = [pscustomobject]@{ number = 17; integrationBranch = 'integration/y' }
$overrideConsolidationPromptNext = Get-ConsolidationPrompt -Plan $conventionsPlan -Standards $conventionsStandards -Wave $overrideWave -NextWave $nextWaveStub
Assert ($overrideConsolidationPromptNext -match 'comment on wave plan #366') 'Get-ConsolidationPrompt (with a next wave) comments on the overriding wave.planIssue, not plan.planIssue'
Assert ($overrideConsolidationPromptNext -match 'wave 16 of PLANNAME-MARKER-b7c2') 'Get-ConsolidationPrompt (with a next wave) also uses wave.planName in its opening sentence'
Assert ($overrideConsolidationPromptNext -notmatch 'Test Plan') 'Get-ConsolidationPrompt (with a next wave) does not also mention plan.name when overridden'

Write-Host "== Invoke-Wave (parallel branch): teardown follows ACTUAL close order (#188) =="

# The parallel branch's own polling loop (wellen-orchestrator.ps1, the `while
# ($pending.Count -gt 0)` loop inside Invoke-Wave) was never exercised: this
# drives the real Invoke-Wave with two fake packages where the SECOND-listed
# one ('iw-b') closes FIRST, and asserts Stop-PackageStack fires in that
# close order, not file order. Two concrete regressions this has to catch:
# dropping the `$pending = $stillPending` reassignment (already-torn-down
# packages keep getting re-checked and the loop never shrinks or exits), and
# swapping the closed/open branches so $stillPending is added to on the
# CLOSED condition instead of the OPEN one (tears every package down on every
# poll instead of once, when it actually closes). Test-IssueClosed,
# Stop-PackageStack, Invoke-Package and Start-Sleep are shadowed per the
# deferred review's own suggestion; Invoke-Native is shadowed too, purely so
# the surrounding consolidation step it also runs (a `git ls-remote` and a
# `gh pr list`) never makes a real call - it is made to look like a PR is
# already open, so Invoke-Wave logs and skips instead of starting a second
# session. That skip means the marker-file branch (Set-Content) is never
# taken either; the one real-path touch left is the unconditional
# `Remove-Item $marker -ErrorAction SilentlyContinue` at the end of
# Invoke-Wave, which is a no-op here since nothing in this test ever creates
# that file.
$stopOrder = [System.Collections.Generic.List[string]]::new()
$closed = @{}
$pkgA = [pscustomobject]@{ spec = 'A'; specIssue = 1; slug = 'iw-a' }
$pkgB = [pscustomobject]@{ spec = 'B'; specIssue = 2; slug = 'iw-b' }
$wave = [pscustomobject]@{ number = 9; waveIssue = 3; integrationBranch = 'x'; packages = @($pkgA, $pkgB) }
$closed[$pkgB.specIssue] = $true
function Test-IssueClosed { param([int]$Number) return [bool]$closed[$Number] }
function Invoke-Package { param($Plan, $Standards, $Wave, $Package) }
function Invoke-Native { param([scriptblock]$Command) $script:NativeExit = 0; return '999' }
function Stop-PackageStack {
    param([string]$WorktreePath, [string]$Slug)
    $stopOrder.Add($Slug)
    # iw-a's fake issue only "closes" once iw-b (its sibling, closing first)
    # has been torn down; the wave issue only "closes" once iw-a has too, so
    # Invoke-Wave's own final Wait-ForIssueClosed returns at once instead of
    # really waiting.
    if ($Slug -eq 'iw-b') { $closed[$pkgA.specIssue] = $true }
    if ($Slug -eq 'iw-a') { $closed[$wave.waveIssue] = $true }
}
$script:sleeps = 0
function Start-Sleep {
    param($Seconds)
    $script:sleeps++
    if ($script:sleeps -gt 3) { throw 'watchdog: the polling loop did not shrink/terminate' }
}

$threw = $false; $err = $null
try { Invoke-Wave -Plan $null -Standards $null -Wave $wave -NextWave $null } catch { $threw = $true; $err = $_.Exception.Message }
Assert (-not $threw) "the polling loop terminates without a shadowed helper throwing ($err)"
Assert (($stopOrder -join ',') -eq 'iw-b,iw-a') "teardown fires in ACTUAL close order (iw-b, listed second, closes first) - got '$($stopOrder -join ',')'"

Write-Host "== Invoke-Wave (unsequential branch) wiring: Test-PackageHeartbeat is actually called for a still-open package (H11) =="

# The #188 test above already drives this same polling loop, but never
# shadows or asserts on Test-PackageHeartbeat - it proves the loop's
# teardown ordering, nothing about the heartbeat call this PR adds to it.
# Deleting that call (and the staleAfterMinutes resolution above it) from
# Invoke-Wave leaves every existing assertion in this file green - this test
# is what actually fails if that happens.
$heartbeatCallsUnseq = [System.Collections.Generic.List[string]]::new()
function Test-PackageHeartbeat {
    param($Package, [string]$WorktreePath, [int]$StaleAfterMinutes, [string]$WindowTitle)
    $heartbeatCallsUnseq.Add($Package.slug)
}
$closedUnseq = @{}
$pkgHbU = [pscustomobject]@{ spec = 'HBU'; specIssue = 100; slug = 'hbu-pkg'; branch = 'hbu-branch' }
$waveHbU = [pscustomobject]@{ number = 20; waveIssue = 101; integrationBranch = 'hbu'; packages = @($pkgHbU) }
function Test-IssueClosed { param([int]$Number) return [bool]$closedUnseq[$Number] }
function Invoke-Package { param($Plan, $Standards, $Wave, $Package) }
function Invoke-Native { param([scriptblock]$Command) $script:NativeExit = 0; return '999' }
function Stop-PackageStack { param([string]$WorktreePath, [string]$Slug) $closedUnseq[$waveHbU.waveIssue] = $true }
$script:sleepsHbU = 0
function Start-Sleep {
    param($Seconds)
    $script:sleepsHbU++
    if ($script:sleepsHbU -gt 3) { throw 'watchdog: unsequential heartbeat wiring test did not terminate' }
    $closedUnseq[$pkgHbU.specIssue] = $true
}
$standardsHbU = [pscustomobject]@{ staleAfterMinutes = 45 }
$threwHbU = $false
try { Invoke-Wave -Plan $null -Standards $standardsHbU -Wave $waveHbU -NextWave $null } catch { $threwHbU = $true }
Assert (-not $threwHbU) 'the unsequential branch terminates without throwing'
Assert ($heartbeatCallsUnseq.Count -ge 1) 'Test-PackageHeartbeat is actually invoked for the still-open package while polling'
Assert ($heartbeatCallsUnseq -contains 'hbu-pkg') 'and it is invoked for the right package'

Write-Host "== Invoke-Wave (sequential branch) wiring: Wait-ForPackageIssueClosed actually calls Test-PackageHeartbeat (H11) =="

# The sequential branch has no equivalent to the #188/unsequential tests at
# all before this PR - Wait-ForPackageIssueClosed's own heartbeat call had
# zero coverage (confirmed by grep in review). Same shape as the test above,
# against a "sequential": true wave instead.
$heartbeatCallsSeq = [System.Collections.Generic.List[string]]::new()
function Test-PackageHeartbeat {
    param($Package, [string]$WorktreePath, [int]$StaleAfterMinutes, [string]$WindowTitle)
    $heartbeatCallsSeq.Add($Package.slug)
}
$closedSeq = @{}
$pkgHbS = [pscustomobject]@{ spec = 'HBS'; specIssue = 200; slug = 'hbs-pkg'; branch = 'hbs-branch' }
$waveHbS = [pscustomobject]@{ number = 21; waveIssue = 201; integrationBranch = 'hbs'; sequential = $true; packages = @($pkgHbS) }
function Test-IssueClosed { param([int]$Number) return [bool]$closedSeq[$Number] }
function Invoke-Package { param($Plan, $Standards, $Wave, $Package) }
function Invoke-Native { param([scriptblock]$Command) $script:NativeExit = 0; return '999' }
function Stop-PackageStack { param([string]$WorktreePath, [string]$Slug) $closedSeq[$waveHbS.waveIssue] = $true }
$script:sleepsHbS = 0
function Start-Sleep {
    param($Seconds)
    $script:sleepsHbS++
    if ($script:sleepsHbS -gt 3) { throw 'watchdog: sequential heartbeat wiring test did not terminate' }
    $closedSeq[$pkgHbS.specIssue] = $true
}
$standardsHbS = [pscustomobject]@{ staleAfterMinutes = 45 }
$threwHbS = $false
try { Invoke-Wave -Plan $null -Standards $standardsHbS -Wave $waveHbS -NextWave $null } catch { $threwHbS = $true }
Assert (-not $threwHbS) 'the sequential branch terminates without throwing'
Assert ($heartbeatCallsSeq.Count -ge 1) 'Wait-ForPackageIssueClosed actually invokes Test-PackageHeartbeat while waiting'
Assert ($heartbeatCallsSeq -contains 'hbs-pkg') 'and it is invoked for the right package'

Write-Host "== Import-WavePlan: staleAfterMinutes validation (H11) =="

# staleAfterMinutes is optional everywhere - Get-Field returns $null when it
# is absent, which Test-StaleAfterMinutes must accept without an error - and,
# when present, must be a positive integer, not a string, decimal, or zero.
$stalePlanDir = Join-Path $env:TEMP "wotest-staleplan-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $stalePlanDir | Out-Null
function New-StalePlanFile {
    param([string]$StandardsExtra = '', [string]$PackageExtra = '')
    $path = Join-Path $stalePlanDir "plan-$([guid]::NewGuid().ToString('N')).json"
    Set-Content -Path $path -Encoding ASCII -Value @"
{
  "plan":      { "name": "t", "planIssue": 1 },
  "standards": { "model": "claude-sonnet-5", "effort": "high",
                 "consolidationModel": "claude-opus-5", "consolidationEffort": "xhigh"$StandardsExtra },
  "waves": [
    { "number": 1, "waveIssue": 2, "integrationBranch": "integration/t-1",
      "packages": [ { "specIssue": 3, "slug": "ztx", "branch": "spec/ztx",
                      "spec": "Spec X", "focus": "f"$PackageExtra } ] }
  ]
}
"@
    return $path
}
function Get-StalePlanError {
    param([string]$StandardsExtra = '', [string]$PackageExtra = '')
    try { Import-WavePlan -Path (New-StalePlanFile -StandardsExtra $StandardsExtra -PackageExtra $PackageExtra) | Out-Null; return $null }
    catch { return $_.Exception.Message }
}
try {
    Assert ($null -eq (Get-StalePlanError)) 'staleAfterMinutes absent everywhere validates (optional field)'
    Assert ($null -eq (Get-StalePlanError -StandardsExtra ',"staleAfterMinutes": 45')) 'standards.staleAfterMinutes as a positive integer validates'
    Assert ($null -eq (Get-StalePlanError -PackageExtra ',"staleAfterMinutes": 10')) 'a per-package staleAfterMinutes as a positive integer validates'

    $err = Get-StalePlanError -StandardsExtra ',"staleAfterMinutes": 0'
    Assert ($err -match 'standards\.staleAfterMinutes: must be a positive integer') "standards.staleAfterMinutes: 0 is rejected, with the field path ($err)"

    $err = Get-StalePlanError -PackageExtra ',"staleAfterMinutes": 0'
    Assert ($err -match 'waves\[0\]\.packages\[0\]\.staleAfterMinutes: must be a positive integer') "a per-package staleAfterMinutes: 0 is rejected, with the field path ($err)"

    $err = Get-StalePlanError -StandardsExtra ',"staleAfterMinutes": "45"'
    Assert ($err -match 'standards\.staleAfterMinutes: must be a positive integer') "a non-integer (string) standards.staleAfterMinutes is rejected ($err)"

    $err = Get-StalePlanError -PackageExtra ',"staleAfterMinutes": 12.5'
    Assert ($err -match 'waves\[0\]\.packages\[0\]\.staleAfterMinutes: must be a positive integer') "a non-integer (decimal) per-package staleAfterMinutes is rejected ($err)"
} finally {
    Remove-Item -Recurse -Force -Path $stalePlanDir -ErrorAction SilentlyContinue
}

Write-Host "== Get-LatestDate: the heartbeat picks the newest of the three sources (H11) =="

$dOld = (Get-Date).AddMinutes(-60)
$dMid = (Get-Date).AddMinutes(-30)
$dNew = (Get-Date).AddMinutes(-5)
Assert ((Get-LatestDate -Dates @($dMid, $dNew, $dOld)) -eq $dNew) 'the newest of three real dates wins, regardless of input order'
Assert ((Get-LatestDate -Dates @($null, $dOld, $null)) -eq $dOld) 'null sources (a signal with nothing yet) are ignored'
Assert ($null -eq (Get-LatestDate -Dates @($null, $null, $null))) 'all three sources empty returns null - no signal at all, not "now"'

Write-Host "== Get-RoundsFromComments: H5 verdict markers -> rounds/blocks (H11) =="

# The exact marker format scripts/dev.d/gate reads:
#   <!-- verdict: APPROVE|BLOCK round=<n> sha=<head sha> reviewer=go|tests|docs -->
# Round 1 has one BLOCK (tests); round 2's re-review is a clean sweep - the
# fixture this acceptance criterion names: rounds=2, blocks=1.
$roundsFixture = @(
    'Looks mostly fine, one nit.',
    '<!-- verdict: APPROVE round=1 sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa reviewer=go -->',
    'The retry loop never terminates on a permanent error.',
    '<!-- verdict: BLOCK round=1 sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa reviewer=tests -->',
    '<!-- verdict: APPROVE round=1 sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa reviewer=docs -->',
    'Fixed - the loop now gives up after 3 attempts.',
    '<!-- verdict: APPROVE round=2 sha=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb reviewer=go -->',
    '<!-- verdict: APPROVE round=2 sha=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb reviewer=tests -->',
    '<!-- verdict: APPROVE round=2 sha=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb reviewer=docs -->'
)
$roundsInfo = Get-RoundsFromComments -CommentLines $roundsFixture -PrNumber '42'
Assert ($roundsInfo.Rounds -eq 2) "rounds=2 from the fixture (got $($roundsInfo.Rounds))"
Assert ($roundsInfo.Blocks -eq 1) "blocks=1 from the fixture (got $($roundsInfo.Blocks))"
Assert ($roundsInfo.PrNumber -eq '42') 'the PR number passes through unchanged'
$noMarkers = Get-RoundsFromComments -CommentLines @('nothing here') -PrNumber '1'
Assert ($noMarkers.Rounds -eq 0 -and $noMarkers.Blocks -eq 0) 'no markers at all is rounds=0, blocks=0 - not an error'

Write-Host "== Get-ParsedDateOrNull / Get-BranchLastCommitDate / Get-NewestReviewerCommentDate: garbage output never throws (H11) =="

# The #188 test above already proves Invoke-Wave survives a shadowed
# Invoke-Native that always returns a placeholder ('999') - this proves the
# specific new functions that try to parse a DATE out of native output do the
# same: a transient gh/git hiccup or an unparseable response must be "no
# signal", never a thrown exception reaching into a multi-day run.
Assert ($null -eq (Get-ParsedDateOrNull -Text 'not-a-date')) 'unparseable text returns null, not a throw'
Assert ($null -eq (Get-ParsedDateOrNull -Text '')) 'empty text returns null'
function Invoke-Native { param([scriptblock]$Command) $script:NativeExit = 0; return '999' }
$threw = $false; $result = $null
try { $result = Get-BranchLastCommitDate -Branch 'some-branch' } catch { $threw = $true }
Assert (-not $threw) 'Get-BranchLastCommitDate does not throw on unparseable gh/git output'
Assert ($null -eq $result) 'and reports no signal'
$threw = $false; $result = $null
try { $result = Get-NewestReviewerCommentDate -Branch 'some-branch' } catch { $threw = $true }
Assert (-not $threw) 'Get-NewestReviewerCommentDate does not throw on unparseable gh output'
Assert ($null -eq $result) 'and reports no signal'

Write-Host "== Register-PackageStartIfUnknown: seeds a floor on restart, without clobbering one that already exists (H11) =="

# Invoke-Package's restart-meets-existing-worktree path (.RESTART SAFETY)
# calls this so a package with none of the three real heartbeat signals is
# still eventually flagged stale after a restart, instead of never at all -
# round-1 review's finding (review-go). Not overwriting an existing floor
# matters too: a second restart, or this same process noticing the same
# package again on a later poll, must not keep pushing "started" into the
# future and resetting the clock.
$script:PackageStartedAt = @{}
Register-PackageStartIfUnknown -Slug 'restart-pkg'
Assert ($script:PackageStartedAt.ContainsKey('restart-pkg')) 'seeds a floor for a package with none yet'
$seededAt = $script:PackageStartedAt['restart-pkg']
Assert (((Get-Date).ToUniversalTime() - $seededAt).TotalSeconds -lt 10) 'the seeded floor is "now" (UTC), not some arbitrary past or future time'

$earlierFloor = (Get-Date).ToUniversalTime().AddMinutes(-45)
$script:PackageStartedAt['restart-pkg'] = $earlierFloor
Register-PackageStartIfUnknown -Slug 'restart-pkg'
Assert ($script:PackageStartedAt['restart-pkg'] -eq $earlierFloor) 'a package that already has a floor keeps it - a later call never overwrites it'

Write-Host "== Get-PackageLastActivity: actually wired to all three heartbeat sources (H11) =="

# Get-LatestDate on its own only proves the "pick the newest" combinator
# works in isolation - this file's own history (#255/#246: a prompt builder
# that was supposed to reference a field and simply never did, shipped with
# a fully green suite) is exactly the class of gap that leaves unguarded.
# Shadowing the three source functions with distinct, known dates proves
# Get-PackageLastActivity actually calls all three and returns their
# maximum, not just one of them by coincidence. Placed AFTER the garbage-
# output section above, deliberately: that section needs the REAL
# Get-BranchLastCommitDate/Get-NewestReviewerCommentDate still in place, and
# these shadows (like every other top-level `function` redefinition in this
# file) persist for the rest of the run once defined.
$commitDate = (Get-Date).ToUniversalTime().AddMinutes(-40)
$worklogDate = (Get-Date).ToUniversalTime().AddMinutes(-10)
$commentDate = (Get-Date).ToUniversalTime().AddMinutes(-25)
function Get-BranchLastCommitDate { param([string]$Branch) return $commitDate }
function Get-WorklogLastWriteDate { param([string]$WorktreePath) return $worklogDate }
function Get-NewestReviewerCommentDate { param([string]$Branch) return $commentDate }
$activityPackage = [pscustomobject]@{ slug = 'gpla-test'; branch = 'x' }
Assert ((Get-PackageLastActivity -Package $activityPackage -WorktreePath 'C:\anything') -eq $worklogDate) `
    'picks the newest of all three real sources (the worklog mtime, here the most recent)'

# With only one source populated, that lone source wins - proves the other
# two are actually consulted (and correctly ignored when empty), not just the
# first one returned.
function Get-BranchLastCommitDate { param([string]$Branch) return $null }
function Get-WorklogLastWriteDate { param([string]$WorktreePath) return $null }
Assert ((Get-PackageLastActivity -Package $activityPackage -WorktreePath 'C:\anything') -eq $commentDate) `
    'with only the PR-comment source populated, that source wins'

Write-Host "== Test-PackageStaleness: WARN + toast once per hour, never acts on it (H11) =="

$script:PackageStartedAt = @{}
$script:PackageLastWarnedAt = @{}
$script:toastCalls = 0
function Invoke-Native { param([scriptblock]$Command) $script:toastCalls++; $script:NativeExit = 0; return '' }
$hbPackage = [pscustomobject]@{ slug = 'hb-test'; branch = 'x'; specIssue = 1; spec = 'X' }
$staleSince = (Get-Date).ToUniversalTime().AddMinutes(-50)
try {
    Reset-Log
    Test-PackageStaleness -Package $hbPackage -LastActivity $staleSince -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - X (#1)'
    Assert ((Get-LogText) -match 'looks stale') 'past the threshold: logs a WARN'
    Assert ($script:toastCalls -eq 1) 'and raises exactly one toast'

    Reset-Log
    Test-PackageStaleness -Package $hbPackage -LastActivity $staleSince -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - X (#1)'
    Assert ((Get-LogText) -notmatch 'looks stale') 'a second check within the same hour logs nothing more'
    Assert ($script:toastCalls -eq 1) 'and does not toast again'

    # Simulate an hour having passed since the last warning.
    $script:PackageLastWarnedAt[$hbPackage.slug] = (Get-Date).AddMinutes(-61)
    Reset-Log
    Test-PackageStaleness -Package $hbPackage -LastActivity $staleSince -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - X (#1)'
    Assert ((Get-LogText) -match 'looks stale') 'after an hour has passed, it warns again'
    Assert ($script:toastCalls -eq 2) 'and toasts again'

    Reset-Log
    Test-PackageStaleness -Package $hbPackage -LastActivity ((Get-Date).ToUniversalTime()) -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - X (#1)'
    Assert ((Get-LogText) -notmatch 'looks stale') 'fresh activity is never flagged stale'

    # No signal at all (LastActivity $null) and no recorded start time either
    # (a package Test-PackageStaleness has never been told about) must not
    # warn - there is nothing to compare against, and inventing "now" as the
    # floor would flag every brand-new package as stale on its very first poll.
    $unknownPackage = [pscustomobject]@{ slug = 'hb-unknown'; branch = 'y'; specIssue = 2; spec = 'Y' }
    Reset-Log
    Test-PackageStaleness -Package $unknownPackage -LastActivity $null -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - Y (#2)'
    Assert ((Get-LogText) -notmatch 'looks stale') 'no signal and no recorded start time: never warns'

    # The PackageStartedAt floor: no signal yet, but a start time was
    # recorded (Invoke-Package's own bookkeeping) - and it is already past
    # the threshold.
    $script:PackageStartedAt[$unknownPackage.slug] = (Get-Date).ToUniversalTime().AddMinutes(-50)
    Reset-Log
    Test-PackageStaleness -Package $unknownPackage -LastActivity $null -StaleAfterMinutes 45 -WindowTitle 'Wave 1 - Y (#2)'
    Assert ((Get-LogText) -match 'looks stale') 'no signal yet, but past the recorded start-time floor: warns'
} finally {
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Invoke-Package: an already-existing worktree still registers a start-time floor (#372 item 2) =="

# Register-PackageStartIfUnknown has its own unit test above, but nothing
# exercised the call SITE in Invoke-Package's restart-meets-existing-worktree
# branch: deleting that call leaves the rest of this suite green. Restores the
# REAL Invoke-Package first - several sections above this one (the #188 and
# heartbeat-wiring sections) permanently shadow it with an empty stub so
# Invoke-Wave's own polling logic can be tested in isolation from it.
Set-Item function:Invoke-Package -Value $script:RealInvokePackage

$savedParentDirIp = $ParentDir
$savedRepoNameIp = $RepoName
$ipParentDir = Join-Path $env:TEMP "wotest-invoke-package-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $ipParentDir | Out-Null
$ParentDir = $ipParentDir
$RepoName = 'wotest-repo'
$ipExistingSlug = 'ip-existing-pkg'
New-Item -ItemType Directory -Force -Path (Join-Path $ipParentDir "$RepoName-$ipExistingSlug") | Out-Null

$script:PackageStartedAt = @{}
$script:DryRun = $true
Reset-Log
try {
    $ipPackage = [pscustomobject]@{ slug = $ipExistingSlug; branch = 'ip-branch'; specIssue = 555; spec = 'IP' }
    $ipWave = [pscustomobject]@{ number = 1; integrationBranch = 'integration/ip-test' }
    Invoke-Package -Plan ([pscustomobject]@{ planIssue = 1; name = 'IP Plan' }) -Standards ([pscustomobject]@{}) -Wave $ipWave -Package $ipPackage
    Assert ($script:PackageStartedAt.ContainsKey($ipExistingSlug)) `
        'an existing worktree still calls Register-PackageStartIfUnknown, seeding a start-time floor'
    Assert ((Get-LogText) -match 'already existed') 'and logs that no new session was started'
} finally {
    $ParentDir = $savedParentDirIp
    $RepoName = $savedRepoNameIp
    $script:DryRun = $false
    Remove-Item -Recurse -Force -Path $ipParentDir -ErrorAction SilentlyContinue
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Invoke-Package: per-package staleAfterMinutes overrides standards.staleAfterMinutes end-to-end (#372 item 3) =="

# The precedence itself (Get-Field $Package 'staleAfterMinutes' (Get-Field
# $Standards 'staleAfterMinutes' 45)) is never asserted end-to-end anywhere
# else in this file - the existing "Start-ClaudeSession -DryRun" test below
# calls Start-ClaudeSession directly with an already-resolved value, which
# would stay green even if Invoke-Package's own Get-Field call swapped
# $Package and $Standards. This drives Invoke-Package itself (still the REAL
# one, restored above) against a brand-new (non-existing) worktree and reads
# the resolved threshold back out of Start-ClaudeSession's own -DryRun log
# line.
$savedParentDirPrec = $ParentDir
$savedRepoNamePrec = $RepoName
$precParentDir = Join-Path $env:TEMP "wotest-stale-precedence-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $precParentDir | Out-Null
$ParentDir = $precParentDir
$RepoName = 'wotest-repo'
$script:DryRun = $true
try {
    $precStandards = [pscustomobject]@{ model = 'claude-sonnet-5'; effort = 'high'; staleAfterMinutes = 45 }
    $precWave = [pscustomobject]@{ number = 1; integrationBranch = 'integration/prec-test' }
    $precPlan = [pscustomobject]@{ planIssue = 1; name = 'Precedence Plan' }

    Reset-Log
    $precPackageNoOverride = [pscustomobject]@{ slug = 'prec-no-override'; branch = 'prec-branch-1'; specIssue = 601; spec = 'Prec1' }
    Invoke-Package -Plan $precPlan -Standards $precStandards -Wave $precWave -Package $precPackageNoOverride
    Assert ((Get-LogText) -match 'stale threshold 45min') `
        'with no per-package override, the resolved threshold is standards.staleAfterMinutes (45)'

    Reset-Log
    $precPackageOverride = [pscustomobject]@{ slug = 'prec-override'; branch = 'prec-branch-2'; specIssue = 602; spec = 'Prec2'; staleAfterMinutes = 20 }
    Invoke-Package -Plan $precPlan -Standards $precStandards -Wave $precWave -Package $precPackageOverride
    Assert ((Get-LogText) -match 'stale threshold 20min') `
        'a per-package staleAfterMinutes (20) overrides standards.staleAfterMinutes (45) - this fails if the precedence is ever swapped'
} finally {
    $ParentDir = $savedParentDirPrec
    $RepoName = $savedRepoNamePrec
    $script:DryRun = $false
    Remove-Item -Recurse -Force -Path $precParentDir -ErrorAction SilentlyContinue
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Invoke-PackageRoundsBookkeeping: both WARN-and-return-null branches (#372 item 4) =="

# Neither branch has ever been exercised: no PR found for the branch yet, and
# a PR found but its comments unreadable (a transient gh hiccup). Both must
# log a WARN and return $null rather than throw, so a single bad poll never
# ends a multi-day orchestrator run.
Reset-Log
function Invoke-Native { param([scriptblock]$Command) $script:NativeExit = 1; return '' }
$rbNoPr = Invoke-PackageRoundsBookkeeping -Package ([pscustomobject]@{ slug = 'rb-no-pr'; branch = 'rb-branch-1' })
Assert ($null -eq $rbNoPr) 'no PR found for the branch: returns $null, not a throw'
Assert ((Get-LogText) -match "Rounds bookkeeping for 'rb-no-pr': no PR found for branch 'rb-branch-1' - skipping\." ) `
    'and logs a WARN naming the branch'
Assert ((Get-LogText) -match '\[WARN\]') 'at WARN level'

Reset-Log
$script:rbCallCount = 0
function Invoke-Native {
    param([scriptblock]$Command)
    $script:rbCallCount++
    if ($script:rbCallCount -eq 1) { $script:NativeExit = 0; return '77' }
    $script:NativeExit = 1
    return ''
}
$rbUnreadable = Invoke-PackageRoundsBookkeeping -Package ([pscustomobject]@{ slug = 'rb-unreadable'; branch = 'rb-branch-2' })
Assert ($null -eq $rbUnreadable) 'a PR found but its comments unreadable: also returns $null, not a throw'
Assert ((Get-LogText) -match "Rounds bookkeeping for 'rb-unreadable': could not read PR #77's comments - skipping\.") `
    'and logs a WARN naming the PR number'
Assert ((Get-LogText) -match '\[WARN\]') 'at WARN level'

Write-Host "== Get-PackageLastActivity: worktree file mtime as a fourth activity signal (#429) =="

# Restore the REAL Invoke-Native - the rounds-bookkeeping section just above
# leaves a stateful stub in place, and this test needs the real thing to run
# `git status --porcelain` against a real scratch repo below.
Set-Item function:Invoke-Native -Value $script:RealInvokeNative

# #429: the three existing signals (branch commit date, worklog mtime, newest
# PR comment) never reflect a session actively editing/testing without
# committing - observed twice in the new-features wave as false-positive
# staleness warnings. Get-WorktreeFileLastWriteDate adds the newest mtime
# across the worktree's own changed (tracked+untracked) files, via `git
# status --porcelain` plus Get-Item, as a fourth source into Get-LatestDate.
$gplaDir = Join-Path $env:TEMP "wotest-worktree-mtime-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $gplaDir | Out-Null
try {
    git -C $gplaDir init --quiet 2>$null
    git -C $gplaDir config user.email 'test@example.com' 2>$null
    git -C $gplaDir config user.name 'Test' 2>$null
    Set-Content -Path (Join-Path $gplaDir 'committed.txt') -Value 'v1'
    git -C $gplaDir add committed.txt 2>$null
    git -C $gplaDir commit -m 'initial' --quiet 2>$null

    # No signal at all from the three original sources; only an uncommitted,
    # freshly-written file in the worktree.
    function Get-BranchLastCommitDate { param([string]$Branch) return $null }
    function Get-WorklogLastWriteDate { param([string]$WorktreePath) return $null }
    function Get-NewestReviewerCommentDate { param([string]$Branch) return $null }
    Start-Sleep -Milliseconds 50
    Set-Content -Path (Join-Path $gplaDir 'wip.txt') -Value 'uncommitted work'
    $expectedMtime = (Get-Item -LiteralPath (Join-Path $gplaDir 'wip.txt')).LastWriteTimeUtc

    $gplaPackage = [pscustomobject]@{ slug = 'gpla-mtime-test'; branch = 'x' }
    $result = Get-PackageLastActivity -Package $gplaPackage -WorktreePath $gplaDir
    Assert ($null -ne $result) 'an uncommitted, untracked change in the worktree counts as activity, even with no other signal'
    Assert ([Math]::Abs(($result - $expectedMtime).TotalSeconds) -lt 5) `
        "the reported activity time matches the changed file's own mtime (got $result, expected ~$expectedMtime)"

    # A newer signal from elsewhere (e.g. a PR comment) still wins over an
    # OLDER worktree change - this is one more source into Get-LatestDate's
    # existing "pick the newest" combinator, not a replacement for it.
    $newerComment = (Get-Date).ToUniversalTime().AddMinutes(5)
    function Get-NewestReviewerCommentDate { param([string]$Branch) return $newerComment }
    $result2 = Get-PackageLastActivity -Package $gplaPackage -WorktreePath $gplaDir
    Assert ($result2 -eq $newerComment) 'a newer signal from an existing source still wins over the worktree mtime'

    # A worktree with no uncommitted changes at all contributes no signal -
    # git status --porcelain is empty, so there is nothing to take an mtime of.
    function Get-NewestReviewerCommentDate { param([string]$Branch) return $null }
    git -C $gplaDir add -A 2>$null
    git -C $gplaDir commit -m 'wip committed' --quiet 2>$null
    $result3 = Get-PackageLastActivity -Package $gplaPackage -WorktreePath $gplaDir
    Assert ($null -eq $result3) 'a clean worktree (nothing uncommitted) contributes no mtime signal - not "now"'
} finally {
    Remove-Item -Recurse -Force -Path $gplaDir -ErrorAction SilentlyContinue
}

Write-Host "== Invoke-DoctorPreflight (H11): fails fast with the doctor's own message, never with a generic one =="

# Restore the REAL Invoke-Native - every section above this one (the #188
# section pre-existing this file, and the garbage-output/staleness sections
# just above) leaves a shadowed one in place, and this test needs the real
# thing to actually invoke the stub below via `sh`.
Set-Item function:Invoke-Native -Value $script:RealInvokeNative

function New-DoctorStub {
    param([string]$Body)
    $dir = Join-Path $env:TEMP "wotest-doctor-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $path = Join-Path $dir 'doctor-stub'
    [IO.File]::WriteAllText($path, ($Body -replace "`r`n", "`n"))
    return $path
}
$savedDoctorScript = $script:DoctorScript
try {
    $script:DoctorScript = New-DoctorStub @'
#!/bin/sh
echo " 1. docker daemon                              FAIL - start Docker Desktop or the docker daemon"
exit 1
'@
    Reset-Log
    $threw = $false; $err = $null
    try { Invoke-DoctorPreflight } catch { $threw = $true; $err = $_.Exception.Message }
    Assert $threw 'a failing doctor stub stops the pre-flight before any worktree or session'
    Assert ($err -match 'start Docker Desktop or the docker daemon') "the thrown message carries the doctor's own remediation text ($err)"

    $script:DoctorScript = New-DoctorStub @'
#!/bin/sh
echo " 1. docker daemon                              ok"
exit 0
'@
    Reset-Log
    $threw = $false
    try { Invoke-DoctorPreflight } catch { $threw = $true }
    Assert (-not $threw) 'a passing doctor stub does not throw'
    Assert ((Get-LogText) -match 'scripts/doctor: all checks passed') 'and logs that the pre-flight passed'

    # #372 item 1: a real `sh` exec failure (not a doctor FAIL) - pointing
    # $script:DoctorScript at a path that does not exist at all, so `sh`
    # itself fails ("No such file or directory") rather than the stub script
    # running and exiting non-zero. `sh` writes that message to STDERR with
    # empty stdout, which Invoke-Native used to discard entirely, making the
    # thrown message empty.
    $script:DoctorScript = Join-Path $env:TEMP "wotest-doctor-missing-$([guid]::NewGuid().ToString('N'))"
    Reset-Log
    $threw = $false; $err = $null
    try { Invoke-DoctorPreflight } catch { $threw = $true; $err = $_.Exception.Message }
    Assert $threw 'sh failing to execute the doctor script at all still stops the pre-flight'
    Assert (-not [string]::IsNullOrWhiteSpace($err) -and $err.Trim() -ne 'scripts/doctor found a problem that must be fixed before starting anything:') `
        "the thrown message is not empty - it carries sh's own stderr, not a blank body (`"$err`")"
    Assert ($err -match 'No such file or directory') "the thrown message is sh's own raw stderr text, not PowerShell's formatted error-record noise (`"$err`")"
    Assert ($err -notmatch 'CategoryInfo|FullyQualifiedErrorId') "and carries no PowerShell error-record formatting (`"$err`")"
} finally {
    $script:DoctorScript = $savedDoctorScript
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}

Write-Host "== Start-ClaudeSession -DryRun: shows the resolved stale threshold per package (H11) =="

$script:DryRun = $true
Reset-Log
$dryRunSessionLog = ''
try {
    Start-ClaudeSession -WorktreePath 'C:\does-not-matter' -PromptText 'p' -WindowTitle 'Wave 1 - Test (#1)' `
        -Model 'claude-sonnet-5' -Effort 'high' -AdvisorModel $null -StaleAfterMinutes 30
    $dryRunSessionLog = Get-LogText
} finally {
    $script:DryRun = $false
    Remove-Item -Force -Path $script:LogFile -ErrorAction SilentlyContinue
}
Assert ($dryRunSessionLog -match 'stale threshold 30min') "-DryRun's per-package line shows the resolved stale threshold"

Write-Host ""
if ($script:failures -gt 0) {
    Write-Host "$($script:failures) assertion(s) FAILED" -ForegroundColor Red
    exit 1
} else {
    Write-Host "All assertions passed." -ForegroundColor Green
    exit 0
}
