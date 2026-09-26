#Requires -Version 5.1
<#
.SYNOPSIS
    Tests for the Docker-facing pieces of wellen-orchestrator.ps1:
    Get-SanitizedProjectName / Set-WorktreeEnvOverrides (#115),
    Stop-PackageStack, Invoke-WaveDockerCleanup and the wave file's
    "dockerCleanup" validation (#140 item 9).

.DESCRIPTION
    The script under test is a top-to-bottom orchestrator, not a module - its
    own "Main flow" section unconditionally calls Import-WavePlan, checks
    `docker info`, and (with -Validate) exits, which would make a plain
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
    Needs Docker (the busybox image is pulled on first use). Takes about a
    minute.
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
    throw "Could not find the '# Main flow' marker in $orchestratorScript - has it moved? This test's dot-source split depends on it staying above every side-effecting call (docker info, Import-WavePlan, exit)."
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

Write-Host "== Invoke-Wave (parallel branch): teardown follows ACTUAL close order (#188) =="

# The parallel branch's own polling loop (wellen-orchestrator.ps1, the `while
# ($pending.Count -gt 0)` loop inside Invoke-Wave) was never exercised: this
# drives the real Invoke-Wave with two fake packages where the SECOND-listed
# one ('iw-b') closes FIRST, and asserts Stop-PackageStack fires in that
# close order, not file order. Test-IssueClosed, Stop-PackageStack,
# Invoke-Package and Start-Sleep are shadowed per the deferred review's own
# suggestion; Invoke-Native is shadowed too, purely so the surrounding
# consolidation step it also runs (a `git ls-remote` and a `gh pr list`) never
# makes a real call - it is made to look like a PR is already open, so
# Invoke-Wave logs and skips instead of starting a second session.
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

Write-Host ""
if ($script:failures -gt 0) {
    Write-Host "$($script:failures) assertion(s) FAILED" -ForegroundColor Red
    exit 1
} else {
    Write-Host "All assertions passed." -ForegroundColor Green
    exit 0
}
