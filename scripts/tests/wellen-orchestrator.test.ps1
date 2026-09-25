#Requires -Version 5.1
<#
.SYNOPSIS
    Tests for the two Docker-isolation pieces of wellen-orchestrator.ps1:
    Get-SanitizedProjectName / Set-WorktreeEnvOverrides (#115) and
    Stop-PackageStack.

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

    Run:  powershell -NoProfile -File scripts\tests\wellen-orchestrator.test.ps1
    Needs Docker (the busybox image is pulled on first use). Takes well under
    a minute.
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

Write-Host ""
if ($script:failures -gt 0) {
    Write-Host "$($script:failures) assertion(s) FAILED" -ForegroundColor Red
    exit 1
} else {
    Write-Host "All assertions passed." -ForegroundColor Green
    exit 0
}
