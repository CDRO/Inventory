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

    Scenario (slugs zta, ztb, ztc, ztc-long; the wave file lists all four):
      * zta   worktree, project named after its folder
      * ztb   worktree, project started under a name of its own
              (found through the container's working_dir label)
      * ztb   a second project whose container is gone but whose network,
              volume and image remain (found through its name)
      * ztc, ztc-long  two live worktrees where one slug is a PREFIX of the
              other; cleaning zta, ztb, ztc must not touch ztc-long
      * main  the "main checkout" project (a container in the repo root)
      * other a foreign project whose name contains a slug

    Run:  powershell -NoProfile -File scripts\tests\wellen-docker-cleanup.test.ps1
    Needs Docker and the busybox image (pulled on first use). Takes about a minute.
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

$cleanupScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'wellen-docker-cleanup.ps1'
$id = -join ((1..8) | ForEach-Object { '{0:x}' -f (Get-Random -Maximum 16) })
$repo = "zt$id"
$root = Join-Path $env:TEMP "wctest-$id"
$repoDir = Join-Path $root $repo
$projects = @($repo, "$repo-zta", "zcust$id", "$repo-ztb-orphan", "$repo-ztc", "$repo-ztc-long", "other-$repo-zta")

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

function Invoke-Cleanup {
    param([hashtable]$Arguments)
    $Arguments['RepoRoot'] = $repoDir
    $Arguments['WaveFile'] = $waveFile
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
    New-Stack (Join-Path $root "$repo-ztb") "zcust$id"   # custom-named project
    New-Stack (Join-Path $root "$repo-ztb") "$repo-ztb-orphan"
    Dk rm -f "$repo-ztb-orphan-app-1"                    # container gone; network, volume, image stay
    New-Stack (Join-Path $root "$repo-ztc")
    New-Stack (Join-Path $root "$repo-ztc-long")
    New-Stack (Join-Path $root 'other') "other-$repo-zta"

    $waveFile = Join-Path $root 'wave.json'
    Set-Content -Path $waveFile -Encoding ASCII -Value '{"plan":{"name":"t","planIssue":1},"standards":{},"waves":[{"number":1,"waveIssue":2147483000,"packages":[{"slug":"zta"},{"slug":"ztb"},{"slug":"ztc"},{"slug":"ztc-long"}]}]}'

    Write-Host "Before"
    foreach ($p in ($projects | Where-Object { $_ -ne "$repo-ztb-orphan" })) { Test-Project $p 1 1 1 1 'stack is up' }
    Test-Project "$repo-ztb-orphan" 0 1 1 1 'orphan project has network, volume and image but no container'

    Write-Host "Dry run"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; DryRun = $true }
    Assert ($null -eq $r.Error) 'dry run does not throw'
    Assert ($r.Out -match "zcust$id \(container in worktree ztb\)") 'custom-named project is found through the working_dir label'
    Assert ($r.Out -match "$repo-ztb-orphan \(named after worktree ztb\)") 'container-less project is found through its name'
    Assert ($r.Out -match "$repo-zta \(container in worktree zta\)") 'zta project is found'
    Assert ($r.Out -notmatch "$repo-ztc-long \(") 'ztc-long is NOT claimed by the slug ztc'
    Assert ($r.Out -match "Left alone, belongs to the main checkout: $repo") 'the main checkout project is reported as left alone'
    Assert ($r.Out -match "other-$repo-zta") 'the foreign project is listed as kept'
    Assert ($r.Out -match 'A real run would refuse') 'a dry run with -Slug says a real run would refuse'
    Test-Project "$repo-zta" 1 1 1 1 'dry run removed nothing'

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

    Write-Host "Real run (-Force: the fake wave issue cannot be closed)"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; Force = $true }
    Assert ($null -eq $r.Error) "real run does not throw ($($r.Error))"
    Assert ($r.Out -match '0 could not be removed') 'nothing failed to remove'
    foreach ($p in @("$repo-zta", "zcust$id", "$repo-ztb-orphan", "$repo-ztc")) { Test-Project $p 0 0 0 0 'worktree project fully removed' }
    Test-Project "$repo-ztc-long" 1 1 1 1 'ztc-long (slug ztc is its prefix) is untouched'
    Test-Project $repo 1 1 1 1 'the main checkout project is untouched'
    Test-Project "other-$repo-zta" 1 1 1 1 'the foreign project is untouched'

    Write-Host "Second run"
    $r = Invoke-Cleanup @{ Slug = 'zta,ztb,ztc'; Force = $true }
    Assert ($r.Out -match 'nothing to remove') 'a second run finds nothing to remove'
    Test-Project "$repo-ztc-long" 1 1 1 1 'ztc-long still untouched'
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
