#Requires -Version 5.1
<#
.SYNOPSIS
    Test for wellen-docker-cleanup.ps1, against real Docker, in a scratch
    environment that touches nothing of this repository.

.DESCRIPTION
    The script under test DELETES VOLUMES, so its attribution rules are tested
    with real containers, networks, volumes and images. Everything is created
    under a random repository name ("zt<8 hex>") in a temporary folder, and all
    of it is removed at the end, pass or fail. The real repository's stacks are
    never looked at: the script is pointed at the scratch "repository" with
    -RepoRoot.

    Scenario (slugs zta, ztb, ztc, ztc-long in wave 1, ztd in wave 2):
      * zta   worktree, project named after its folder
      * zta   a SECOND container, in a subfolder of that worktree, belonging
              to a project named after the repository itself - the one way
              the main-checkout veto can fire on an already-owned project
      * ztb   worktree, project started under a name of its own
              (found through the container's working_dir label)
      * ztb   a second project whose container is gone but whose network,
              volume and image remain (found through its name)
      * ztb   a project with one container here and one in a folder that is
              no worktree at all - the shape docker-compose.e2e.yml's pinned
              project name produces across worktrees
      * ztc, ztc-long  two live worktrees where one slug is a PREFIX of the
              other; cleaning zta, ztb, ztc must not touch ztc-long
      * ztc-long  a CONTAINER-LESS project named after the longer slug, which
              only the longest-slug rule can save (no container, so no
              directory to veto with)
      * ztd   a worktree of ANOTHER wave, for the cross-wave -Slug guard
      * main  the "main checkout" project (a container in the repo root)
      * other a foreign project whose name contains a slug

    Docker and gh are also faked through PATH shims, for the paths that a real
    daemon cannot be made to take: a listing or an inspect that fails (the
    fail-closed paths of Get-DockerLines / Get-Inspected) and a wave issue that
    reads CLOSED (the positive -Wave path, and the cross-wave -Slug guard,
    which only bites once the wave itself is finished).

    Run:  powershell -NoProfile -File scripts\tests\wellen-docker-cleanup.test.ps1
    Needs Docker and the busybox image (pulled on first use). Takes about two
    minutes.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$script:failures = 0

function Dk {
    # docker, output discarded; Windows PowerShell 5.1 turns any stderr output of
    # a native command into a terminating error under 'Stop'.
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & docker @args 2>&1 | Out-Null } finally { $ErrorActionPreference = $previous }
}
function DkOut {
    $previous = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { return @(& docker @args 2>$null) } finally { $ErrorActionPreference = $previous }
}
function Assert {
    param([bool]$Condition, [string]$Message)
    if ($Condition) { Write-Host "  PASS  $Message" } else { Write-Host "  FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}
# containers / networks / volumes / image ids of a compose project
function Get-ProjectCount {
    param([string]$Project)
    $l = "label=com.docker.compose.project=$Project"
    return [pscustomobject]@{
        C = @(DkOut ps -a -q --filter $l).Count
        N = @(DkOut network ls -q --filter $l).Count
        V = @(DkOut volume ls -q --filter $l).Count
        I = @(DkOut image ls -q --filter $l | Select-Object -Unique).Count
    }
}
function Test-Project {
    param([string]$Project, [int]$C, [int]$N, [int]$V, [int]$I, [string]$What)
    $x = Get-ProjectCount $Project
    Assert ($x.C -eq $C -and $x.N -eq $N -and $x.V -eq $V -and $x.I -eq $I) "$What (${Project}: containers=$($x.C) networks=$($x.N) volumes=$($x.V) images=$($x.I), expected $C/$N/$V/$I)"
}

# The script's output, split into the section a name appears under. Asserting
# that a name occurs ANYWHERE in the output (as this test did before, #140
# item 8) passes whether it sits under "Kept" or wrongly under "To remove" -
# which is exactly the difference between keeping and deleting it. A section is
# its header line followed by indented lines; the first unindented line ends it.
function Get-Section {
    param([string]$Text, [string]$StartsWith)
    $found = New-Object System.Collections.Generic.List[string]
    $inside = $false
    foreach ($line in ($Text -split "`r?`n")) {
        $bare = $line -replace '^\[DryRun\] ', ''
        if (-not $inside) {
            if ($bare.StartsWith($StartsWith)) { $inside = $true }
            continue
        }
        if ($bare -notmatch '^\s+\S') { break }
        $found.Add($bare.Trim())
    }
    return @($found)
}
# "container zt1234-app-1" -> "zt1234-app-1"; a Kept line is a bare name already.
function Get-SectionNames {
    param([string]$Text, [string]$StartsWith, [switch]$Kinded)
    $entries = Get-Section -Text $Text -StartsWith $StartsWith
    if (-not $Kinded) { return $entries }
    return @($entries | ForEach-Object { ($_ -split '\s+', 2)[1] })
}

# A directory prepended to PATH holding a fake `docker` or `gh`, so the script
# can be driven down paths a real daemon will not produce on demand.
function New-PathShim {
    param([string]$Name, [string]$Body)
    $dir = Join-Path $env:TEMP "wcshim-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Set-Content -Path (Join-Path $dir "$Name.cmd") -Encoding ASCII -Value $Body
    return $dir
}
# Save and restore, never a string subtraction on PATH: a shim that outlived
# its block would quietly change a LATER section's meaning - a leftover `gh`
# saying CLOSED under the "-Force: the fake wave issue cannot be closed"
# heading would pass either way, because -Force short-circuits the issue read.
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
$ghClosedBody = @"
@echo off
echo CLOSED
exit /b 0
"@

$cleanupScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'wellen-docker-cleanup.ps1'
$id = -join ((1..8) | ForEach-Object { '{0:x}' -f (Get-Random -Maximum 16) })
$repo = "zt$id"
$root = Join-Path $env:TEMP "wctest-$id"
$repoDir = Join-Path $root $repo
$shared = "zshared$id"
$projects = @(
    $repo, "$repo-zta", "zcust$id", "$repo-ztb-orphan", $shared,
    "$repo-ztc", "$repo-ztc-long", "$repo-ztc-long-orphan", "$repo-ztd",
    "other-$repo-zta"
)

function New-Stack {
    param([string]$Dir, [string]$Project = '')
    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Set-Content -Path (Join-Path $Dir 'Dockerfile') -Value "FROM busybox`nRUN echo $id > /marker" -Encoding ASCII
    Set-Content -Path (Join-Path $Dir 'compose.yaml') -Encoding ASCII -Value @"
services:
  app:
    build: .
    command: sleep 3600
    volumes:
      - data:/data
volumes:
  data:
"@
    Push-Location $Dir
    try {
        if ($Project) { Dk compose -p $Project up -d --build } else { Dk compose up -d --build }
    } finally { Pop-Location }
}

# A second container for an EXISTING project, from a different directory. It
# has to be a different service name: same project plus same service is the
# same container to Compose, which would move the first one rather than add a
# second. Unbuilt and volume-less, so it adds a container and nothing else -
# the project's own network is reused, and busybox carries no project label.
function New-ProbeStack {
    param([string]$Dir, [string]$Project, [string]$Service)
    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Set-Content -Path (Join-Path $Dir 'compose.yaml') -Encoding ASCII -Value @"
services:
  ${Service}:
    image: busybox
    command: sleep 3600
"@
    Push-Location $Dir
    try { Dk compose -p $Project up -d } finally { Pop-Location }
}

function Invoke-Cleanup {
    param([hashtable]$Arguments)
    $Arguments['RepoRoot'] = $repoDir
    if (-not $Arguments.ContainsKey('WaveFile')) { $Arguments['WaveFile'] = $waveFile }
    try {
        $out = @(& $cleanupScript @Arguments) -join "`n"
        return [pscustomobject]@{ Out = $out; Error = $null }
    } catch {
        return [pscustomobject]@{ Out = ''; Error = $_.Exception.Message }
    }
}

try {
    Write-Host "Scratch environment '$repo' in $root"
    New-Stack $repoDir                                   # main checkout: project $repo
    New-Stack (Join-Path $root "$repo-zta")              # project $repo-zta
    # A container INSIDE the zta worktree that belongs to the project named
    # after the repository: the working_dir rule claims it, and only then can
    # the main-checkout veto that runs after ownership actually fire.
    New-ProbeStack (Join-Path $root "$repo-zta\probe") $repo 'probe'
    New-Stack (Join-Path $root "$repo-ztb") "zcust$id"   # custom-named project
    New-Stack (Join-Path $root "$repo-ztb") "$repo-ztb-orphan"
    Dk rm -f "$repo-ztb-orphan-app-1"                    # container gone; network, volume, image stay
    # One project, two directories: a container in the ztb worktree and one in
    # a folder that is no worktree of this wave. This is what a project name
    # pinned in a compose FILE (docker-compose.e2e.yml) looks like once two
    # checkouts have run it.
    New-Stack (Join-Path $root "$repo-ztb") $shared
    New-ProbeStack (Join-Path $root 'elsewhere') $shared 'probe'
    New-Stack (Join-Path $root "$repo-ztc")
    New-Stack (Join-Path $root "$repo-ztc-long")
    # Named after the LONGER slug and container-less: the $foreignDir veto
    # cannot save it (no container, no directory), so the longest-slug rule is
    # the only thing standing between it and `-Slug ztc`.
    New-Stack (Join-Path $root "$repo-ztc-long") "$repo-ztc-long-orphan"
    Dk rm -f "$repo-ztc-long-orphan-app-1"
    New-Stack (Join-Path $root "$repo-ztd")              # a package of wave 2
    New-Stack (Join-Path $root 'other') "other-$repo-zta"

    $waveFile = Join-Path $root 'wave.json'
    Set-Content -Path $waveFile -Encoding ASCII -Value '{"plan":{"name":"t","planIssue":1},"standards":{},"waves":[{"number":1,"waveIssue":2147483000,"packages":[{"slug":"zta"},{"slug":"ztb"},{"slug":"ztc"},{"slug":"ztc-long"}]},{"number":2,"waveIssue":2147483001,"packages":[{"slug":"ztd"}]}]}'

    Write-Host "Before"
    foreach ($p in @("$repo-zta", "zcust$id", "$repo-ztc", "$repo-ztc-long", "$repo-ztd", "other-$repo-zta")) {
        Test-Project $p 1 1 1 1 'stack is up'
    }
    Test-Project $repo 2 1 1 1 'the main-checkout project has a container in the repo root AND one in the zta worktree'
    Test-Project $shared 2 1 1 1 'the shared-name project has a container in the ztb worktree AND one elsewhere'
    Test-Project "$repo-ztb-orphan" 0 1 1 1 'orphan project has network, volume and image but no container'
    Test-Project "$repo-ztc-long-orphan" 0 1 1 1 'ztc-long-orphan has network, volume and image but no container'

    Write-Host "Dry run"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; DryRun = $true }
    Assert ($null -eq $r.Error) 'dry run does not throw'
    $ownedList = Get-Section $r.Out 'Compose projects that belong to the wave:'
    Assert ($r.Out -match "zcust$id \(container in worktree ztb\)") 'custom-named project is found through the working_dir label'
    Assert ($r.Out -match "$repo-ztb-orphan \(named after worktree ztb\)") 'container-less project is found through its name'
    Assert ($r.Out -match "$repo-zta \(container in worktree zta\)") 'zta project is found'
    Assert ($r.Out -notmatch "$repo-ztc-long \(") 'ztc-long is NOT claimed by the slug ztc'
    Assert ($r.Out -match "Left alone, belongs to the main checkout: $repo") 'the main checkout project is reported as left alone'
    Assert ($r.Out -match 'A real run would refuse') 'a dry run with -Slug says a real run would refuse'
    Test-Project "$repo-zta" 1 1 1 1 'dry run removed nothing'

    # #140 item 8: under WHICH heading the foreign project is listed is the
    # whole point - matching it anywhere passed either way.
    $keptNames = Get-SectionNames $r.Out 'Kept - mentions a slug'
    $removeNames = Get-SectionNames $r.Out 'To remove:' -Kinded
    # Most assertions below are of the shape "this name is NOT in that list",
    # which is vacuously true if the parser returned an empty list at all - a
    # drifted header, a changed $mode prefix, a blank line inside a section.
    # Both lists are known to be non-empty here, so say so once.
    Assert ($removeNames.Count -gt 0 -and $keptNames.Count -gt 0) "both sections parsed non-empty ($($removeNames.Count) to remove, $($keptNames.Count) kept) - the ''is not in'' assertions below are real"
    Assert ($keptNames -contains "other-$repo-zta-app-1") 'the foreign project''s container is listed under "Kept"'
    Assert (-not ($removeNames | Where-Object { $_ -like "other-$repo-zta*" })) 'nothing of the foreign project is under "To remove"'

    # #140 item 2: one container in a worktree is not enough when another of
    # the same project lives outside every worktree.
    Assert (-not ($ownedList | Where-Object { $_ -like "$shared (*" })) 'a project with a container outside the worktrees is NOT claimed by its container inside one'
    Assert ($r.Out -match "Left alone, has a container outside this wave's worktrees: $shared") 'and it is reported as left alone, with the reason'
    Assert (-not ($removeNames | Where-Object { $_ -like "$shared*" })) 'nothing of the shared-name project is under "To remove"'

    # #140 item 6: the main-checkout veto that runs AFTER ownership - only a
    # project the working_dir rule already claimed can reach it.
    Assert (-not ($ownedList | Where-Object { $_ -like "$repo (*" })) 'the repository-named project is not left in the owned list by its worktree container'
    Assert (-not ($removeNames | Where-Object { $_ -like "$repo-app-1*" -or $_ -like "$repo-probe-1*" })) 'neither container of the repository-named project is under "To remove"'

    # #140 item 5: the longest-slug rule, on a project no directory can veto.
    Assert (-not ($removeNames | Where-Object { $_ -like "$repo-ztc-long-orphan*" })) 'the container-less project named after the LONGER slug is not under "To remove"'
    Assert ($keptNames -contains "$repo-ztc-long-orphan_data") 'and its volume is reported under "Kept"'

    Write-Host "Missing wave file (#140 item 4)"
    $missing = Join-Path $root 'no-such-wave.json'
    $r = Invoke-Cleanup @{ Slug = 'ztc'; Force = $true; DryRun = $true; WaveFile = $missing }
    Assert ($null -eq $r.Error) 'a missing wave file with -Slug does not throw'
    Assert ($r.Out -match 'WARNING: wave file not found') 'a missing wave file warns'
    # The warning is not cosmetic: without the file the longest-slug rule has
    # nothing to compare against, and ztc claims ztc-long-orphan.
    $removeNames = Get-SectionNames $r.Out 'To remove:' -Kinded
    Assert (@($removeNames | Where-Object { $_ -like "$repo-ztc-long-orphan*" }).Count -gt 0) 'and without the file the longer slug''s container-less project IS claimed - what the warning is about'
    Test-Project "$repo-ztc-long-orphan" 0 1 1 1 'the dry run removed nothing anyway'

    Write-Host "Guards"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc' }
    Assert ($r.Error -match 'Refusing') '-Slug without -Force refuses'
    $r = Invoke-Cleanup @{ Wave = 1 }
    Assert ($r.Error -match 'Refusing') '-Wave refuses when the wave issue cannot be read as closed (fails closed)'
    Test-Project "$repo-zta" 1 1 1 1 'a refused run removed nothing'
    $saved = $env:DOCKER_HOST; $env:DOCKER_HOST = 'tcp://127.0.0.1:1'
    try { $r = Invoke-Cleanup @{ Slug = 'zta'; Force = $true } }
    finally { if ($null -eq $saved) { Remove-Item Env:\DOCKER_HOST } else { $env:DOCKER_HOST = $saved } }
    Assert ($r.Error -match 'not reachable') 'an unreachable daemon stops the script before anything is removed'
    Test-Project "$repo-zta" 1 1 1 1 'unreachable daemon: nothing removed'

    # #140 item 7: DOCKER_HOST only trips the `docker info` gate. A daemon that
    # answers `info` and then fails a listing or an inspect is what
    # Get-DockerLines and Get-Inspected exist for, and no real daemon does that
    # on request - so `docker` itself is faked for these two.
    Write-Host "Docker answers, then fails (#140 item 7)"
    $listFails = New-PathShim -Name 'docker' -Body @"
@echo off
if "%1"=="info" exit /b 0
exit /b 1
"@
    $r = Invoke-WithShim $listFails { Invoke-Cleanup @{ Slug = 'zta'; Force = $true } }
    Assert ($r.Error -match 'Could not list containers') 'a failed listing stops the script (not read as "nothing to remove")'
    Assert ($r.Error -match 'nothing was removed') 'and says so'
    Test-Project "$repo-zta" 1 1 1 1 'failed listing: nothing removed'

    $inspectFails = New-PathShim -Name 'docker' -Body @"
@echo off
if "%1"=="info" exit /b 0
if "%1"=="ps" goto listing
exit /b 1
:listing
echo deadbeefdeadbeef
exit /b 0
"@
    $r = Invoke-WithShim $inspectFails { Invoke-Cleanup @{ Slug = 'zta'; Force = $true } }
    Assert ($r.Error -match 'docker container inspect failed') 'a failed inspect stops the script'
    Assert ($r.Error -match 'nothing was removed') 'and says so'
    Test-Project "$repo-zta" 1 1 1 1 'failed inspect: nothing removed'
    Test-PathRestored 'after the docker shims'

    # #140 items 1 and 9: everything below needs the wave issue to read CLOSED.
    # A fake issue number cannot be closed on GitHub, and the guard that item 1
    # is about only bites once the wave IS finished - so gh is faked.
    Write-Host "Wave issue reads CLOSED (#140 items 1 and 9)"
    Invoke-WithShim (New-PathShim -Name 'gh' -Body $ghClosedBody) {
        $r = Invoke-Cleanup @{ Wave = 1; DryRun = $true }
        Assert ($r.Out -match "Wave 1 is finished \(wave issue #2147483000 is closed\)") '-Wave with a closed wave issue is allowed'
        Assert ($r.Out -notmatch 'A real run would refuse') 'and a real run would not refuse'
        $removeNames = Get-SectionNames $r.Out 'To remove:' -Kinded
        Assert (@($removeNames | Where-Object { $_ -like "$repo-ztc-long*" }).Count -gt 0) 'and -Wave 1 does claim ztc-long, which -Slug ztc did not'

        # The guard: wave 1's closed issue says nothing about wave 2's ztd.
        $r = Invoke-Cleanup @{ Wave = 1; Slug = 'ztd'; DryRun = $true }
        Assert ($r.Out -match 'belong to no package of wave 1') '-Wave 1 -Slug ztd (wave 2''s package) is refused by the cross-wave guard'
        Assert ($r.Out -match 'A real run would refuse') 'and the dry run says a real run would refuse'
        $removeNames = Get-SectionNames $r.Out 'To remove:' -Kinded
        Assert (@($removeNames | Where-Object { $_ -like "$repo-ztd*" }).Count -gt 0) 'the guard is not cosmetic: ztd''s live stack is what would have been removed'

        $r = Invoke-Cleanup @{ Wave = 1; Slug = 'ztd'; Force = $true; DryRun = $true }
        Assert ($r.Out -match '-Force: not checking') '-Force is the documented way past the cross-wave guard'

        # The real thing: without -Force it must actually throw, not just warn.
        $r = Invoke-Cleanup @{ Wave = 1; Slug = 'ztd' }
        Assert ($r.Error -match 'Refusing') 'a REAL -Wave 1 -Slug ztd run throws'
        Test-Project "$repo-ztd" 1 1 1 1 'and wave 2''s stack is untouched'
    }
    Test-PathRestored 'after the gh shim'

    Write-Host "Real run (-Force: the fake wave issue cannot be closed)"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; Force = $true }
    Assert ($null -eq $r.Error) "real run does not throw ($($r.Error))"
    Assert ($r.Out -match '0 could not be removed') 'nothing failed to remove'
    foreach ($p in @("$repo-zta", "zcust$id", "$repo-ztb-orphan", "$repo-ztc")) { Test-Project $p 0 0 0 0 'worktree project fully removed' }
    Test-Project "$repo-ztc-long" 1 1 1 1 'ztc-long (slug ztc is its prefix) is untouched'
    Test-Project "$repo-ztc-long-orphan" 0 1 1 1 'the container-less project of the longer slug is untouched'
    Test-Project $repo 2 1 1 1 'the main checkout project is untouched, both containers'
    Test-Project $shared 2 1 1 1 'the project with a container outside the worktrees is untouched'
    Test-Project "$repo-ztd" 1 1 1 1 'wave 2''s stack is untouched'
    Test-Project "other-$repo-zta" 1 1 1 1 'the foreign project is untouched'

    Write-Host "Second run"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; Force = $true }
    Assert ($r.Out -match 'nothing to remove') 'a second run finds nothing to remove'
    Test-Project "$repo-ztc-long" 1 1 1 1 'ztc-long still untouched'

    # The positive -Wave path end to end: not "would be allowed", but a real
    # run that removes, driven only by the wave issue being closed.
    Write-Host "Real -Wave run with a closed wave issue (#140 item 9)"
    Invoke-WithShim (New-PathShim -Name 'gh' -Body $ghClosedBody) {
        $r = Invoke-Cleanup @{ Wave = 1 }
        Assert ($null -eq $r.Error) "a real -Wave run with a closed issue does not throw ($($r.Error))"
        Assert ($r.Out -match '0 could not be removed') 'and nothing failed to remove'
    }
    Test-PathRestored 'after the second gh shim'
    Test-Project "$repo-ztc-long" 0 0 0 0 'ztc-long, this time a requested slug, is removed'
    Test-Project "$repo-ztc-long-orphan" 0 0 0 0 'and so is the container-less project named after it'
    Test-Project $repo 2 1 1 1 'the main checkout project is still untouched'
    Test-Project $shared 2 1 1 1 'the project with a container outside the worktrees is still untouched'
    Test-Project "$repo-ztd" 1 1 1 1 'wave 2''s stack is still untouched'
    Test-Project "other-$repo-zta" 1 1 1 1 'the foreign project is still untouched'
}
finally {
    Write-Host "Cleaning up"
    foreach ($p in $projects) {
        foreach ($c in (DkOut ps -a -q --filter "label=com.docker.compose.project=$p")) { Dk rm -f -v $c }
        foreach ($n in (DkOut network ls -q --filter "label=com.docker.compose.project=$p")) { Dk network rm $n }
        foreach ($v in (DkOut volume ls -q --filter "label=com.docker.compose.project=$p")) { Dk volume rm $v }
        foreach ($t in (DkOut image ls --filter "reference=$p-*" --format '{{.Repository}}:{{.Tag}}')) { Dk rmi $t }
    }
    if (Test-Path $root) { Remove-Item -Recurse -Force $root -ErrorAction SilentlyContinue }
}

if ($script:failures -gt 0) { Write-Host "`n$($script:failures) assertion(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "`nAll assertions passed."
