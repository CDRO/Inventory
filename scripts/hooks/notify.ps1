#Requires -Version 5.1
<#
.SYNOPSIS
    Notification/Stop hook: raises a Windows toast so a session that is
    waiting on a permission prompt, or has finished, is noticed within
    seconds instead of at the next glance at nine windows.

.DESCRIPTION
    Invoked by .claude/settings.json as:

        powershell -NoProfile -File scripts/hooks/notify.ps1 <event> [title]

    <event> is either a hook name Claude Code passes ("Notification" or
    "Stop"), shown verbatim so the toast says which one fired, or "stale"
    (H11: the orchestrator's own heartbeat, scripts/wellen-planen.md's "What
    the orchestrator watches"). [title] is the last-resort fallback for
    Notification/Stop; the toast body there prefers, in order: the CURRENT
    console window's title (since the orchestrator sets it per package to
    "Wave N - Spec X (#issue)" and each session runs in its OWN console, so
    nothing else here can say which of nine open windows fired), then the
    hook's own stdin JSON ("message"/"title"/"reason", whichever is present -
    a Notification event carries the actual prompt text there), then [title]
    itself. A "stale" call is different: it is made from the ORCHESTRATOR's
    own console about a DIFFERENT session entirely, so the current-console
    preference would report the orchestrator's own title, not the stale
    package's - [title] (the package's window title, passed explicitly) is
    used as the toast body directly instead.

    Must NEVER block or fail the session, and must return within about 2
    seconds even if the toast backend hangs: the actual toast call runs on a
    runspace (a background thread in this same process, via
    [PowerShell]::Create()/BeginInvoke) bounded by an AsyncWaitHandle.WaitOne
    timeout, and every path - missing BurntToast, a thrown exception, a call
    that never completes - falls through to `exit 0`. A missed toast is not a
    failed session.

    #330 measured an earlier Start-Job-based version of this hook as having
    only ~0.5 s of margin against that 2 s budget: Start-Job spawns a whole
    PowerShell child process, which cost most of a ~1.5 s quiet run, and
    under host load the hang case measured 2.31 s - over budget, so a busy
    machine could kill this script before the toast fires, which is the very
    moment a waiting session most needs it. Running the toast call on a
    runspace instead removes that process-spawn cost (no new OS process, just
    a thread in this one), which is why the wait below can keep the same
    0.8 s budget #326 chose and still land around 1.1 s total, not 1.5-2.3 s.

    Testable without a real Windows toast backend: scripts/tests/hooks.test.ps1
    dot-sources everything above this script's entry-point marker to call
    the functions directly, and separately runs this script end-to-end with
    $env:CLAUDE_NOTIFY_TEST_BACKEND set to "ok", "throw", or "hang" (a 30s
    sleep, proving the WaitOne timeout below actually bounds it), which
    makes Send-Toast substitute a fake backend instead of BurntToast/NotifyIcon.
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

function Get-ToastBodyForEvent {
    # A "stale" call (H11's heartbeat) is made from the ORCHESTRATOR's own
    # console about a DIFFERENT session entirely, so Get-ToastBody's "the
    # current console's own title wins" preference would report the
    # orchestrator's own title, not the stale package's - $Fallback (the
    # package's window title, passed explicitly as [title]) is used as the
    # toast body directly instead, bypassing Get-ToastBody altogether.
    # Every other event (Notification, Stop) keeps Get-ToastBody's own
    # preference, since those DO fire from inside the session they are about.
    param([string]$EventName, [string]$Fallback)
    if ($EventName -eq 'stale') { return $Fallback }
    return Get-ToastBody -Fallback $Fallback
}

function Send-Toast {
    # Isolated so the test can substitute a fake backend without a real
    # Windows toast subsystem (CI, a locked-down account, no BurntToast
    # installed). "ok" simulates success; "throw" simulates a backend that
    # fails; "hang" simulates one that never returns - all three must still
    # let the caller (the WaitOne timeout below) reach exit 0 on schedule.
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
$toastBody = Get-ToastBodyForEvent -EventName $EventName -Fallback (Get-StdinMessage -Raw $stdinRaw -Fallback $FallbackTitle)

try {
    # [PowerShell]::Create() + BeginInvoke runs Send-Toast on a new thread in
    # THIS process (a runspace), not Start-Job's new child powershell.exe -
    # that process spawn was measured (#330) to cost ~1.5s of a ~1.5-2.3s
    # total run, leaving as little as ~0.5s of margin against the 2s hook
    # timeout .claude/settings.json sets. A runspace pays none of that
    # process-startup cost, so keeping the same 0.8s wait budget #326 already
    # validated now leaves comfortable margin instead of a thin one.
    $ps = [PowerShell]::Create()
    [void]$ps.AddScript(${function:Send-Toast}.ToString()).AddArgument($toastTitle).AddArgument($toastBody)
    $asyncResult = $ps.BeginInvoke()
    if (-not $asyncResult.AsyncWaitHandle.WaitOne(800)) {
        $ps.Stop()
    }
    $ps.Dispose()
} catch {
    # A backend failure, a runspace that never starts, anything at all - the
    # hook still succeeds.
}
exit 0
