#Requires -Version 5.1
<#
.SYNOPSIS
    Tests for scripts/hooks/notify.ps1, the Notification/Stop hook
    (issue #303 / H4 attention).

.DESCRIPTION
    Two layers, matching wellen-orchestrator.test.ps1's split of a
    top-to-bottom script into "functions" and "main": Get-StdinMessage and
    Get-ToastBody are dot-sourced from the portion of notify.ps1 above its
    "# Main" marker and called directly, against a stubbed toast backend -
    no real Windows toast subsystem is needed. The main tail (runspace
    dispatch, the try/catch, `exit 0`) is then exercised end to end as a real
    subprocess, with $env:CLAUDE_NOTIFY_TEST_BACKEND selecting Send-Toast's
    fake backend, to prove the acceptance criteria in #303: exit 0 and total
    runtime under 2 seconds whether the backend succeeds, throws, or - the
    case that actually exercises the WaitOne timeout rather than a backend
    that just happens to be fast - hangs for 30 seconds.

    Run:  powershell -NoProfile -File scripts\tests\hooks.test.ps1
    Needs no Docker, no network, no real toast backend. Takes a few seconds.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$script:failures = 0

function Assert {
    param([bool]$Condition, [string]$Message)
    if ($Condition) { Write-Host "  PASS  $Message" } else { Write-Host "  FAIL  $Message" -ForegroundColor Red; $script:failures++ }
}

# --- Load just the function definitions from the real script --------------

$hookScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'hooks\notify.ps1'
$sourceText = Get-Content -LiteralPath $hookScript -Raw
$marker = '# === ENTRYPOINT ==='
$splitAt = $sourceText.IndexOf($marker)
if ($splitAt -lt 0) {
    throw "Could not find the '$marker' marker in $hookScript - has it moved? This test's dot-source split depends on it staying above the stdin read, the job dispatch and exit 0."
}
$functionsOnly = $sourceText.Substring(0, $splitAt)
$sb = [ScriptBlock]::Create($functionsOnly)
. $sb

Write-Host "== Get-StdinMessage =="
Assert ((Get-StdinMessage -Raw '{"message":"needs permission for Bash(rm -rf /)"}' -Fallback 'fallback') -ceq 'needs permission for Bash(rm -rf /)') `
    'prefers the JSON "message" field when present'
Assert ((Get-StdinMessage -Raw '{"title":"only a title"}' -Fallback 'fallback') -ceq 'only a title') `
    'falls back to "title" when "message" is absent'
Assert ((Get-StdinMessage -Raw '{"reason":"only a reason"}' -Fallback 'fallback') -ceq 'only a reason') `
    'falls back to "reason" when neither "message" nor "title" is present'
Assert ((Get-StdinMessage -Raw '{"message":"m","title":"t","reason":"r"}' -Fallback 'fallback') -ceq 'm') `
    'with all three fields present, "message" wins over "title" and "reason"'
Assert ((Get-StdinMessage -Raw '{"title":"t","reason":"r"}' -Fallback 'fallback') -ceq 't') `
    'with "title" and "reason" both present (no "message"), "title" wins'
Assert ((Get-StdinMessage -Raw '' -Fallback 'fallback') -ceq 'fallback') `
    'empty stdin returns the fallback, no throw'
Assert ((Get-StdinMessage -Raw 'not json at all' -Fallback 'fallback') -ceq 'fallback') `
    'unparsable stdin returns the fallback, no throw'
Assert ((Get-StdinMessage -Raw '{}' -Fallback 'fallback') -ceq 'fallback') `
    'valid JSON with none of the three fields returns the fallback'

Write-Host "== Get-ToastBody =="
$previousTitle = $Host.UI.RawUI.WindowTitle
try {
    $Host.UI.RawUI.WindowTitle = 'Wave 1 - H4 attention (#303)'
    Assert ((Get-ToastBody -Fallback 'fallback') -ceq 'Wave 1 - H4 attention (#303)') `
        'prefers the live console window title over the fallback'
} finally {
    $Host.UI.RawUI.WindowTitle = $previousTitle
}

Write-Host "== Get-ToastBodyForEvent (H11): a 'stale' toast names the STALE package, not this console =="

# The orchestrator calls this from its OWN console about a DIFFERENT window
# (the package session it suspects is stuck) - the live console title set
# here stands in for "the orchestrator's own console title", which a naive
# call to Get-ToastBody would report instead of the package's title.
$previousTitle = $Host.UI.RawUI.WindowTitle
try {
    $Host.UI.RawUI.WindowTitle = 'Wave 3 - Orchestrator console (not the stale package)'
    Assert ((Get-ToastBodyForEvent -EventName 'stale' -Fallback 'Wave 3 - H11 orchestrator attention (#310)') -ceq 'Wave 3 - H11 orchestrator attention (#310)') `
        'a "stale" event uses the passed title, never the current (orchestrator) console title'
    Assert ((Get-ToastBodyForEvent -EventName 'Notification' -Fallback 'fallback') -ceq 'Wave 3 - Orchestrator console (not the stale package)') `
        'every other event still prefers the live console title, unchanged from before H11'
} finally {
    $Host.UI.RawUI.WindowTitle = $previousTitle
}

Write-Host "== Send-Toast (stubbed backend) =="
$env:CLAUDE_NOTIFY_TEST_BACKEND = 'ok'
try {
    Send-Toast -Title 't' -Body 'b'
    Assert $true 'the "ok" stub backend returns without throwing'
} catch {
    Assert $false "the \"ok\" stub backend must not throw: $_"
} finally {
    Remove-Item Env:\CLAUDE_NOTIFY_TEST_BACKEND -ErrorAction SilentlyContinue
}

$env:CLAUDE_NOTIFY_TEST_BACKEND = 'throw'
try {
    Send-Toast -Title 't' -Body 'b'
    Assert $false 'the "throw" stub backend was expected to throw, and did not'
} catch {
    Assert $true 'the "throw" stub backend actually throws (proves the real script''s try/catch is doing real work)'
} finally {
    Remove-Item Env:\CLAUDE_NOTIFY_TEST_BACKEND -ErrorAction SilentlyContinue
}

# The "hang" backend sleeps 30s - if Send-Toast is called directly (as
# above) this call would genuinely block for 30s, so it is only exercised
# through the real script end-to-end below, where the WaitOne timeout must
# cut it off. This directly guards against the timeout being deleted or
# bypassed: without it, the "hang" case in the section below would time out
# this whole test file instead of completing in under 2 seconds.

# --- Run the real script end to end, as a subprocess -----------------------
# This is what actually proves the acceptance criteria: the *whole* hook -
# stdin read, runspace dispatch, WaitOne timeout, exit 0 - not just the
# functions above.

function Invoke-Hook {
    param([string]$Backend, [string]$StdinJson = '{}', [string]$EventName = 'Notification')
    $env:CLAUDE_NOTIFY_TEST_BACKEND = $Backend
    try {
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $StdinJson | & powershell -NoProfile -File $hookScript $EventName 'test title' | Out-Null
        $exitCode = $LASTEXITCODE
        $sw.Stop()
        return @{ ExitCode = $exitCode; Seconds = $sw.Elapsed.TotalSeconds }
    } finally {
        Remove-Item Env:\CLAUDE_NOTIFY_TEST_BACKEND -ErrorAction SilentlyContinue
    }
}

Write-Host "== notify.ps1 end to end =="
$ok = Invoke-Hook -Backend 'ok'
Assert ($ok.ExitCode -eq 0) "exit 0 when the toast backend succeeds (got $($ok.ExitCode))"
Assert ($ok.Seconds -lt 2.0) "runs in under 2 seconds when the backend succeeds (took $([math]::Round($ok.Seconds, 2))s)"

$thrown = Invoke-Hook -Backend 'throw'
Assert ($thrown.ExitCode -eq 0) "exit 0 when the toast backend throws (got $($thrown.ExitCode))"
Assert ($thrown.Seconds -lt 2.0) "runs in under 2 seconds when the backend throws (took $([math]::Round($thrown.Seconds, 2))s)"

# This is the one that actually proves the WaitOne timeout does something:
# the "hang" backend sleeps 30s, so a script that waited on it
# unconditionally, or whose timeout had been deleted or widened, would blow
# well past 2 seconds here - this is not a fast backend happening to finish
# quickly.
$hung = Invoke-Hook -Backend 'hang'
Assert ($hung.ExitCode -eq 0) "exit 0 when the toast backend hangs for 30s (got $($hung.ExitCode))"
Assert ($hung.Seconds -lt 2.0) "runs in under 2 seconds even when the backend hangs for 30s (took $([math]::Round($hung.Seconds, 2))s) - proves the WaitOne timeout actually bounds it"

# H11's new "stale" event flows through the exact same entrypoint - proves it
# is not a special case that skips the exit-0/timeout guarantees above.
$stale = Invoke-Hook -Backend 'ok' -EventName 'stale'
Assert ($stale.ExitCode -eq 0) "exit 0 for the 'stale' event too (got $($stale.ExitCode))"
Assert ($stale.Seconds -lt 2.0) "runs in under 2 seconds for the 'stale' event too (took $([math]::Round($stale.Seconds, 2))s)"

if ($script:failures -gt 0) {
    Write-Host "$($script:failures) assertion(s) FAILED" -ForegroundColor Red
    exit 1
} else {
    Write-Host "All assertions passed"
    exit 0
}
