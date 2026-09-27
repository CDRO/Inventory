#Requires -Version 5.1
<#
.SYNOPSIS
    Notification/Stop hook: raises a Windows toast so a session that is
    waiting on a permission prompt, or has finished, is noticed within
    seconds instead of at the next glance at nine windows.

.DESCRIPTION
    Invoked by .claude/settings.json as:

        powershell -NoProfile -File scripts/hooks/notify.ps1 <event> [title]

    <event> is the hook name Claude Code passes ("Notification" or "Stop"),
    shown verbatim so the toast says which one fired. [title] is the
    last-resort fallback; the toast body prefers, in order: the CURRENT
    console window's title (since the orchestrator sets it per package to
    "Wave N - Spec X (#issue)", scripts/wellen-planen.md - nothing else here
    can say which of nine open windows fired), then the hook's own stdin
    JSON ("message"/"title"/"reason", whichever is present - a Notification
    event carries the actual prompt text there), then [title] itself.

    Must NEVER block or fail the session, and must return within about 2
    seconds even if the toast backend hangs: the actual toast call runs as a
    background job bounded by Wait-Job -Timeout, and every path - missing
    BurntToast, a thrown exception, a job that never completes - falls
    through to `exit 0`. A missed toast is not a failed session.

    Testable without a real Windows toast backend: scripts/tests/hooks.test.ps1
    dot-sources everything above this script's entry-point marker to call
    the functions directly, and separately runs this script end-to-end with
    $env:CLAUDE_NOTIFY_TEST_BACKEND set to "ok" or "throw", which makes
    Send-Toast substitute a fake backend instead of BurntToast/NotifyIcon.
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)] [string]$EventName = 'Notification',
    [Parameter(Position = 1)] [string]$FallbackTitle = 'Claude Code'
)

function Get-StdinMessage {
    # Parses the hook's JSON payload (already read from stdin by the caller -
    # kept out of this function so a test can pass a literal string instead
    # of redirecting a real console). Stop hooks often send an empty or
    # trivial body, and this must never throw past its own boundary
    # regardless of what (if anything) was actually there.
    param([string]$Raw, [string]$Fallback)
    try {
        if ($Raw) {
            $json = $Raw | ConvertFrom-Json -ErrorAction Stop
            foreach ($field in 'message', 'title', 'reason') {
                if ($json.$field) { return [string]$json.$field }
            }
        }
    } catch {
        # Not JSON - fall through to the fallback.
    }
    return $Fallback
}

function Get-ToastBody {
    # The console window's own title beats the stdin message when present -
    # it is the one thing that says which of several open sessions fired.
    param([string]$Fallback)
    try {
        $title = $Host.UI.RawUI.WindowTitle
        if ($title) { return $title }
    } catch {}
    return $Fallback
}

function Send-Toast {
    # Isolated so the test can substitute a fake backend without a real
    # Windows toast subsystem (CI, a locked-down account, no BurntToast
    # installed). "ok" simulates success; "throw" simulates a backend that
    # fails; "hang" simulates one that never returns - all three must still
    # let the caller (the Wait-Job -Timeout below) reach exit 0 on schedule.
    param([string]$Title, [string]$Body)
    switch ($env:CLAUDE_NOTIFY_TEST_BACKEND) {
        'ok' { return }
        'throw' { throw 'simulated toast backend failure (test)' }
        'hang' { Start-Sleep -Seconds 30; return }
    }
    if (Get-Module -ListAvailable -Name BurntToast) {
        Import-Module BurntToast -ErrorAction Stop
        New-BurntToastNotification -Text $Title, $Body | Out-Null
        return
    }
    Add-Type -AssemblyName System.Windows.Forms
    $icon = New-Object System.Windows.Forms.NotifyIcon
    try {
        $icon.Icon = [System.Drawing.SystemIcons]::Information
        $icon.Visible = $true
        $icon.ShowBalloonTip(1500, $Title, $Body, [System.Windows.Forms.ToolTipIcon]::Info)
        Start-Sleep -Milliseconds 200
    } finally {
        $icon.Dispose()
    }
}

# === ENTRYPOINT =============================================================
# Everything above this point is a plain function definition; nothing here
# runs until this line, which is what lets the test dot-source the
# functions alone (scripts/tests/hooks.test.ps1, same split as
# wellen-orchestrator.test.ps1 uses on wellen-orchestrator.ps1's own marker)
# and separately exercise this tail end to end.
$stdinRaw = ''
try {
    if ([Console]::IsInputRedirected) { $stdinRaw = [Console]::In.ReadToEnd() }
} catch {
    # No stdin to read - stays empty, Get-StdinMessage falls through.
}

$toastTitle = "Claude Code - $EventName"
$toastBody = Get-ToastBody -Fallback (Get-StdinMessage -Raw $stdinRaw -Fallback $FallbackTitle)

try {
    $job = Start-Job -ScriptBlock ${function:Send-Toast} -ArgumentList $toastTitle, $toastBody
    # 0.8s, not the full ~2s budget: Start-Job itself already costs time (a
    # new background PowerShell process), so the wait has to leave headroom
    # for that overhead plus Remove-Job and PowerShell's own startup - a
    # hanging backend (scripts/tests/hooks.test.ps1's "hang" case) must not
    # push the total past Claude Code's ~2s hook timeout.
    Wait-Job -Job $job -Timeout 0.8 | Out-Null
    Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
} catch {
    # A backend failure, a job that never starts, anything at all - the hook
    # still succeeds.
}
exit 0
