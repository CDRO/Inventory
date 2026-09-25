#Requires -Version 5.1
<#
.SYNOPSIS
    Tests for the two Docker-isolation pieces of wellen-orchestrator.ps1:
    Get-SanitizedProjectName (#115) and Stop-PackageStack.

.DESCRIPTION
    The script under test is a top-to-bottom orchestrator, not a module - its
    own "Main flow" section unconditionally calls Import-WavePlan, checks
    `docker info`, and (with -Validate) exits, which would make a plain
    dot-source of the whole file run (part of) a real orchestration pass.
    This test instead dot-sources only the portion BEFORE the
    "# Main flow" marker comment - every function definition and the handful
    of side-effect-free variable assignments above it - into its own scope,
    so the real function bodies are under test without any of that.

    Get-SanitizedProjectName is pure and tested directly. Stop-PackageStack
    is tested against a real, disposable Compose project in a scratch
    directory (this repo's own convention for these scripts: see
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
    Assert ($got -eq $case.Out) "'$($case.In)' -> '$got' (expected '$($case.Out)')"
    Assert ($got -match '^[a-z0-9][a-z0-9_-]*$') "'$got' matches Compose's own project-name rule"
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
    Set-Content -Path (Join-Path $Dir 'docker-compose.yml') -Encoding ASCII -Value @"
services:
  app:
    image: busybox
    command: ["sleep", "3600"]
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
    if (Test-Path -LiteralPath $otherDir) {
        Push-Location -LiteralPath $otherDir
        try { Dk compose down -v --remove-orphans } finally { Pop-Location }
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
