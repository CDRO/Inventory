#Requires -Version 5.1
<#
.SYNOPSIS
    Generic wave orchestrator: reads a wave plan from a JSON file (default:
    scripts\wellen.json), waits for the previous wave, creates one worktree
    per package, starts a visible, interactive Claude Code session with the
    matching prompt/model/effort, tears down that package's own Docker stack
    the moment it is done (issue closed), kicks off consolidation once every
    package of the wave is done, removes what is left of the wave's Docker
    footprint (waves with "dockerCleanup": true - see .DOCKER CLEANUP for what
    that layer catches that the per-package teardown cannot), and moves on to
    the next wave.

    New waves are planned exclusively in the JSON file - this script never
    needs to change for that. How a wave is planned is described in
    scripts\wellen-planen.md (including the prompt for a Claude session that
    builds the plan).

.DOCKER ISOLATION
    Every package session runs its own `docker compose` invocations
    (CLAUDE.md's ship loop) inside its own worktree. Before starting
    anything, the script checks that docker compose actually has the rights
    it needs (`docker info` must succeed for the current user) and fails
    fast, with an actionable message, if it does not - a permission problem
    found only after nine sessions are already running helps no one.
    Once a worktree is created, its copied .env gets its own
    COMPOSE_PROJECT_NAME, HTTP_PORT, and TRAEFIK_PORT (deterministic per
    package, see Set-WorktreeEnvOverrides), so two worktrees running
    `docker compose up` at once - or a crashed session's orphaned
    containers sitting around in one - never collide on a host port or a
    container/network/volume name. The project name is sanitized (lowercased,
    anything outside [a-z0-9_-] replaced) before it is written, since it is
    built from the checkout's own directory name (e.g. "Inventory"), which
    Compose's project-name rule does not allow verbatim - see
    Get-SanitizedProjectName (#115). Full rationale:
    docs/specs/01-architecture-and-deployment.md, "Running more than one
    instance of the stack locally".

.DOCKER CLEANUP
    Two layers, at different times, for different problems.

    Per package, the moment its issue closes - Wait-ForIssueClosed returns,
    in a sequential wave; a round-robin poll finds it closed, in an
    unsequential one - Stop-PackageStack runs
    `docker compose down -v --remove-orphans` inside that worktree.
    `docker compose run` (the ship loop's own `go test`/`migrate` calls) never
    stops a dependency container it started - `db` keeps running after every
    invocation - and a session may also have brought the stack up with
    `docker compose up -d` for manual verification and never taken it back
    down. This step is independent of "dockerCleanup" and always runs: a
    finished package's own, uniquely-named project is safe to tear down
    unconditionally, and doing it immediately (rather than waiting for the
    wave to finish) is what keeps a long or parallel wave from accumulating
    containers, networks and bound host ports across packages that are
    already done. It runs a bare `docker compose down`, no `-f`, so it only
    ever selects the default files (docker-compose.yml/override.yml) -
    never docker-compose.e2e.yml, which Compose loads only via an explicit
    `-f` or COMPOSE_FILE that this call does not pass. That E2E stack pins
    its own project name in the file, but a worktree's own COMPOSE_PROJECT_NAME
    (from its .env, which Compose auto-loads regardless of -f) overrides a
    file's `name:` - confirmed live - so what that stack is actually named,
    and whether it is safe to blindly tear down at all, is genuinely unclear
    and is #190's question, not answered here. (#190 was split out of #140
    item 2, which is closed: #140 gave the wave cleanup a veto that makes it
    safe whatever the project is called, but did not settle the naming.)

    Per wave, for a wave with "dockerCleanup": true, the script also runs
    wellen-docker-cleanup.ps1 once the wave's consolidation is done (its wave
    issue is closed): it removes the Docker resources of that wave's package
    worktrees, decided by Docker's own labels, and never the main checkout's
    stack or the images that are shared with it. A failure is logged and does
    not stop the next wave. A wave that is already complete at start-up is
    cleaned as well; waves skipped with -StartWave are not. The orchestrator
    and the wave file are read once at start-up, so a running orchestrator
    does not pick up a change to either (the cleanup script is read afresh at
    every call). This layer exists for what the per-package step cannot
    reach: built images; a package's OWN further Compose project under a name
    of its own (Stop-PackageStack only ever runs a bare `docker compose down`,
    which touches only the worktree's default project - a project a session
    started under a different name for some check of its own is invisible to
    it); and any resource left by a package whose session crashed before its
    issue ever closed. Details and manual use: scripts\wellen-planen.md,
    "Docker cleanup after a wave".

.ARCHITECTURE
    This script itself makes NO git/GitHub write operations other than
    "worktree add" and copying .env - every substantive action (branching,
    committing, pushing, opening a PR, merging, running the reviewers) is
    done by the Claude Code session it starts. The only other thing it does
    is the Docker cleanup above. The script is only the
    metronome: it knows WHEN each prompt is due, and polls the same GitHub
    state a person would check by hand (issue open/closed).

    The integration branch of wave N+1 is NOT created by this script, but by
    wave N's consolidation session, as the last step of its prompt. The
    script only waits until wave N's wave issue is closed, then checks
    whether wave N+1's branch exists.

    Remote Control, model, effort, and advisor are passed to `claude` as CLI
    arguments - no UI automation. The advisor defaults to the same model as
    the session (the claude CLI has no separate advisor-effort flag yet;
    once it does, the rule "one effort step above the session, max stays
    max" belongs here).

.RESTART SAFETY
    On every (re)start, the script first checks GitHub state per package: if
    the spec issue is already closed, nothing is started. If the worktree
    already exists, it is likewise NOT started again (that would run two
    Claude sessions in the same working directory) - instead it only waits
    and prints a message that the location should be checked by hand. Only
    missing worktrees for still-open issues are created and started.

    Consolidation has no worktree to signal its state; instead a marker file
    (scripts\konsol-welle-<issue>.started, catches restarts of this script)
    and an open wave-branch -> main PR protect against a double start.
    Regardless: only ONE orchestrator per wave file.

.USAGE
    Validate the wave plan only (for the planning Claude session):
        .\wellen-orchestrator.ps1 -Validate

    Dry run, no real actions (shows only what would happen):
        .\wellen-orchestrator.ps1 -DryRun

    Normal start (production, actually waits and starts things):
        .\wellen-orchestrator.ps1

    A different wave file, or starting later:
        .\wellen-orchestrator.ps1 -WaveFile .\wellen-other.json -StartWave 3

    Administrator rights are NOT required. git, gh, and claude run under
    your own user account; elevation changes nothing about that. Since the
    prompt and Remote Control are passed as CLI arguments, it does not
    matter which window has focus while a session starts.
#>

[CmdletBinding()]
param(
    # Path to the wave plan file (schema: scripts\wellen-planen.md).
    # Empty = scripts\wellen.json next to this script.
    [string]$WaveFile = '',

    # Wave number the script starts orchestrating from. Waves before it are
    # skipped entirely, Docker cleanup included - clean those by hand.
    # 0 = all waves in the file. A wave whose issue is already closed has its
    # packages and its consolidation skipped, but is still Docker-cleaned when
    # it carries "dockerCleanup": true, since that is idempotent and the wave
    # may have finished under an orchestrator that did not clean (.DOCKER
    # CLEANUP).
    [int]$StartWave = 0,

    # How often (seconds) GitHub state is polled. 120s is plenty for a
    # multi-day run and goes easy on the API rate limit.
    [int]$PollSeconds = 120,

    # Only validates the wave file against the schema and exits - starts
    # nothing, needs no gh/claude. Meant for the planning Claude session to
    # verify its own draft.
    [switch]$Validate,

    # Shows only what the script would do, without creating worktrees or
    # opening windows.
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Base settings
# ---------------------------------------------------------------------------

if ([string]::IsNullOrWhiteSpace($WaveFile)) {
    $WaveFile = Join-Path $PSScriptRoot 'wellen.json'
}

$RepoRoot   = Split-Path -Path $PSScriptRoot -Parent
$RepoName   = Split-Path -Path $RepoRoot -Leaf
$ParentDir  = Split-Path -Path $RepoRoot -Parent
$LogFile    = Join-Path $PSScriptRoot 'wellen-orchestrator.log'
$GhRepo     = $null   # derived from origin after validation

# slug -> a stable, small, unique integer, assigned once the wave file is
# loaded (every package's ordinal position across every wave, in file
# order). Used only to derive a deterministic HTTP_PORT/TRAEFIK_PORT per
# worktree (below) - never used for anything that needs to survive editing
# the wave file, since inserting a package earlier in the file reassigns
# every later index.
$script:PackagePortIndex = @{}

# The first of a contiguous block of host ports handed out per package, one
# pair (HTTP_PORT, TRAEFIK_PORT) per package index - see
# Set-WorktreeEnvOverrides. Chosen high and round purely so a collision with
# something else the operator happens to run locally is unlikely; there is
# nothing more significant about the exact numbers.
$HttpPortBase    = 18000
$TraefikPortBase = 19000

$ValidEfforts = @('low', 'medium', 'high', 'xhigh', 'max')

function Write-Log {
    param([string]$Message, [string]$Level = 'INFO')
    $line = "[{0}] [{1}] {2}" -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Level, $Message
    Write-Host $line
    Add-Content -Path $LogFile -Value $line
}

# Reads an optional field from an object produced by ConvertFrom-Json; if
# missing or empty, returns the default.
function Get-Field {
    param($Object, [string]$Name, $Default = $null)
    if ($null -ne $Object -and $Object.PSObject.Properties[$Name]) {
        $value = $Object.$Name
        if ($null -ne $value -and -not ($value -is [string] -and [string]::IsNullOrWhiteSpace($value))) {
            return $value
        }
    }
    return $Default
}

# ---------------------------------------------------------------------------
# Load and validate the wave plan. Every violation is collected and reported
# with its path, so the planning session can fix it precisely.
# ---------------------------------------------------------------------------

function Import-WavePlan {
    param([string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) { throw "Wave file not found: $Path" }
    try {
        $data = Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json
    } catch {
        throw "Wave file is not valid JSON ($Path): $($_.Exception.Message)"
    }

    $errors = New-Object System.Collections.Generic.List[string]
    function Add-ErrorMsg { param([string]$Text) $errors.Add($Text) | Out-Null }

    # Free-text fields end up in a single-quoted here-string and in
    # single-quoted CLI arguments - these two patterns would break that
    # quoting and are therefore rejected right here.
    function Test-PromptText {
        param([string]$Path, $Value)
        if ($Value -and $Value -match "(?m)^'@") { Add-ErrorMsg "${Path}: must not contain a line starting with '@ (here-string terminator)" }
    }
    function Test-TitleText {
        param([string]$Path, $Value)
        if ($Value -and $Value -like "*'*") { Add-ErrorMsg "${Path}: must not contain a single quote (passed single-quoted to claude)" }
    }

    $plan = Get-Field $data 'plan'
    if ($null -eq $plan) { Add-ErrorMsg "plan: missing" }
    else {
        if (-not (Get-Field $plan 'name'))      { Add-ErrorMsg "plan.name: missing" }
        Test-PromptText 'plan.conventions' (Get-Field $plan 'conventions')
        $planIssue = Get-Field $plan 'planIssue' 0
        if ($planIssue -isnot [int] -and $planIssue -isnot [long]) { Add-ErrorMsg "plan.planIssue: must be an issue number (integer)" }
        elseif ($planIssue -le 0) { Add-ErrorMsg "plan.planIssue: missing or not a positive number" }
    }

    $standards = Get-Field $data 'standards'
    if ($null -eq $standards) { Add-ErrorMsg "standards: missing" }
    else {
        foreach ($field in @('model', 'consolidationModel')) {
            if (-not (Get-Field $standards $field)) { Add-ErrorMsg "standards.${field}: missing" }
        }
        foreach ($field in @('effort', 'consolidationEffort')) {
            $value = Get-Field $standards $field
            if (-not $value) { Add-ErrorMsg "standards.${field}: missing" }
            elseif ($ValidEfforts -notcontains $value) { Add-ErrorMsg "standards.${field}: '$value' is not a valid effort ($($ValidEfforts -join ', '))" }
        }
    }

    $waves = @(Get-Field $data 'waves' @())
    if ($waves.Count -eq 0) { Add-ErrorMsg "waves: missing or empty" }

    $allSlugs = @{}; $allBranches = @{}; $allSpecIssues = @{}; $allNumbers = @{}
    $previousNumber = 0

    for ($w = 0; $w -lt $waves.Count; $w++) {
        $wave = $waves[$w]
        $wPath = "waves[$w]"

        $number = Get-Field $wave 'number' 0
        if ($number -le 0) { Add-ErrorMsg "${wPath}.number: missing or not a positive number" }
        elseif ($allNumbers.ContainsKey($number)) { Add-ErrorMsg "${wPath}.number: $number appears twice" }
        else {
            $allNumbers[$number] = $true
            if ($number -le $previousNumber) { Add-ErrorMsg "${wPath}.number: $number is not ascending (previous wave: $previousNumber)" }
            $previousNumber = $number
        }

        if ((Get-Field $wave 'waveIssue' 0) -le 0) { Add-ErrorMsg "${wPath}.waveIssue: missing or not a positive number" }
        if (-not (Get-Field $wave 'integrationBranch')) { Add-ErrorMsg "${wPath}.integrationBranch: missing" }

        # Read the property itself: Get-Field turns "" and null into "not set", and
        # a value that is present but not a boolean must not silently mean false.
        $dockerCleanupProperty = $wave.PSObject.Properties['dockerCleanup']
        if ($null -ne $dockerCleanupProperty -and $dockerCleanupProperty.Value -isnot [bool]) {
            Add-ErrorMsg "${wPath}.dockerCleanup: must be true or false, not '$($dockerCleanupProperty.Value)'"
        }

        $external = [bool](Get-Field $wave 'external' $false)
        $packages = @(Get-Field $wave 'packages' @())
        if ($external) {
            if ($packages.Count -gt 0) { Add-ErrorMsg "${wPath}: an external wave must not have packages" }
            continue
        }
        if ($packages.Count -eq 0) { Add-ErrorMsg "${wPath}.packages: missing or empty (or mark the wave as external)" }

        for ($p = 0; $p -lt $packages.Count; $p++) {
            $package = $packages[$p]
            $pPath = "${wPath}.packages[$p]"

            if ((Get-Field $package 'specIssue' 0) -le 0) { Add-ErrorMsg "${pPath}.specIssue: missing or not a positive number" }
            else {
                $si = $package.specIssue
                if ($allSpecIssues.ContainsKey($si)) { Add-ErrorMsg "${pPath}.specIssue: #$si appears twice" } else { $allSpecIssues[$si] = $true }
            }

            $slug = Get-Field $package 'slug'
            if (-not $slug) { Add-ErrorMsg "${pPath}.slug: missing" }
            elseif ($slug -notmatch '^[a-z0-9][a-z0-9-]*$') { Add-ErrorMsg "${pPath}.slug: '$slug' may only contain lowercase letters, digits, and hyphens" }
            elseif ($allSlugs.ContainsKey($slug)) { Add-ErrorMsg "${pPath}.slug: '$slug' appears twice" }
            else { $allSlugs[$slug] = $true }

            $branch = Get-Field $package 'branch'
            if (-not $branch) { Add-ErrorMsg "${pPath}.branch: missing" }
            elseif ($allBranches.ContainsKey($branch)) { Add-ErrorMsg "${pPath}.branch: '$branch' appears twice" }
            else { $allBranches[$branch] = $true }

            if (-not (Get-Field $package 'spec'))  { Add-ErrorMsg "${pPath}.spec: missing" }
            Test-TitleText "${pPath}.spec" (Get-Field $package 'spec')
            if (-not (Get-Field $package 'focus')) { Add-ErrorMsg "${pPath}.focus: missing - the focus paragraph is mandatory, it carries the spec's guardrails into the prompt" }
            Test-PromptText "${pPath}.focus" (Get-Field $package 'focus')

            $effort = Get-Field $package 'effort'
            if ($effort -and $ValidEfforts -notcontains $effort) { Add-ErrorMsg "${pPath}.effort: '$effort' is not a valid effort ($($ValidEfforts -join ', '))" }
        }
    }

    if ($errors.Count -gt 0) {
        throw ("Wave file '$Path' is invalid ($($errors.Count) finding(s)):`n - " + ($errors -join "`n - "))
    }
    return $data
}

# ---------------------------------------------------------------------------
# Native commands whose failure this script wants to HANDLE
# ---------------------------------------------------------------------------

# Exit code of the last Invoke-Native call.
$script:NativeExit = 0

# Runs a native command (gh, git, docker, claude) with stderr discarded,
# returns its stdout, and leaves the exit code in $script:NativeExit.
#
# Under $ErrorActionPreference = 'Stop', Windows PowerShell 5.1 turns ANY
# stderr output of a native command into a terminating NativeCommandError -
# even with 2>$null, *> $null or 2>&1 (verified on 5.1). So a plain
# "gh ... 2>$null; if ($LASTEXITCODE -ne 0)" never reaches its own error
# handling: the first transient network error, rate limit, or stopped Docker
# daemon would end a multi-day run instead of being treated as "not yet" and
# retried on the next poll. Lowering the preference for just this call is the
# only form that works.
function Invoke-Native {
    param([scriptblock]$Command)
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = & $Command 2>$null
        $script:NativeExit = $LASTEXITCODE
        return $output
    } finally {
        $ErrorActionPreference = $previous
    }
}

# ---------------------------------------------------------------------------
# GitHub state (read-only)
# ---------------------------------------------------------------------------

function Get-IssueState {
    param([int]$Number)
    $json = Invoke-Native { gh issue view $Number --repo $GhRepo --json state -q '.state' }
    if ($script:NativeExit -ne 0 -or [string]::IsNullOrWhiteSpace($json)) {
        return $null
    }
    return $json.Trim()
}

function Test-IssueClosed {
    param([int]$Number)
    return (Get-IssueState -Number $Number) -eq 'CLOSED'
}

function Wait-ForIssueClosed {
    param([int]$Number, [string]$Description)
    Write-Log "Waiting for issue #$Number ($Description) ..."
    while (-not (Test-IssueClosed -Number $Number)) {
        Start-Sleep -Seconds $PollSeconds
    }
    Write-Log "Issue #$Number ($Description) is closed."
}

function Test-RemoteBranchExists {
    param([string]$Branch)
    Invoke-Native { git -C $RepoRoot ls-remote --exit-code --heads origin $Branch } | Out-Null
    return ($script:NativeExit -eq 0)
}

function Wait-ForRemoteBranch {
    param([string]$Branch, [int]$TimeoutSeconds = 900)
    Write-Log "Checking whether branch '$Branch' exists on origin ..."
    $elapsed = 0
    while (-not (Test-RemoteBranchExists -Branch $Branch)) {
        if ($elapsed -ge $TimeoutSeconds) {
            Write-Log "Branch '$Branch' still does not exist after $TimeoutSeconds s. The wave issue is closed, but the next wave's branch is missing - please check by hand (the consolidation session may not have completed its last step)." 'WARN'
            return $false
        }
        Start-Sleep -Seconds 15
        $elapsed += 15
    }
    Write-Log "Branch '$Branch' exists."
    return $true
}

# ---------------------------------------------------------------------------
# Worktrees and Claude sessions
# ---------------------------------------------------------------------------

# Ensures (adds or replaces) a KEY=value line in a .env file. Used to
# override, not append blindly - a worktree's .env starts as a copy of the
# main checkout's and may already define the same keys.
function Set-EnvValue {
    param([string]$EnvPath, [string]$Key, [string]$Value)
    # .NET IO on purpose, not Get-Content/Set-Content: on Windows PowerShell
    # 5.1 those read a BOM-less file as ANSI and write ANSI with CRLF, which
    # would quietly re-encode a UTF-8/LF .env (the setup wizard writes one)
    # and could corrupt a non-ASCII secret. UTF-8 without BOM, LF, matches
    # what `docker compose run --rm setup` produced. $EnvPath must be absolute.
    $lines = @()
    if (Test-Path -LiteralPath $EnvPath) { $lines = @([System.IO.File]::ReadAllLines($EnvPath)) }
    $pattern = "^$([regex]::Escape($Key))="
    $found = $false
    $lines = @($lines | ForEach-Object {
        if ($_ -match $pattern) { $found = $true; "$Key=$Value" } else { $_ }
    })
    if (-not $found) { $lines += "$Key=$Value" }
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($EnvPath, (($lines -join "`n") + "`n"), $utf8NoBom)
}

# Docker Compose's project-name rule (v2.31.0, confirmed live): "must consist
# only of lowercase alphanumeric characters, hyphens, and underscores as well
# as start with a letter or number". $RepoName is a checkout's directory
# name, which is free-form and, for this repo, starts with a capital
# ("Inventory") - passing "$RepoName-$Slug" straight through makes EVERY
# `docker compose` invocation in EVERY worktree fail before it does anything
# (#115). Compose's own default project-name derivation (no
# COMPOSE_PROJECT_NAME set at all) already lowercases the directory name for
# exactly this reason; this function applies the same rule to the name this
# script constructs explicitly, rather than relying on every caller to
# remember it.
function Get-SanitizedProjectName {
    param([string]$Value)
    $sanitized = $Value.ToLowerInvariant() -replace '[^a-z0-9_-]', '-'
    if ($sanitized -notmatch '^[a-z0-9]') { $sanitized = "x-$sanitized" }
    return $sanitized
}

# Gives a package worktree's .env its own COMPOSE_PROJECT_NAME, HTTP_PORT,
# and TRAEFIK_PORT (see "Running more than one instance of the stack
# locally" in docs/specs/01-architecture-and-deployment.md), so that
# `docker compose up` (or `run`) in two worktrees at once - or a crashed
# session's containers still sitting around in one - never collides with
# another worktree's containers, network, volumes, or host ports. This is
# the only place the orchestrator changes a copied .env's content; every
# other value (secrets, POSTGRES_*, GEMINI_*, …) is passed through
# unmodified from the main checkout.
function Set-WorktreeEnvOverrides {
    param([string]$EnvPath, [string]$Slug)
    if (-not $script:PackagePortIndex.ContainsKey($Slug)) {
        throw "No port index assigned for slug '$Slug' - this is an orchestrator bug, not a wave-file problem (PackagePortIndex should be populated for every package before any worktree is created)."
    }
    $index = $script:PackagePortIndex[$Slug]
    $projectName = Get-SanitizedProjectName "$RepoName-$Slug"
    $httpPort = $HttpPortBase + $index
    $traefikPort = $TraefikPortBase + $index
    Set-EnvValue -EnvPath $EnvPath -Key 'COMPOSE_PROJECT_NAME' -Value $projectName
    Set-EnvValue -EnvPath $EnvPath -Key 'HTTP_PORT' -Value $httpPort
    Set-EnvValue -EnvPath $EnvPath -Key 'TRAEFIK_PORT' -Value $traefikPort
    Write-Log "Worktree env for '$Slug': COMPOSE_PROJECT_NAME=$projectName HTTP_PORT=$httpPort TRAEFIK_PORT=$traefikPort"
}

function New-PackageWorktree {
    param([string]$Slug, [string]$Branch, [string]$BaseBranch)
    $path = Join-Path $ParentDir "$RepoName-$Slug"
    if (Test-Path $path) {
        Write-Log "Worktree '$path' already exists - not created again." 'WARN'
        return $path
    }
    if ($DryRun) {
        Write-Log "[DryRun] would create: git worktree add --no-track -b $Branch $path origin/$BaseBranch"
        return $path
    }
    git -C $RepoRoot fetch origin | Out-Null
    git -C $RepoRoot worktree add --no-track -b $Branch $path "origin/$BaseBranch"
    if ($LASTEXITCODE -ne 0) {
        if (Test-Path $path) {
            throw "git worktree add for '$Slug' failed, and the path now exists - probably a second orchestrator run created it in parallel. Only ONE metronome per wave file is supported; stop this run or the other one."
        }
        throw "git worktree add for '$Slug' failed."
    }
    $worktreeEnv = Join-Path $path '.env'
    Copy-Item -Path (Join-Path $RepoRoot '.env') -Destination $worktreeEnv -Force
    Set-WorktreeEnvOverrides -EnvPath $worktreeEnv -Slug $Slug
    Write-Log "Worktree '$path' created (branch '$Branch' from origin/$BaseBranch)."
    return $path
}

# Starts a visible, interactive Claude Code session. Prompt, model, effort,
# advisor, and Remote Control are passed to claude as CLI arguments - no UI
# automation, no focus dependency, no clipboard. The inner script is passed
# Base64-encoded (-EncodedCommand) so the multi-line prompt causes no
# quoting problem. In exchange, the prompt must not contain a line starting
# exactly with '@ (here-string terminator).
function Start-ClaudeSession {
    param(
        [string]$WorktreePath,
        [string]$PromptText,
        [string]$WindowTitle,
        [string]$Model,
        [string]$Effort,
        [string]$AdvisorModel   # empty = no advisor
    )
    $advisorArg = if ($AdvisorModel) { "--advisor '$AdvisorModel' " } else { '' }
    if ($DryRun) {
        Write-Log "[DryRun] would start Claude session in '$WorktreePath' titled '$WindowTitle': claude --model $Model --effort $Effort $($advisorArg)--remote-control '$WindowTitle' <prompt as argument>"
        return
    }

    $inner = @"
`$Host.UI.RawUI.WindowTitle = '$WindowTitle'
Set-Location -LiteralPath '$WorktreePath'
`$prompt = @'
$PromptText
'@
claude --model '$Model' --effort '$Effort' $advisorArg--remote-control '$WindowTitle' `$prompt
"@
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($inner))

    $proc = Start-Process -FilePath 'powershell.exe' `
        -ArgumentList @('-NoExit', '-EncodedCommand', $encoded) `
        -WorkingDirectory $WorktreePath -PassThru

    Write-Log "Claude session started: '$WindowTitle' in '$WorktreePath' (PID $($proc.Id)) - model $Model, effort $Effort$(if ($AdvisorModel) { ", advisor $AdvisorModel" }), Remote Control and prompt passed as CLI arguments."
}

# ---------------------------------------------------------------------------
# Resolving model/effort/advisor per session
# ---------------------------------------------------------------------------

# A session's advisor model: defaults to the same model as the session
# itself; overridable per package/wave via 'advisorModel', switchable off
# via 'advisor': false. The claude CLI currently has NO configurable
# advisor effort - the intended rule "one step above the session, max stays
# max" can therefore not (yet) be implemented; once the CLI offers it, it
# belongs here.
function Get-AdvisorModel {
    param($Standards, $Context, [string]$SessionModel)
    $enabled = [bool](Get-Field $Context 'advisor' (Get-Field $Standards 'advisor' $true))
    if (-not $enabled) { return $null }
    return (Get-Field $Context 'advisorModel' (Get-Field $Standards 'advisorModel' $SessionModel))
}

# ---------------------------------------------------------------------------
# Prompt builders, parametrized purely from the wave file's fields.
# ---------------------------------------------------------------------------

function Get-PackagePrompt {
    param($Plan, $Standards, $Wave, $Package)
    $limit = Get-Field $Standards 'roundLimitPackage' 2
    $conventions = Get-Field $Plan 'conventions' ''
    if ($conventions) { $conventions = "$conventions " }
    return @"
/pickup

Work $($Package.spec) (issue #$($Package.specIssue), wave $($Wave.number) of $($Plan.name)) through the full ship loop, here in this worktree, with the PR against $($Wave.integrationBranch) instead of main - wave plan #$($Plan.planIssue) takes precedence over the ship skill's default target. Open PRs belonging to other packages belong to parallel sessions: do not touch them, do not ask about them. $($Package.focus) ${conventions}After the merge, close the spec issue, comment on wave issue #$($Wave.waveIssue), and do not switch to main. Stop and report if you hit the round limit ($limit).
"@
}

function Get-ConsolidationPrompt {
    param($Plan, $Standards, $Wave, $NextWave)
    $limit = Get-Field $Standards 'roundLimitConsolidation' 4
    $branch = $Wave.integrationBranch
    # A session must not clean Docker up itself: it runs in the main checkout,
    # whose own stack and shared images are not the wave's to remove.
    $dockerNote = if ([bool](Get-Field $Wave 'dockerCleanup' $false)) {
        "Do not remove Docker resources yourself and never run docker system, volume, network or image prune: once you are done, the orchestrator removes the Docker resources that this wave's package worktrees created. "
    } else { '' }
    $tail = if ($null -ne $NextWave) {
        "After the merge: close the wave issue, comment on wave plan #$($Plan.planIssue), delete the wave branch, and create $($NextWave.integrationBranch) from current origin/main and push it - no commit, no PR, so wave $($NextWave.number) can start immediately."
    } else {
        "After the merge: close the wave issue, delete the wave branch, and close wave plan #$($Plan.planIssue) - $($Plan.name) is then complete."
    }
    return @"
/pickup

Start the consolidation of wave $($Wave.number) of $($Plan.name) (wave issue #$($Wave.waveIssue), wave plan #$($Plan.planIssue)). First check: every package of this wave is merged and no PR against $branch is still open. Then merge origin/main into $branch (a merge commit, no rebase, no force-push), note every conflict resolution, run both suites, and open the PR $branch -> main, with Closes for every spec issue in this wave. Then the review loop with round limit $limit instead of 2: all THREE reviewers (review-go, review-tests, review-docs) get the full diff main...$branch and the list of package PRs. Merge with a merge commit once all three approve in the same round and the suite is green. Whatever is still open after round $limit becomes an issue and is named with its risk in the report. ${dockerNote}$tail Stop and report if you hit the round limit ($limit).
"@
}

# ---------------------------------------------------------------------------
# Docker cleanup after a wave
# ---------------------------------------------------------------------------

# For a wave with "dockerCleanup": true, removes what its package worktrees
# left in Docker (containers, networks, volumes, the images built for them),
# by running wellen-docker-cleanup.ps1 - see its header for how ownership is
# decided and what is never touched. Run once the wave's consolidation has
# finished. The script is called with -Wave and checks for itself, through the
# wave issue, that the wave is over, and refuses otherwise. It runs as a job with
# a time limit, and a failure or a timeout only logs: the next wave must not wait
# on housekeeping. Idempotent.
$DockerCleanupScript = Join-Path $PSScriptRoot 'wellen-docker-cleanup.ps1'
$DockerCleanupTimeoutSeconds = 900

function Invoke-WaveDockerCleanup {
    param($Wave)
    if (-not [bool](Get-Field $Wave 'dockerCleanup' $false)) { return }
    $slugs = @(@(Get-Field $Wave 'packages' @()) | ForEach-Object { $_.slug })
    if ($slugs.Count -eq 0) { return }
    if ($DryRun) {
        Write-Log "[DryRun] would remove the Docker resources of wave $($Wave.number)'s package worktrees ($($slugs -join ', ')): $DockerCleanupScript -Wave $($Wave.number)"
        return
    }
    Write-Log "Removing the Docker resources of wave $($Wave.number)'s package worktrees ($($slugs -join ', '))."
    $params = @{ Wave = [int]$Wave.number; WaveFile = $WaveFile; RepoRoot = $RepoRoot; GhRepo = [string]$GhRepo }
    $job = $null
    try {
        $job = Start-Job -ScriptBlock { param($script, $p) & $script @p } -ArgumentList $DockerCleanupScript, $params
        if (-not (Wait-Job -Job $job -Timeout $DockerCleanupTimeoutSeconds)) {
            Stop-Job -Job $job
            Write-Log "Docker cleanup of wave $($Wave.number) did not finish within $DockerCleanupTimeoutSeconds s and was stopped - continuing. Run scripts\wellen-docker-cleanup.ps1 -Wave $($Wave.number) -DryRun by hand." 'WARN'
            return
        }
        foreach ($line in @(Receive-Job -Job $job -ErrorAction SilentlyContinue)) {
            $level = if ("$line" -match '^\s*WARNING') { 'WARN' } else { 'INFO' }
            Write-Log "  $line" $level
        }
        if ($job.State -eq 'Failed') {
            $reason = $job.ChildJobs[0].JobStateInfo.Reason.Message
            Write-Log "Docker cleanup of wave $($Wave.number) did not run: $reason - continuing. Run scripts\wellen-docker-cleanup.ps1 -Wave $($Wave.number) by hand once it is safe." 'WARN'
        }
    } catch {
        Write-Log "Docker cleanup of wave $($Wave.number) failed: $($_.Exception.Message) - continuing. Run scripts\wellen-docker-cleanup.ps1 -Wave $($Wave.number) by hand." 'WARN'
    } finally {
        if ($null -ne $job) { Remove-Job -Job $job -Force -ErrorAction SilentlyContinue }
    }
}

# ---------------------------------------------------------------------------
# Orchestrate one wave: wait for its branch, start/wait for packages, start
# consolidation, wait for the wave issue.
# ---------------------------------------------------------------------------

function Invoke-Wave {
    param($Plan, $Standards, $Wave, $NextWave)

    $n = $Wave.number
    $branch = $Wave.integrationBranch
    Write-Log "=== Wave ${n}: start ($branch, wave issue #$($Wave.waveIssue)) ==="

    if (Test-IssueClosed -Number $Wave.waveIssue) {
        Write-Log "Wave $n is already complete (issue #$($Wave.waveIssue) closed) - skipping."
        # It may have finished under an orchestrator that did not know about the
        # cleanup yet (or before it was switched on): the cleanup is idempotent.
        Invoke-WaveDockerCleanup -Wave $Wave
        return
    }

    if ([bool](Get-Field $Wave 'external' $false)) {
        Write-Log "Wave $n runs outside this script (external) - only waiting for it to close."
        if (-not $DryRun) {
            Wait-ForIssueClosed -Number $Wave.waveIssue -Description "wave $n (external)"
        }
        return
    }

    if (-not $DryRun) {
        Wait-ForRemoteBranch -Branch $branch | Out-Null
    }

    $sequential = [bool](Get-Field $Wave 'sequential' $false)
    if ($sequential) {
        foreach ($package in $Wave.packages) {
            Invoke-Package -Plan $Plan -Standards $Standards -Wave $Wave -Package $package
            if (-not $DryRun) {
                Wait-ForIssueClosed -Number $package.specIssue -Description $package.spec
            }
            # Unconditional, not "if (-not $DryRun)": Stop-PackageStack checks
            # $DryRun itself (like every other action function here), which is
            # what makes a -DryRun run log that a teardown would happen here
            # too, instead of silently skipping it.
            Stop-PackageStack -WorktreePath (Join-Path $ParentDir "$RepoName-$($package.slug)") -Slug $package.slug
        }
    } else {
        foreach ($package in $Wave.packages) {
            Invoke-Package -Plan $Plan -Standards $Standards -Wave $Wave -Package $package
        }
        if ($DryRun) {
            foreach ($package in $Wave.packages) {
                Stop-PackageStack -WorktreePath (Join-Path $ParentDir "$RepoName-$($package.slug)") -Slug $package.slug
            }
        } else {
            # Poll every still-open package together, tearing each one's
            # stack down the moment ITS OWN issue closes - never in file
            # order. A package listed later that finishes first must not
            # wait for an earlier-listed sibling still running in the same
            # (unsequential) wave; the sequential branch above has no such
            # problem; a package-at-a-time wait+teardown there is already
            # "the moment its issue closes" for that package.
            $pending = [System.Collections.Generic.List[object]]::new()
            foreach ($package in $Wave.packages) {
                $pending.Add($package)
                Write-Log "Waiting for issue #$($package.specIssue) ($($package.spec)) ..."
            }
            while ($pending.Count -gt 0) {
                $stillPending = [System.Collections.Generic.List[object]]::new()
                foreach ($package in $pending) {
                    if (Test-IssueClosed -Number $package.specIssue) {
                        Write-Log "Issue #$($package.specIssue) ($($package.spec)) is closed."
                        Stop-PackageStack -WorktreePath (Join-Path $ParentDir "$RepoName-$($package.slug)") -Slug $package.slug
                    } else {
                        $stillPending.Add($package)
                    }
                }
                $pending = $stillPending
                if ($pending.Count -gt 0) { Start-Sleep -Seconds $PollSeconds }
            }
        }
    }

    Write-Log "All packages of wave $n are done."

    # Double-start protection for consolidation: unlike packages, there is no
    # worktree whose existence would reveal a running session. Two signals
    # substitute for it: a local marker file (catches restarts of THIS
    # script) and an open branch -> main PR (catches any other metronome,
    # once its consolidation session has opened its PR). There remains a
    # blind window between a foreign session starting and it opening its PR
    # - two orchestrators on the same wave file are therefore still not
    # supported.
    $marker = Join-Path $PSScriptRoot "konsol-welle-$($Wave.waveIssue).started"
    $openPr = $null
    if (-not $DryRun) {
        $openPr = Invoke-Native { gh pr list --repo $GhRepo --head $branch --base main --state open --json number -q '.[0].number' }
        if ($script:NativeExit -ne 0) { $openPr = $null }
    }
    if ((Test-Path $marker) -or -not [string]::IsNullOrWhiteSpace($openPr)) {
        $reason = if (Test-Path $marker) { "marker file '$marker'" } else { "open PR #$openPr ($branch -> main)" }
        Write-Log "Consolidation of wave $n already appears to be running ($reason) - no new session started, only waiting for completion." 'WARN'
    } else {
        Write-Log "Starting consolidation."
        $consolModel = Get-Field $Wave 'consolidationModel' (Get-Field $Standards 'consolidationModel')
        $consolEffort = Get-Field $Wave 'consolidationEffort' (Get-Field $Standards 'consolidationEffort')
        $consolAdvisor = Get-AdvisorModel -Standards $Standards -Context $Wave -SessionModel $consolModel
        $consolPrompt = Get-ConsolidationPrompt -Plan $Plan -Standards $Standards -Wave $Wave -NextWave $NextWave
        Start-ClaudeSession -WorktreePath $RepoRoot -PromptText $consolPrompt -WindowTitle "Wave $n - Consolidation" `
            -Model $consolModel -Effort $consolEffort -AdvisorModel $consolAdvisor
        if (-not $DryRun) {
            Set-Content -Path $marker -Value (Get-Date -Format 'o')
        }
    }

    if (-not $DryRun) {
        Wait-ForIssueClosed -Number $Wave.waveIssue -Description "consolidation wave $n"
        Remove-Item -Path $marker -ErrorAction SilentlyContinue
    }
    Invoke-WaveDockerCleanup -Wave $Wave
    Write-Log "=== Wave ${n}: done ==="
}

# Tears down whatever a finished package's Claude session left running in its
# own worktree, the moment its issue closes. `docker compose run` (the ship
# loop's own `go test`/`migrate` calls) never stops a dependency container it
# started - `db` keeps running after every invocation via `depends_on` - and
# a session may separately have brought the stack up with `docker compose up
# -d` for manual verification and never taken it back down. Run from inside
# the worktree so Compose auto-loads its own .env - the isolated
# COMPOSE_PROJECT_NAME Set-WorktreeEnvOverrides wrote there - which is what
# makes this safe to call unconditionally: it only ever touches the one
# project that worktree's own base/override files ever created. `-v` removes
# that project's named volumes (pgdata, uploads, imagecache): a package
# worktree is single-purpose and its own data is not meant to outlive it.
# Deliberately a bare `docker compose down`, no `-f`: this command only ever
# LOADS docker-compose.yml/override.yml, never docker-compose.e2e.yml (which
# needs an explicit `-f` or COMPOSE_FILE). That is not the same as "never
# touched", though: `--remove-orphans` removes any container already in the
# loaded PROJECT whose service is not in the loaded FILES, so an E2E stack
# that landed in this same project would be swept as an orphan, not skipped.
# Whether it CAN land in this project depends on naming this function does
# not control: docker-compose.e2e.yml pins `name: inventory-e2e`, but a
# worktree's own COMPOSE_PROJECT_NAME overrides a file's `name:` - confirmed
# live - so whether an E2E run from inside this worktree ends up isolated or
# shares this project is genuinely unclear. Real isolation for it is #190's
# job, not answered here. Best-effort and non-fatal: a package that
# never brought anything up simply has nothing to remove.
function Stop-PackageStack {
    param([string]$WorktreePath, [string]$Slug)
    if ($DryRun) {
        Write-Log "[DryRun] would run 'docker compose down -v --remove-orphans' for '$Slug' in $WorktreePath"
        return
    }
    if (-not (Test-Path -LiteralPath $WorktreePath)) {
        # A package whose issue was already closed before this run started
        # (Invoke-Package's own early-return) never gets a worktree here -
        # nothing to tear down, and Push-Location on a missing path would
        # otherwise throw under this script's $ErrorActionPreference = 'Stop'.
        return
    }
    Push-Location -LiteralPath $WorktreePath
    try {
        Invoke-Native { docker compose down -v --remove-orphans } | Out-Null
        if ($script:NativeExit -eq 0) {
            Write-Log "Stopped the Docker stack for '$Slug'."
        } else {
            Write-Log "'docker compose down' for '$Slug' exited $($script:NativeExit) - continuing (it may never have brought anything up)." 'WARN'
        }
    } finally {
        Pop-Location
    }
}

function Invoke-Package {
    param($Plan, $Standards, $Wave, $Package)

    if (-not $DryRun -and (Test-IssueClosed -Number $Package.specIssue)) {
        Write-Log "Package '$($Package.spec)' (#$($Package.specIssue)) is already done - skipping."
        return
    }

    $worktreePath = Join-Path $ParentDir "$RepoName-$($Package.slug)"
    $alreadyExists = Test-Path $worktreePath

    New-PackageWorktree -Slug $Package.slug -Branch $Package.branch -BaseBranch $Wave.integrationBranch | Out-Null

    if ($alreadyExists) {
        Write-Log "Worktree for '$($Package.spec)' already existed - no new session started, only waiting for completion. If no session is running there anymore, please check by hand." 'WARN'
        return
    }

    $model = Get-Field $Package 'model' (Get-Field $Standards 'model')
    $effort = Get-Field $Package 'effort' (Get-Field $Standards 'effort')
    $advisor = Get-AdvisorModel -Standards $Standards -Context $Package -SessionModel $model
    $prompt = Get-PackagePrompt -Plan $Plan -Standards $Standards -Wave $Wave -Package $Package
    Start-ClaudeSession -WorktreePath $worktreePath -PromptText $prompt `
        -WindowTitle "Wave $($Wave.number) - $($Package.spec) (#$($Package.specIssue))" `
        -Model $model -Effort $effort -AdvisorModel $advisor
}

# ---------------------------------------------------------------------------
# Main flow
# ---------------------------------------------------------------------------

$data = Import-WavePlan -Path $WaveFile
$plan = $data.plan
$standards = $data.standards
$waves = @($data.waves)

if ($Validate) {
    Write-Host "Wave file '$WaveFile' is valid: plan '$($plan.name)' (wave plan #$($plan.planIssue)), $($waves.Count) wave(s), $((@($waves | ForEach-Object { @(Get-Field $_ 'packages' @()).Count }) | Measure-Object -Sum).Sum) package(s)."
    exit 0
}

# Every package's ordinal position across the whole file, in file order -
# see Set-WorktreeEnvOverrides. Computed once, here, so it stays stable for
# the life of this run regardless of which packages are already done.
$portIndex = 0
foreach ($wave in $waves) {
    foreach ($package in @(Get-Field $wave 'packages' @())) {
        $script:PackagePortIndex[$package.slug] = $portIndex
        $portIndex++
    }
}

Write-Log "Orchestrator started. WaveFile=$WaveFile Plan='$($plan.name)' StartWave=$StartWave PollSeconds=$PollSeconds DryRun=$($DryRun.IsPresent)"

if (-not (Get-Command git -ErrorAction SilentlyContinue)) { throw "git not found on PATH." }
if (-not (Get-Command gh  -ErrorAction SilentlyContinue)) { throw "gh (GitHub CLI) not found on PATH." }
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) {
    throw "claude not found on PATH in THIS PowerShell process. " +
          "This script must be started from the same kind of PowerShell window where 'claude' normally works " +
          "(not from a heavily restricted/automated environment). Open a new PowerShell window and try again."
}
if (-not (Invoke-Native { claude --help } | Out-String).Contains('--remote-control')) {
    throw "The installed claude version does not know the --remote-control flag. Please run 'claude update' - this script relies on passing Remote Control and the prompt as CLI arguments."
}
if (-not (Test-Path (Join-Path $RepoRoot '.env'))) { throw ".env is missing in the repo root - every worktree needs a copy of it." }

# Every package session runs `docker compose run --rm app go test ./...`
# (CLAUDE.md) as part of its own ship loop, in its own worktree, so this
# has to work BEFORE any worktree/session is created - a permission problem
# discovered only after nine sessions are already running is nine sessions
# stuck at the same point. `docker compose version` only proves the CLI and
# plugin exist; `docker info` is what actually needs the daemon and
# therefore actually proves the current user can reach it.
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw "docker not found on PATH." }
Invoke-Native { docker compose version } | Out-Null
if ($script:NativeExit -ne 0) { throw "docker compose (the CLI plugin) is not available - install/update Docker Desktop or the compose plugin." }
Invoke-Native { docker info } | Out-Null
if ($script:NativeExit -ne 0) {
    throw "docker compose does not have the rights it needs: 'docker info' failed, which means the Docker daemon is either not running or not reachable by this user (on Windows/macOS: start Docker Desktop and wait until it reports running; on Linux: typically this user is not in the 'docker' group, or the socket needs sudo). Fix that first - every package session will otherwise fail its own ship loop at the same first 'docker compose run' in every worktree."
}

$GhRepo = (Invoke-Native { gh repo view --json nameWithOwner -q '.nameWithOwner' })
if ($script:NativeExit -ne 0 -or [string]::IsNullOrWhiteSpace($GhRepo)) {
    throw "Could not derive the GitHub repository from origin (gh repo view). Is gh logged in, and is origin set?"
}
$GhRepo = $GhRepo.Trim()
Write-Log "GitHub repository: $GhRepo"

for ($i = 0; $i -lt $waves.Count; $i++) {
    $wave = $waves[$i]
    if ($StartWave -gt 0 -and $wave.number -lt $StartWave) {
        Write-Log "Wave $($wave.number) is before StartWave=$StartWave - skipping (assumed complete)."
        continue
    }
    $next = if ($i + 1 -lt $waves.Count) { $waves[$i + 1] } else { $null }
    Invoke-Wave -Plan $plan -Standards $standards -Wave $wave -NextWave $next
}

Write-Log "All waves in the file are done. '$($plan.name)' complete."
