#Requires -Version 5.1
<#
.SYNOPSIS
    Test for the executable bit on scripts/dev and scripts/dev.d/* (#336).

.DESCRIPTION
    #336: scripts/dev and every command under scripts/dev.d/ were committed
    as mode 100644, so a bare `scripts/dev <cmd>` invocation (not through
    `sh`) fails with "Permission denied" on a Linux-hosted CI runner or the
    NAS runner - real Linux, unlike a Windows/Git-Bash checkout, honors git's
    stored executable bit at checkout time.

    Nothing else in this repository's test suite would catch that bit
    reverting: `docker compose run --rm app go test ./...` runs inside the
    dev image on a bind mount from this Windows host, where NTFS has no
    executable bit for the container's Linux filesystem view to reflect
    either way, and the image has no git installed to inspect the index
    directly (Dockerfile: "No git needed"). `scripts/dev check` only runs
    gofmt/go vet/staticcheck/revive/TestEnvExampleParity, none of which look
    at file modes. So, like scripts/tests/doctor.test.ps1, this test runs
    directly on the host, where `git` is real and the index is the actual
    source of truth for what a Linux checkout will do with these files.

    Run:  powershell -NoProfile -File scripts\tests\dev-executable.test.ps1
    Needs `git` on PATH. Instant - reads the index, touches nothing.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$script:failures = 0

function Assert {
    param([bool]$Condition, [string]$Message)
    if ($Condition) { Write-Host "  PASS  $Message" } else { Write-Host "  FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}

$repoRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent

Write-Host "scripts/dev and scripts/dev.d/* are committed as mode 100755 (#336)"
Push-Location $repoRoot
try {
    $lines = @(git ls-files -s -- scripts/dev scripts/dev.d | Where-Object { $_ -ne '' })
} finally {
    Pop-Location
}

Assert ($lines.Count -ge 2) "git ls-files -s found scripts/dev and at least one scripts/dev.d/* entry, got $($lines.Count)"

foreach ($line in $lines) {
    # `<mode> <blob sha> <stage>\t<path>`
    $mode = $line.Split(' ')[0]
    $path = ($line -split "`t", 2)[1]
    Assert ($mode -eq '100755') "$path is mode $mode in the index, want 100755 (git update-index --chmod=+x $path)"
}

if ($script:failures -gt 0) { Write-Host "`n$($script:failures) assertion(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host "`nAll assertions passed."
