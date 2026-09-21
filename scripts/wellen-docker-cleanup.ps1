#Requires -Version 5.1
<#
.SYNOPSIS
    Removes the Docker resources that a wave's package worktrees created:
    containers (with their anonymous volumes), networks, named volumes and the
    images built for them. Called by the wave orchestrator after a wave's
    consolidation has finished (for waves with "dockerCleanup": true in the
    wave file), and usable by hand at any time.

.DESCRIPTION
    Package sessions run `docker compose` in their own worktree
    (<parent>\<repo>-<slug>). That leaves, per worktree and per compose
    project: a database container, a default network, three named volumes
    (pgdata, uploads, imagecache) and one or two built images - and sessions
    sometimes start a further project under a name of their own (for
    example "inv-w1-setup-wizard") to run a check. Nothing removes them when
    the wave is done.

    Ownership is decided by Docker's own labels, not by guessing at names:

      * A compose PROJECT belongs to the wave if any of its containers has
        the label com.docker.compose.project.working_dir pointing into one of
        the wave's package worktrees. That catches a project whatever it is
        called.
      * A project also belongs to the wave if its name is the one the
        orchestrator assigned (<repo>-<slug>) or starts with it followed by a
        hyphen (case-insensitive, "_" and "-" treated alike). That catches
        the networks, volumes and images of a project whose containers are
        already gone.
      * Everything carrying com.docker.compose.project=<such a project> is
        removed: containers, networks, volumes and images.

    What is never touched:

      * The main checkout's own stack: any project that has a container in
        the repository root, or is named after the repository itself.
      * Images that carry no such project label or tag - the shared
        <repo>-app-dev image (the dev override gives it one fixed name, so
        every worktree AND the main checkout use it), postgres, traefik,
        tailscale, the Playwright image, and the build cache.
      * Anything of another repository. Resources whose name merely mentions
        a slug but cannot be attributed to a worktree are listed as "kept",
        not removed.

    Images are removed by tag, never by id: two projects that built the same
    content share an image id, and removing by id would take the other
    project's tag with it.

.PARAMETER Wave
    Wave number in the wave file; its packages' slugs are used.

.PARAMETER Slug
    Package slugs to clean up, instead of (or in addition to) -Wave. The
    orchestrator passes them this way.

.PARAMETER WaveFile
    Wave plan for -Wave. Default: wellen.json next to this script.

.PARAMETER RepoRoot
    The main checkout. Default: the parent of this script's folder. Worktrees
    are <parent of RepoRoot>\<RepoName>-<slug>.

.PARAMETER DryRun
    Only list what would be removed.

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
# a scriptblock sees the innermost variable of that name. Returns @() for no ids or on failure.
function Get-Inspected {
    param([string[]]$InspectArgs, [string[]]$Ids)
    $ids = @($Ids | Where-Object { $_ })
    $result = @()
    for ($i = 0; $i -lt $ids.Count; $i += 40) {
        $chunk = @($ids[$i..([Math]::Min($i + 39, $ids.Count - 1))])
        $lines = Invoke-Native { & docker @InspectArgs @chunk }
        if ($script:NativeExit -ne 0 -or -not $lines) { continue }
        $parsed = (@($lines) -join "`n") | ConvertFrom-Json
        $result += @($parsed)
    }
    return $result
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
if ($Wave -gt 0) {
    if ([string]::IsNullOrWhiteSpace($WaveFile)) { $WaveFile = Join-Path $PSScriptRoot 'wellen.json' }
    if (-not (Test-Path -LiteralPath $WaveFile)) { throw "Wave file not found: $WaveFile" }
    $data = Get-Content -Raw -LiteralPath $WaveFile | ConvertFrom-Json
    $match = @($data.waves | Where-Object { $_.number -eq $Wave })
    if ($match.Count -eq 0) { throw "Wave $Wave is not in $WaveFile." }
    $slugs += @($match[0].packages | ForEach-Object { $_.slug })
}
$slugs = @($slugs | Where-Object { $_ } | Select-Object -Unique)
if ($slugs.Count -eq 0) { throw "Nothing to clean up: give -Wave <n> or -Slug <slug,...>." }
foreach ($s in $slugs) {
    if ($s -cnotmatch '^[a-z0-9][a-z0-9-]*$') { throw "Invalid slug '$s' (lowercase letters, digits and hyphens only)." }
}

$mode = if ($DryRun) { '[DryRun] ' } else { '' }
# Write-Output, not Write-Host: the orchestrator captures these lines into its log.
function Write-Step { param([string]$Text) Write-Output "$mode$Text" }

$repoKey  = ConvertTo-NameKey $RepoName
$rootKey  = ConvertTo-PathKey $RepoRoot
$worktrees = @{}    # path key -> slug
$bases     = @{}    # name key of the orchestrator-assigned project -> slug
foreach ($s in $slugs) {
    $worktrees[(ConvertTo-PathKey (Join-Path $ParentDir "$RepoName-$s"))] = $s
    $bases[(ConvertTo-NameKey "$RepoName-$s")] = $s
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw "docker not found on PATH." }
Invoke-Native { docker info } | Out-Null
if ($script:NativeExit -ne 0) { throw "The Docker daemon is not reachable ('docker info' failed): nothing was removed." }

$projectKey = 'com.docker.compose.project'
$workdirKey = 'com.docker.compose.project.working_dir'

Write-Step "Wave cleanup for '$RepoName': $($slugs -join ', ')"

# ---------------------------------------------------------------------------
# 1. Which compose projects belong to these worktrees?
# ---------------------------------------------------------------------------

$containerIds = @(Invoke-Native { docker ps -a -q --no-trunc } | Where-Object { $_ })
$containers = Get-Inspected -InspectArgs @('container', 'inspect') -Ids $containerIds

# project name (as labelled) -> reason
$owned = @{}
# project name -> true: never removed (the main checkout's stack)
$protected = @{}

foreach ($c in $containers) {
    $project = Get-Label $c.Config.Labels $projectKey
    if (-not $project) { continue }
    $workdir = ConvertTo-PathKey (Get-Label $c.Config.Labels $workdirKey)
    if ($workdir -eq $rootKey) { $protected[$project] = $true; continue }
    foreach ($wt in $worktrees.Keys) {
        if ($workdir -eq $wt -or $workdir.StartsWith("$wt/")) {
            $owned[$project] = "container in worktree $($worktrees[$wt])"
        }
    }
}

# Networks, volumes and images labelled with a project: their project names
# are candidates too - a project whose containers are gone still has them.
function Get-LabelledProject {
    param($Labels)
    return (Get-Label $Labels $projectKey)
}

$networkIds = @(Invoke-Native { docker network ls -q --no-trunc --filter "label=$projectKey" } | Where-Object { $_ })
$networks   = Get-Inspected -InspectArgs @('network', 'inspect') -Ids $networkIds
$volumeNames = @(Invoke-Native { docker volume ls -q --filter "label=$projectKey" } | Where-Object { $_ })
$volumes    = Get-Inspected -InspectArgs @('volume', 'inspect') -Ids $volumeNames
$imageIds   = @(Invoke-Native { docker image ls -q --no-trunc --filter "label=$projectKey" } | Where-Object { $_ } | Select-Object -Unique)
$images     = Get-Inspected -InspectArgs @('image', 'inspect') -Ids $imageIds

$candidateProjects = @{}
foreach ($c in $containers) { $p = Get-LabelledProject $c.Config.Labels; if ($p) { $candidateProjects[$p] = $true } }
foreach ($n in $networks)   { $p = Get-LabelledProject $n.Labels;        if ($p) { $candidateProjects[$p] = $true } }
foreach ($v in $volumes)    { $p = Get-LabelledProject $v.Labels;        if ($p) { $candidateProjects[$p] = $true } }
foreach ($i in $images)     { $p = Get-LabelledProject $i.Config.Labels; if ($p) { $candidateProjects[$p] = $true } }

foreach ($project in @($candidateProjects.Keys)) {
    if ($owned.ContainsKey($project)) { continue }
    $key = ConvertTo-NameKey $project
    foreach ($base in $bases.Keys) {
        if ($key -eq $base -or $key.StartsWith("$base-")) {
            $owned[$project] = "named after worktree $($bases[$base])"
        }
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
$allNames += @(@(Invoke-Native { docker network ls --format '{{.Name}}' }) | Where-Object { $_ })
$allNames += @(@(Invoke-Native { docker volume ls -q }) | Where-Object { $_ })
$allNames += @(@(Invoke-Native { docker image ls --format '{{.Repository}}:{{.Tag}}' }) | Where-Object { $_ -and $_ -notlike '<none>*' })
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
            Write-Warning "Could not remove $Kind '$name' (still in use?) - left in place."
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
