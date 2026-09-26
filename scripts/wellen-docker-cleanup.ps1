#Requires -Version 5.1
<#
.SYNOPSIS
    Removes the Docker resources that a finished wave's package worktrees
    created: containers (with their anonymous volumes), networks, named
    volumes and the images built for them. Called by the wave orchestrator
    after a wave's consolidation (for waves with "dockerCleanup": true in the
    wave file), and usable by hand for a wave that is finished.

.DESCRIPTION
    Package sessions run `docker compose` in their own worktree
    (<parent>\<repo>-<slug>). That leaves, per worktree and per compose
    project: a database container, a default network, three named volumes
    (pgdata, uploads, imagecache) and one or two built images - and sessions
    sometimes start a further project under a name of their own (for
    example "inv-w1-setup-wizard") to run a check. Nothing removes them when
    the wave is done.

    THIS SCRIPT DELETES VOLUMES. Two guards keep it away from live work:

      * A real run refuses unless the wave is finished: with -Wave, its wave
        issue must be CLOSED (checked with gh; if gh cannot tell, it refuses).
        That issue vouches for the packages of THAT wave only, so a -Slug
        given alongside it that belongs to another wave refuses as well.
        With -Slug alone the script cannot tell, so it refuses without -Force.
        -DryRun never removes anything and only reports.
      * Ownership is decided by Docker's own labels, and a slug is never
        allowed to claim a longer sibling (see below).

    Ownership:

      * A compose PROJECT belongs to the wave if any of its containers has
        the label com.docker.compose.project.working_dir pointing into one of
        the wave's package worktrees. That catches a project whatever it is
        called.
      * A project also belongs to the wave if its name is the one the
        orchestrator assigned, <repo>-<slug>, or that followed by a hyphen
        and more (case-insensitive, "_" and "-" treated alike). That catches
        the networks, volumes and images of a project whose containers are
        gone. It applies only if the LONGEST <repo>-<slug> that the project's
        name matches, over every slug in the wave file, is one of the
        requested slugs - so w5-barcode never claims
        <repo>-w5-barcode-hot-cache.
      * Either way, a project with a container in a directory OUTSIDE the
        requested worktrees is not claimed: one container inside a worktree
        is not enough when another of the same project lives elsewhere.
        (A machine has ONE E2E project, inventory-e2e, whichever checkout
        brought it up - docker-compose.e2e.yml's `name:` comment, #190 - so
        without this a live E2E stack could be swept out from under the
        checkout running it.) A
        container carrying the project label but no working_dir label at all
        vetoes in the same way - it cannot be placed, so it is no evidence of
        ownership - and is reported as unplaceable rather than as "outside".
        A container in the repository root is handled by the main-checkout
        rule below instead.
      * Containers, networks and volumes carrying
        com.docker.compose.project=<such a project> are removed. Of its
        IMAGES only the tags that start with the project's own name are
        (compose names a built image <project>-<service>, see Test-OwnTag):
        an image that carries the project label but is tagged something else
        keeps that tag.

    What is never touched:

      * The main checkout's own stack: any project that has a container in
        the repository root, or is named after the repository itself.
      * Images that carry no such project label or tag - the shared
        <repo>-app-dev image (the dev override gives it one fixed name, so
        every worktree AND the main checkout use it), postgres, traefik,
        tailscale, the Playwright image, and the build cache.
      * Resources whose name merely mentions a slug, without being a compose
        project this wave can claim, are listed as "kept", not removed.

    The name rule is a NAME rule, and a compose project name says nothing
    about which checkout produced it. A CONTAINER-LESS project called
    <repo>-<slug> that another clone of this same repository left behind is
    therefore claimed and removed: nothing distinguishes it from the one this
    wave's own worktree left, since the veto above needs a container to read
    a directory from and this project has none. Run with -RepoRoot pointing
    at the clone you mean, and look with -DryRun first.

    Images are removed by tag, never by id: two projects that built the same
    content share an image id, and removing by id would take the other
    project's tag with it.

    If Docker cannot be listed or inspected the script stops BEFORE removing
    anything: a FAILED listing is never read as "nothing to do", because its
    empty result is indistinguishable from a real one. A listing that
    succeeds and is empty does mean there is nothing to do, and is treated
    as such.

.PARAMETER Wave
    Wave number in the wave file; its packages' slugs are used, and its wave
    issue must be closed for a real run.

.PARAMETER Slug
    Package slugs (comma-separated) to clean up, instead of or in addition to
    -Wave. A real run with -Slug and no -Wave needs -Force, and so does a
    slug that belongs to no package of the -Wave it is given with.

.PARAMETER WaveFile
    The wave plan. Default: wellen.json next to this script. It is always
    read when it exists, to know every slug of the plan. With -Wave it must
    exist; with -Slug alone a missing file only warns, since the longest-slug
    protection then has nothing to compare against.

.PARAMETER RepoRoot
    The main checkout. Default: the parent of this script's folder. Worktrees
    are <parent of RepoRoot>\<RepoName>-<slug>.

.PARAMETER GhRepo
    owner/name for the wave-issue check. Default: gh derives it from the
    repository at RepoRoot.

.PARAMETER Force
    Skip the "wave is finished" check entirely - the wave-issue check that
    -Wave would otherwise do included, and the cross-wave check on a -Slug
    given alongside it. Only for worktrees you know are finished.

.PARAMETER DryRun
    Only list what would be removed, and say whether a real run would be
    allowed.

.EXAMPLE
    .\wellen-docker-cleanup.ps1 -Wave 3 -DryRun
    .\wellen-docker-cleanup.ps1 -Wave 3
#>

[CmdletBinding()]
param(
    [int]$Wave = 0,
    [string[]]$Slug = @(),
    [string]$WaveFile = '',
    [string]$RepoRoot = '',
    [string]$GhRepo = '',
    [switch]$Force,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

$script:NativeExit = 0

# Same reason as in wellen-orchestrator.ps1: under $ErrorActionPreference =
# 'Stop', Windows PowerShell 5.1 turns any stderr output of a native command
# into a terminating error, so the preference is lowered for this one call.
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

# A listing that must succeed: an empty result from a failed call would read as
# "nothing to remove", so a failure stops the script before anything is removed.
function Get-DockerLines {
    param([scriptblock]$List, [string]$What)
    $lines = Invoke-Native $List
    if ($script:NativeExit -ne 0) {
        throw "Could not list $What (docker exit code $script:NativeExit): nothing was removed."
    }
    return @($lines | Where-Object { $_ })
}

# Lower case, "_" as "-": compose project names appear in both spellings
# (inventory-w1-x, inventory_w1_x) and are compared case-insensitively.
function ConvertTo-NameKey {
    param([string]$Name)
    if ([string]::IsNullOrEmpty($Name)) { return '' }
    return $Name.ToLowerInvariant().Replace('_', '-')
}

# Comparable form of a directory: full path, forward slashes, no trailing
# slash, lower case (Windows paths are case-insensitive).
function ConvertTo-PathKey {
    param([string]$Path)
    if ([string]::IsNullOrEmpty($Path)) { return '' }
    try { $full = [System.IO.Path]::GetFullPath($Path) } catch { $full = $Path }
    return $full.TrimEnd('\', '/').Replace('\', '/').ToLowerInvariant()
}

function Get-Label {
    param($Labels, [string]$Key)
    if ($null -eq $Labels) { return $null }
    $property = $Labels.PSObject.Properties[$Key]
    if ($null -eq $property) { return $null }
    return $property.Value
}

# `docker <InspectArgs> <ids...>` in chunks (Windows caps a command line),
# parsed. The parameter must not be called `$Command`: Invoke-Native has one, and
# a scriptblock sees the innermost variable of that name.
#
# A chunk in which some id vanished between the listing and the inspect (a
# container of another stack that just exited) exits non-zero but still prints
# the others: that output is used. A chunk that prints nothing while failing
# means Docker is not answering, and stops the script.
function Get-Inspected {
    param([string[]]$InspectArgs, [string[]]$Ids)
    $ids = @($Ids | Where-Object { $_ })
    $result = @()
    for ($i = 0; $i -lt $ids.Count; $i += 40) {
        $chunk = @($ids[$i..([Math]::Min($i + 39, $ids.Count - 1))])
        $lines = Invoke-Native { & docker @InspectArgs @chunk }
        if (-not $lines) {
            if ($script:NativeExit -ne 0) {
                throw "docker $($InspectArgs -join ' ') failed (exit code $script:NativeExit): nothing was removed."
            }
            continue
        }
        $parsed = (@($lines) -join "`n") | ConvertFrom-Json
        $result += @($parsed)
    }
    return $result
}

# 'CLOSED', 'OPEN', or $null when gh cannot tell.
function Get-IssueState {
    param([int]$Number)
    $ghArgs = @('issue', 'view', "$Number", '--json', 'state', '-q', '.state')
    if ($GhRepo) { $ghArgs += @('--repo', $GhRepo) }
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { return $null }
    Push-Location -LiteralPath $RepoRoot
    try { $state = Invoke-Native { & gh @ghArgs } } finally { Pop-Location }
    if ($script:NativeExit -ne 0 -or -not $state) { return $null }
    return ([string]@($state)[0]).Trim().ToUpperInvariant()
}

# ---------------------------------------------------------------------------
# Inputs
# ---------------------------------------------------------------------------

if ([string]::IsNullOrWhiteSpace($RepoRoot)) { $RepoRoot = Split-Path -Path $PSScriptRoot -Parent }
$RepoRoot  = [System.IO.Path]::GetFullPath($RepoRoot)
$RepoName  = Split-Path -Path $RepoRoot -Leaf
$ParentDir = Split-Path -Path $RepoRoot -Parent

# `powershell -File ... -Slug a,b` delivers "a,b" as ONE string: split it.
$slugs = @($Slug | ForEach-Object { $_ -split '[,\s]+' } | Where-Object { $_ })

# The wave file names every slug of the plan; it is needed to keep a slug from
# claiming a longer sibling, so it is read whenever it exists.
if ([string]::IsNullOrWhiteSpace($WaveFile)) { $WaveFile = Join-Path $PSScriptRoot 'wellen.json' }
$plan = $null
if (Test-Path -LiteralPath $WaveFile) {
    $plan = Get-Content -Raw -LiteralPath $WaveFile | ConvertFrom-Json
} elseif ($Wave -gt 0) {
    throw "Wave file not found: $WaveFile"
} else {
    # Not fatal - -Slug on a checkout without a plan file is legitimate - but it
    # silently costs the longest-slug protection, so it is said out loud (#140
    # item 4). "WARNING" so the orchestrator logs it at WARN; Write-Warning
    # goes to a stream it does not capture, and Write-Step is not defined yet.
    Write-Output "WARNING: wave file not found ($WaveFile) - only the slugs given with -Slug are known. A longer sibling slug of the same plan (say 'w5-barcode-hot-cache' next to 'w5-barcode') cannot be recognized, so a container-less project named after it would be claimed by the shorter slug. Pass -WaveFile if this plan has one, and look with -DryRun."
}
$waveIssue = 0
# Slugs that came from -Slug rather than from the wave: the finished-wave guard
# below can only ever vouch for the slugs of the wave whose issue it reads.
$givenSlugs = @($slugs)
$foreignSlugs = @()
if ($Wave -gt 0) {
    $match = @($plan.waves | Where-Object { $_.number -eq $Wave })
    if ($match.Count -eq 0) { throw "Wave $Wave is not in $WaveFile." }
    $waveSlugs = @{}
    foreach ($p in @($match[0].packages)) { if ($p.slug) { $waveSlugs[[string]$p.slug] = $true } }
    # -Wave 3 -Slug w5-barcode would otherwise let wave 3's closed issue
    # authorize the removal of wave 5's live stack: the guard below reads wave
    # 3's issue and nothing else (#140 item 1). Such a slug needs -Force.
    $foreignSlugs = @($givenSlugs | Where-Object { -not $waveSlugs.ContainsKey($_) })
    $slugs += @($match[0].packages | ForEach-Object { $_.slug })
    $waveIssue = [int]$match[0].waveIssue
}
$slugs = @($slugs | Where-Object { $_ } | Select-Object -Unique)
if ($slugs.Count -eq 0) { throw "Nothing to clean up: give -Wave <n> or -Slug <slug,...>." }
foreach ($s in $slugs) {
    if ($s -cnotmatch '^[a-z0-9][a-z0-9-]*$') { throw "Invalid slug '$s' (lowercase letters, digits and hyphens only)." }
}

$allSlugs = @($slugs)
if ($null -ne $plan) {
    foreach ($w in @($plan.waves)) {
        foreach ($p in @($w.packages)) { if ($p.slug) { $allSlugs += [string]$p.slug } }
    }
}
$allSlugs = @($allSlugs | Select-Object -Unique)

$mode = if ($DryRun) { '[DryRun] ' } else { '' }
# Write-Output, not Write-Host: the orchestrator captures these lines into its log.
function Write-Step { param([string]$Text) Write-Output "$mode$Text" }

$repoKey   = ConvertTo-NameKey $RepoName
$rootKey   = ConvertTo-PathKey $RepoRoot
$worktrees = @{}    # path key -> slug (requested)
$bases     = @{}    # name key <repo>-<slug> -> slug, for EVERY slug of the plan
$requested = @{}    # name key <repo>-<slug> -> slug, requested only
foreach ($s in $allSlugs) { $bases[(ConvertTo-NameKey "$RepoName-$s")] = $s }
foreach ($s in $slugs) {
    $worktrees[(ConvertTo-PathKey (Join-Path $ParentDir "$RepoName-$s"))] = $s
    $requested[(ConvertTo-NameKey "$RepoName-$s")] = $s
}

Write-Step "Wave cleanup for '$RepoName': $($slugs -join ', ')"

# ---------------------------------------------------------------------------
# 0. Is the wave over? (a real run needs a yes)
# ---------------------------------------------------------------------------

$allowed = $false
if ($Force) {
    $allowed = $true
    Write-Step "-Force: not checking that the wave is finished."
} elseif ($Wave -gt 0) {
    $state = Get-IssueState -Number $waveIssue
    if ($state -eq 'CLOSED' -and $foreignSlugs.Count -gt 0) {
        # Wave $Wave being over says nothing about these: their own packages may
        # still be running (#140 item 1).
        Write-Step "Wave $Wave is finished, but -Slug names $($foreignSlugs -join ', '), which belong to no package of wave $Wave - its issue says nothing about them. Clean them with their own -Wave, or pass -Force if you know they are finished."
    } elseif ($state -eq 'CLOSED') {
        $allowed = $true
        Write-Step "Wave $Wave is finished (wave issue #$waveIssue is closed)."
    } elseif ($state) {
        Write-Step "Wave $Wave is NOT finished: wave issue #$waveIssue is $state. Its package sessions may still be using this Docker state."
    } else {
        Write-Step "Cannot tell whether wave $Wave is finished: gh could not read wave issue #$waveIssue."
    }
} else {
    Write-Step "Without -Wave the script cannot tell whether these packages are finished."
}
if (-not $allowed -and -not $DryRun) {
    throw "Refusing to remove anything: the wave is not known to be finished. Look with -DryRun first; use -Force only for a wave you know is over."
}
if (-not $allowed) { Write-Step "A real run would refuse (see above)." }

# ---------------------------------------------------------------------------
# 1. Which compose projects belong to these worktrees?
# ---------------------------------------------------------------------------

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw "docker not found on PATH." }
Invoke-Native { docker info } | Out-Null
if ($script:NativeExit -ne 0) { throw "The Docker daemon is not reachable ('docker info' failed): nothing was removed." }

$projectKey = 'com.docker.compose.project'
$workdirKey = 'com.docker.compose.project.working_dir'

$containerIds = Get-DockerLines { docker ps -a -q --no-trunc } 'containers'
$containers = Get-Inspected -InspectArgs @('container', 'inspect') -Ids $containerIds

function Get-LabelledProject {
    param($Labels)
    return (Get-Label $Labels $projectKey)
}

# project name (as labelled) -> reason
$owned = @{}
# project name -> true: never removed (the main checkout's stack)
$protected = @{}
# project name -> true: has a container in a real directory OUTSIDE the
# requested worktrees, so a similar name is not enough to claim it
$foreignDir = @{}
# project name -> true: has a container carrying the project label but NO
# working_dir label, so it cannot be placed at all. Vetoes exactly like
# $foreignDir - an unplaceable container is not evidence of ownership - but it
# is reported differently: "outside the worktrees" would name a directory that
# does not exist and send a reader looking in the wrong place.
$unknownDir = @{}

foreach ($c in $containers) {
    $project = Get-LabelledProject $c.Config.Labels
    if (-not $project) { continue }
    $workdir = ConvertTo-PathKey (Get-Label $c.Config.Labels $workdirKey)
    if ($workdir -eq $rootKey) { $protected[$project] = $true; continue }
    $inside = $false
    foreach ($wt in $worktrees.Keys) {
        if ($workdir -eq $wt -or $workdir.StartsWith("$wt/")) {
            $owned[$project] = "container in worktree $($worktrees[$wt])"
            $inside = $true
        }
    }
    if (-not $inside) {
        if ($workdir) { $foreignDir[$project] = $true } else { $unknownDir[$project] = $true }
    }
}

# A project with a container in a directory outside the requested worktrees is
# not this wave's to remove, even when another of its containers does live in
# one (#140 item 2). A machine has exactly ONE E2E project, inventory-e2e, no
# matter which checkout brought it up (#190), so without this a wave that ran
# the E2E suite would take a live E2E stack of the main checkout - or of a
# worktree outside this wave - with it, the exact thing the name rule's own
# $foreignDir veto already refuses to do.
#
# Applied AFTER the whole container pass, not inside it: the container that
# claims the project and the one that vetoes it are different containers, and
# either may be seen first.
#
# The repository root is deliberately not such a directory: a container there
# takes the `continue` above, so it never reaches $foreignDir. Such a project
# stays claimable here and is removed from $owned by the main-checkout veto
# further down, which also records it as protected.
$vetoed = @{}   # project name -> why it was left alone
foreach ($project in @($owned.Keys)) {
    # A project of the main checkout is left to the main-checkout veto further
    # down, which reports it in the right words. Without this, a main-checkout
    # project that also has a stray container somewhere else would be reported
    # as "outside this wave's worktrees" rather than as the main checkout's.
    if ($protected.ContainsKey($project) -or (ConvertTo-NameKey $project) -eq $repoKey) { continue }
    if ($foreignDir.ContainsKey($project)) {
        $owned.Remove($project)
        $vetoed[$project] = "has a container in a directory outside this wave's worktrees"
    } elseif ($unknownDir.ContainsKey($project)) {
        $owned.Remove($project)
        $vetoed[$project] = "has a container with no working_dir label, which cannot be placed"
    }
}

# Networks, volumes and images labelled with a project: their project names
# are candidates too - a project whose containers are gone still has them.
$networkIds  = Get-DockerLines { docker network ls -q --no-trunc --filter "label=$projectKey" } 'networks'
$networks    = Get-Inspected -InspectArgs @('network', 'inspect') -Ids $networkIds
$volumeNames = Get-DockerLines { docker volume ls -q --filter "label=$projectKey" } 'volumes'
$volumes     = Get-Inspected -InspectArgs @('volume', 'inspect') -Ids $volumeNames
$imageIds    = @(Get-DockerLines { docker image ls -q --no-trunc --filter "label=$projectKey" } 'images' | Select-Object -Unique)
$images      = Get-Inspected -InspectArgs @('image', 'inspect') -Ids $imageIds

$candidateProjects = @{}
foreach ($c in $containers) { $p = Get-LabelledProject $c.Config.Labels; if ($p) { $candidateProjects[$p] = $true } }
foreach ($n in $networks)   { $p = Get-LabelledProject $n.Labels;        if ($p) { $candidateProjects[$p] = $true } }
foreach ($v in $volumes)    { $p = Get-LabelledProject $v.Labels;        if ($p) { $candidateProjects[$p] = $true } }
foreach ($i in $images)     { $p = Get-LabelledProject $i.Config.Labels; if ($p) { $candidateProjects[$p] = $true } }

foreach ($project in @($candidateProjects.Keys)) {
    # $unknownDir vetoes the name rule exactly as $foreignDir always has: before
    # the two were told apart for reporting, an unlabelled container landed in
    # $foreignDir and skipped here. Splitting them must not widen what is claimed.
    if ($owned.ContainsKey($project) -or $foreignDir.ContainsKey($project) -or $unknownDir.ContainsKey($project)) { continue }
    $key = ConvertTo-NameKey $project
    # The most specific <repo>-<slug> this name matches, over every slug of the
    # plan: "inventory-w5-barcode-hot-cache" matches both w5-barcode and
    # w5-barcode-hot-cache, and belongs to the longer one.
    $best = $null
    foreach ($base in $bases.Keys) {
        if ($key -eq $base -or $key.StartsWith("$base-")) {
            if ($null -eq $best -or $base.Length -gt $best.Length) { $best = $base }
        }
    }
    if ($null -ne $best -and $requested.ContainsKey($best)) {
        $owned[$project] = "named after worktree $($requested[$best])"
    }
}

# The main checkout's stack is never ours: a project named after the repository
# itself, or with a container in the repository root.
foreach ($project in @($owned.Keys)) {
    if ((ConvertTo-NameKey $project) -eq $repoKey -or $protected.ContainsKey($project)) {
        $owned.Remove($project)
        $protected[$project] = $true
    }
}

# Before the early return below: a vetoed project is the one case where
# something WAS found and is deliberately not removed, so it must be said even
# when nothing else is left to do.
if ($vetoed.Count -gt 0) {
    Write-Step "Left alone, not this wave's to remove:"
    foreach ($project in ($vetoed.Keys | Sort-Object)) { Write-Step "  $project ($($vetoed[$project]))" }
}

if ($owned.Count -eq 0) {
    Write-Step "No Docker resources of these worktrees found - nothing to remove."
    return
}

Write-Step "Compose projects that belong to the wave:"
foreach ($project in ($owned.Keys | Sort-Object)) { Write-Step "  $project ($($owned[$project]))" }
if ($protected.Count -gt 0) {
    Write-Step "Left alone, belongs to the main checkout: $((@($protected.Keys) | Sort-Object) -join ', ')"
}

# ---------------------------------------------------------------------------
# 2. What to remove
# ---------------------------------------------------------------------------

# ContainsKey() throws on a null key, and a resource without the label has none.
function Test-Owned { param($Project) return ($Project -and $owned.ContainsKey([string]$Project)) }

$rmContainers = @($containers | Where-Object { Test-Owned (Get-LabelledProject $_.Config.Labels) })
$rmNetworks   = @($networks   | Where-Object { Test-Owned (Get-LabelledProject $_.Labels) })
$rmVolumes    = @($volumes    | Where-Object { Test-Owned (Get-LabelledProject $_.Labels) })

# Image TAGS: those of a project image whose repository starts with the
# project's own name (compose names an image <project>-<service>), and the
# image a removed container was created from when it follows that naming.
$imageTags = New-Object System.Collections.Generic.List[string]
function Test-OwnTag {
    param([string]$Tag, [string]$Project)
    $repo = ($Tag -replace ':[^:/]*$', '')
    return (ConvertTo-NameKey $repo).StartsWith("$(ConvertTo-NameKey $Project)-")
}
foreach ($i in $images) {
    $project = Get-LabelledProject $i.Config.Labels
    if (-not (Test-Owned $project)) { continue }
    foreach ($tag in @($i.RepoTags)) {
        if ($tag -and (Test-OwnTag -Tag $tag -Project $project) -and -not $imageTags.Contains($tag)) { $imageTags.Add($tag) }
    }
}
foreach ($c in $rmContainers) {
    $project = Get-LabelledProject $c.Config.Labels
    $ref = [string]$c.Config.Image
    if ($ref -and -not $ref.StartsWith('sha256:') -and (Test-OwnTag -Tag $ref -Project $project)) {
        if (-not $imageTags.Contains($ref) -and -not $imageTags.Contains("${ref}:latest")) { $imageTags.Add($ref) }
    }
}

# Resources that mention a slug but were not attributed: reported, kept.
$removedNames = @{}
foreach ($c in $rmContainers) { $removedNames[$c.Name.TrimStart('/')] = $true }
foreach ($n in $rmNetworks)   { $removedNames[$n.Name] = $true }
foreach ($v in $rmVolumes)    { $removedNames[$v.Name] = $true }
foreach ($t in $imageTags)    { $removedNames[$t] = $true }
$kept = New-Object System.Collections.Generic.List[string]
$allNames = @()
$allNames += @($containers | ForEach-Object { $_.Name.TrimStart('/') })
$allNames += Get-DockerLines { docker network ls --format '{{.Name}}' } 'networks'
$allNames += Get-DockerLines { docker volume ls -q } 'volumes'
$allNames += @(Get-DockerLines { docker image ls --format '{{.Repository}}:{{.Tag}}' } 'images' | Where-Object { $_ -notlike '<none>*' })
foreach ($name in ($allNames | Select-Object -Unique)) {
    if ($removedNames.ContainsKey($name)) { continue }
    $key = ConvertTo-NameKey $name
    foreach ($s in $slugs) {
        if ($key.Contains($s)) { $kept.Add($name); break }
    }
}

Write-Step ("To remove: {0} container(s), {1} network(s), {2} volume(s), {3} image tag(s)." -f $rmContainers.Count, $rmNetworks.Count, $rmVolumes.Count, $imageTags.Count)
foreach ($c in $rmContainers) { Write-Step "  container $($c.Name.TrimStart('/'))" }
foreach ($n in $rmNetworks)   { Write-Step "  network   $($n.Name)" }
foreach ($v in $rmVolumes)    { Write-Step "  volume    $($v.Name)" }
foreach ($t in $imageTags)    { Write-Step "  image     $t" }
if ($kept.Count -gt 0) {
    Write-Step "Kept - mentions a slug, but is not attributable to a worktree of this wave:"
    foreach ($name in ($kept | Sort-Object)) { Write-Step "  $name" }
}

if ($DryRun) { return }

# ---------------------------------------------------------------------------
# 3. Remove, in dependency order. A failure is reported and does not stop the
#    rest: an image still used by something else, or a volume still mounted,
#    stays and is named.
# ---------------------------------------------------------------------------

$failed = 0
function Remove-Each {
    param([string]$Kind, [string[]]$Names, [scriptblock]$Remove)
    foreach ($name in $Names) {
        Invoke-Native { & $Remove $name } | Out-Null
        if ($script:NativeExit -ne 0) {
            # Output, not Write-Warning: the orchestrator logs only this stream.
            Write-Output "WARNING: could not remove $Kind '$name' (still in use?) - left in place."
            $script:failed++
        }
    }
}

if ($rmContainers.Count -gt 0) {
    # -v also removes the containers' anonymous volumes
    Remove-Each -Kind 'container' -Names @($rmContainers | ForEach-Object { $_.Id }) -Remove { param($n) docker rm -f -v $n }
}
if ($rmNetworks.Count -gt 0) {
    Remove-Each -Kind 'network' -Names @($rmNetworks | ForEach-Object { $_.Name }) -Remove { param($n) docker network rm $n }
}
if ($rmVolumes.Count -gt 0) {
    Remove-Each -Kind 'volume' -Names @($rmVolumes | ForEach-Object { $_.Name }) -Remove { param($n) docker volume rm $n }
}
if ($imageTags.Count -gt 0) {
    # by tag, no -f: a tag the image shares with another project's image only
    # loses this name, and an image still used by a container stays.
    Remove-Each -Kind 'image' -Names @($imageTags) -Remove { param($n) docker rmi $n }
}

Write-Output ("Done: {0} container(s), {1} network(s), {2} volume(s), {3} image tag(s) processed; {4} could not be removed." -f $rmContainers.Count, $rmNetworks.Count, $rmVolumes.Count, $imageTags.Count, $failed)
