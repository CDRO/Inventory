#!/bin/sh
# scripts/tests/agent-loop.test.sh - stub-driven test for scripts/agent-loop.sh
# (H19, #318). Mirrors scripts/tests/doctor.test.ps1's own convention: a
# standalone script an operator runs by hand, not wired into
# `docker compose run --rm app go test ./...` (agent-loop.sh needs `jq`,
# which the Go dev image does not carry - see scripts/agent_loop_test.go for
# the thin smoke test that IS part of that suite, covering --help/--dry-run
# wiring without needing jq at all).
#
# Needs: jq, gh, git, sh, and GNU coreutils' date (real binaries; every call
# claude/gh/git/sleep make is faked below, so nothing here reaches a real
# repository or a real GitHub - date is the one real binary agent-loop.sh's
# own backoff_and_wait actually calls, to parse an ISO-8601 reset time out of
# a fixture's result text; BusyBox date (plain `alpine`, most embedded
# Linux) cannot parse `-d '<ISO-8601 string>'` at all and fails every
# usage-limit assertion that depends on it - this is a real gap the suite
# will now report honestly (round 1 of this PR made it report falsely
# instead: an unset variable made the assertion's needle empty, which always
# "matches"). Run inside deploy/agent's container, where GNU date already is
# (its image is debian:bookworm-slim):
#
#   export REPO_PATH=$(pwd)
#   docker compose -f deploy/agent/docker-compose.agent.yml run --rm agent \
#     sh scripts/tests/agent-loop.test.sh
#
# or on any Linux host with jq and GNU coreutils installed (i.e. not a
# BusyBox/musl minimal image):
#
#   sh scripts/tests/agent-loop.test.sh
#
# Every scenario copies scripts/agent-loop.sh and
# scripts/package-prompt.template into a throwaway directory (the same
# reason scripts/dev_test.go copies scripts/dev - agent-loop.sh derives its
# own repository root from its own path, and a real git worktree next to the
# real checkout is not something a test should create), puts a fake claude,
# gh, git and sleep ahead of the real ones on PATH, and points the script's
# own test seams (AGENT_LOOP_LOCK_DIR, AGENT_LOOP_DOCTOR, AGENT_LOOP_LOG_DIR,
# AGENT_LOOP_UID) at scratch state. `sleep` is faked for the same reason a
# fixture below names a reset time about a day out: this test asserts what
# the loop DID with that reset time, never actually waits for it.
set -u

failures=0
passes=0

assert() {
  if [ "$1" = "0" ]; then
    passes=$((passes + 1))
    printf '  PASS  %s\n' "$2"
  else
    failures=$((failures + 1))
    printf '  FAIL  %s\n' "$2"
  fi
}

assert_contains() {
  # $1 haystack, $2 needle, $3 message
  case "$1" in
    *"$2"*) assert 0 "$3" ;;
    *) assert 1 "$3 (did not find: $2)" ;;
  esac
}

assert_not_contains() {
  case "$1" in
    *"$2"*) assert 1 "$3 (unexpectedly found: $2)" ;;
    *) assert 0 "$3" ;;
  esac
}

claude_call_count() {
  # grep -c always prints a count, including 0 - but exits 1 when the count
  # IS 0 (no line matched), so a "|| fallback" after it would print a SECOND
  # "0" on top of the first. Every scenario pre-creates the calls log (even
  # empty), so there is no missing-file case here to fall back for.
  if [ -f "$SCPATH/state/claude-calls.log" ]; then
    grep -c '^===CALL===$' "$SCPATH/state/claude-calls.log"
  else
    printf '0'
  fi
}

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
TESTBASE=$(mktemp -d)
cleanup() { rm -rf "$TESTBASE"; }
trap cleanup EXIT INT TERM

# --- Fixtures: stream-json result lines, one scenario each -------------------
FIXDIR="$TESTBASE/fixtures"
mkdir -p "$FIXDIR"

cat > "$FIXDIR/success.jsonl" <<'EOF'
{"type":"system","subtype":"init","session_id":"sess-success-1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":12,"session_id":"sess-success-1","result":"Shipped in PR #999."}
EOF

cat > "$FIXDIR/max_turns.jsonl" <<'EOF'
{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":400,"session_id":"sess-maxturns-1","result":"Reached the maximum number of turns."}
EOF

# D10 could not verify the exact usage-limit subtype or wording - this
# fixture encodes BOTH candidate signals the issue names: a mid-stream
# system/api_retry event with error "rate_limit", and result text that reads
# like a usage-limit message. The reset time is computed a day out from
# whenever the suite actually runs (never a hardcoded date) - agent-loop.sh's
# own backoff_and_wait only trusts a reset time that is still in the future
# (scripts/agent-loop.sh:436), so a fixed past-tense date goes stale and
# silently falls through to the default backoff instead of failing loudly.
#
# `date -d '+1 day'` is a GNU-only relative-date extension: on BusyBox date
# (e.g. plain `alpine`, which the header above says is a valid place to run
# this) it fails silently, leaving this variable empty - and assert_contains
# with an empty needle always matches (see its `case "$1" in *"$2"*)`  above),
# so the round-1 fix for this same fixture's staleness regressed into an
# assertion that could never fail while looking like it still tested
# anything. `-d @<epoch>` is POSIX-portable across both date flavors, so the
# offset is computed as arithmetic on `date +%s` instead of parsed as text.
now_epoch=$(date -u +%s) || { echo "FATAL: date +%s failed" >&2; exit 1; }
RATE_LIMIT_RESET_ISO=$(date -u -d "@$((now_epoch + 86400))" +%Y-%m-%dT%H:%M:%SZ)
[ -n "$RATE_LIMIT_RESET_ISO" ] || { echo "FATAL: could not compute RATE_LIMIT_RESET_ISO - date(1) does not support -d @<epoch> here" >&2; exit 1; }
cat > "$FIXDIR/rate_limit.jsonl" <<EOF
{"type":"system","subtype":"api_retry","attempt":1,"max_retries":3,"retry_delay_ms":2000,"error_status":429,"error":"rate_limit","uuid":"u1","session_id":"sess-ratelimit-1"}
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":50,"session_id":"sess-ratelimit-1","result":"Stopped: usage limit reached, resets at $RATE_LIMIT_RESET_ISO"}
EOF

cat > "$FIXDIR/after_resume_success.jsonl" <<'EOF'
{"type":"result","subtype":"success","is_error":false,"num_turns":5,"session_id":"sess-ratelimit-1","result":"Shipped in PR #1000."}
EOF

cat > "$FIXDIR/other_error.jsonl" <<'EOF'
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":30,"session_id":"sess-error-1","result":"Unhandled exception: something broke."}
EOF

# --- One wave-file fixture per scenario, minimal on purpose ------------------
make_wave_file() {
  # $1 path, $2 specIssue, $3 slug, $4 second package's specIssue (0 = none)
  path=$1; issue=$2; slug=$3; second=$4
  extra=""
  if [ "$second" != "0" ]; then
    extra=",{\"specIssue\":$second,\"slug\":\"${slug}-second\",\"branch\":\"harness/${slug}-second\",\"spec\":\"Second package\",\"focus\":\"Never reached if the loop stops on the first.\"}"
  fi
  cat > "$path" <<EOF
{
  "plan": {"name": "Test Plan", "planIssue": 900, "conventions": "TEST-CONVENTIONS."},
  "standards": {"model": "claude-sonnet-5", "effort": "high", "advisor": false, "roundLimitPackage": 2},
  "waves": [
    {
      "number": 6,
      "waveIssue": 299,
      "integrationBranch": "integration/harness-welle-6",
      "packages": [
        {"specIssue": $issue, "slug": "$slug", "branch": "harness/$slug", "spec": "Test package", "focus": "Do the test thing."}$extra
      ]
    }
  ]
}
EOF
}

# --- Isolated copy of the script under test, PATH shims ----------------------
# Every scenario gets a fresh instance: reused mutable state (call logs, gh
# issue states, the lock directory) between scenarios would make one
# scenario's assertions depend on run order.
setup_scenario() {
  name=$1
  SC="$TESTBASE/$name"
  mkdir -p "$SC/repo/scripts" "$SC/bin" "$SC/state"
  cp "$REPO_ROOT/scripts/agent-loop.sh" "$SC/repo/scripts/agent-loop.sh"
  cp "$REPO_ROOT/scripts/package-prompt.template" "$SC/repo/scripts/package-prompt.template"
  chmod +x "$SC/repo/scripts/agent-loop.sh"

  CLAUDE_STUB_CALLS="$SC/state/claude-calls.log"
  GH_STUB_CALLS="$SC/state/gh-calls.log"
  GIT_STUB_CALLS="$SC/state/git-calls.log"
  GH_STUB_STATE_FILE="$SC/state/gh-issue-states"
  GH_STUB_COUNT_DIR="$SC/state/gh-view-counts"
  GH_STUB_ISSUES_FILE="$SC/state/gh-open-issues"
  GH_STUB_BODIES_DIR="$SC/state/gh-issue-bodies"
  CLAUDE_STUB_QUEUE="$SC/state/claude-queue"
  : > "$CLAUDE_STUB_CALLS"; : > "$GH_STUB_CALLS"; : > "$GIT_STUB_CALLS"
  : > "$GH_STUB_STATE_FILE"; : > "$CLAUDE_STUB_QUEUE"; : > "$GH_STUB_ISSUES_FILE"
  mkdir -p "$GH_STUB_COUNT_DIR" "$GH_STUB_BODIES_DIR"
  export CLAUDE_STUB_CALLS GH_STUB_CALLS GIT_STUB_CALLS GH_STUB_STATE_FILE GH_STUB_COUNT_DIR \
    GH_STUB_ISSUES_FILE GH_STUB_BODIES_DIR CLAUDE_STUB_QUEUE

  cat > "$SC/bin/claude" <<'STUB'
#!/bin/sh
# The prompt argument itself contains embedded newlines ("/pickup\n\nWork
# ..."), so a plain `wc -l` on this log cannot count invocations - a
# "===CALL===" marker line per call is what the test counts instead.
printf '===CALL===\n' >> "$CLAUDE_STUB_CALLS"
printf '%s\n' "$*" >> "$CLAUDE_STUB_CALLS"
if [ -s "$CLAUDE_STUB_QUEUE" ]; then
  fixture=$(head -n 1 "$CLAUDE_STUB_QUEUE")
  tail -n +2 "$CLAUDE_STUB_QUEUE" > "$CLAUDE_STUB_QUEUE.next" 2>/dev/null || : > "$CLAUDE_STUB_QUEUE.next"
  mv "$CLAUDE_STUB_QUEUE.next" "$CLAUDE_STUB_QUEUE"
else
  fixture=""
fi
[ -n "$fixture" ] && [ -f "$fixture" ] && cat "$fixture"
exit 0
STUB

  # State is expressed as "N THRESHOLD" (issue N reads CLOSED starting from
  # its THRESHOLD-th own "issue view --json state" call, OPEN before that;
  # no entry for N = always OPEN) rather than a fixed state, so a scenario
  # can model the ordinary case - the issue is still open when the loop
  # checks it before starting a session, and closed by the time it checks
  # again after the session (because the session's own ship-skill run is
  # what closed it) - without needing the stub to know anything about turns.
  cat > "$SC/bin/gh" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$GH_STUB_CALLS"
case "$*" in
  "issue view "*"--json state"*)
    n=$3
    count_file="$GH_STUB_COUNT_DIR/$n"
    count=0
    [ -f "$count_file" ] && count=$(cat "$count_file")
    count=$((count + 1))
    printf '%s\n' "$count" > "$count_file"
    threshold=$(awk -v n="$n" '$1==n{print $2; f=1} END{if(!f) print 0}' "$GH_STUB_STATE_FILE" 2>/dev/null)
    if [ "$threshold" -gt 0 ] 2>/dev/null && [ "$count" -ge "$threshold" ]; then
      printf 'CLOSED\n'
    else
      printf 'OPEN\n'
    fi
    exit 0 ;;
  "issue view "*"--json body"*)
    n=$3
    cat "$GH_STUB_BODIES_DIR/$n" 2>/dev/null
    exit 0 ;;
  "issue comment "*) exit 0 ;;
  "issue list "*)
    cat "$GH_STUB_ISSUES_FILE" 2>/dev/null
    exit 0 ;;
esac
exit 0
STUB

  cat > "$SC/bin/git" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$GIT_STUB_CALLS"
is_worktree_add=0
for a in "$@"; do [ "$a" = "worktree" ] && is_worktree_add=1; done
if [ "$is_worktree_add" -eq 1 ]; then
  last=""; secondlast=""
  for a in "$@"; do secondlast=$last; last=$a; done
  mkdir -p "$secondlast"
fi
exit 0
STUB

  # Real sleep would block on the rate-limit scenario's ~1-day reset time;
  # this records what it was asked to wait for and returns immediately.
  cat > "$SC/bin/sleep" <<'STUB'
#!/bin/sh
printf 'slept %s\n' "$*" >> "$SLEEP_STUB_CALLS"
exit 0
STUB
  chmod +x "$SC/bin/claude" "$SC/bin/gh" "$SC/bin/git" "$SC/bin/sleep"

  cat > "$SC/bin/doctor-pass.sh" <<'STUB'
#!/bin/sh
exit 0
STUB
  cat > "$SC/bin/doctor-fail.sh" <<'STUB'
#!/bin/sh
echo "FAIL docker daemon    docker info failed"
exit 1
STUB
  chmod +x "$SC/bin/doctor-pass.sh" "$SC/bin/doctor-fail.sh"

  SLEEP_STUB_CALLS="$SC/state/sleep-calls.log"
  : > "$SLEEP_STUB_CALLS"
  export SLEEP_STUB_CALLS

  SCPATH=$SC
}

run_loop() {
  # Runs agent-loop.sh with the scenario's PATH shims first and sane test
  # seam defaults, capturing combined output. Extra args are appended.
  PATH="$SCPATH/bin:$PATH" \
  AGENT_LOOP_UID="${TEST_UID:-1000}" \
  AGENT_LOOP_LOCK_DIR="$SCPATH/state/lock" \
  AGENT_LOOP_DOCTOR="${TEST_DOCTOR:-$SCPATH/bin/doctor-pass.sh}" \
  AGENT_LOOP_LOG_DIR="$SCPATH/state/logs" \
  CLAUDE_CODE_OAUTH_TOKEN="${TEST_OAUTH_TOKEN-test-oauth-token}" \
  sh "$SCPATH/repo/scripts/agent-loop.sh" "$@" >"$SCPATH/state/output.log" 2>&1
  echo $?
}

# =============================================================================
echo "== scenario: success closes the issue, loop moves on =="
setup_scenario success
make_wave_file "$SCPATH/wave.json" 101 w6-success 0
printf '%s\n' "$FIXDIR/success.jsonl" > "$SCPATH/state/claude-queue"
# Threshold 2: the pre-flight check (before the session starts) is this
# issue's 1st "view --json state" call and sees OPEN; decide_stop_action's
# own check afterwards is the 2nd and sees CLOSED - exactly the ordinary
# case, where the session's own ship-skill run is what closed the issue.
printf '101 2\n' > "$SCPATH/state/gh-issue-states"
rc=$(run_loop --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "loop exits 0 after a successful, closed package"
assert_contains "$out" "issue #101 closed" "log names the issue closed"
calls=$(cat "$SCPATH/state/claude-calls.log")
assert_contains "$calls" "--output-format stream-json" "claude invoked with stream-json output"
assert_not_contains "$calls" "--resume" "no resume on a plain success"
callcount=$(claude_call_count)
assert "$([ "$callcount" -eq 1 ] && echo 0 || echo 1)" "exactly one session ran (the issue closing on its own is not a resume trigger)"

echo "== scenario: success but issue still open (not one of D10's four - documented fallback) =="
setup_scenario success_open
make_wave_file "$SCPATH/wave.json" 102 w6-open 0
printf '%s\n' "$FIXDIR/success.jsonl" > "$SCPATH/state/claude-queue"
rc=$(run_loop --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "loop exits 0 even when the issue is still open after success"
assert_contains "$out" "still open" "log says the issue is still open rather than pretending it is done"

echo "== scenario: error_max_turns comments on the issue and moves on =="
setup_scenario maxturns
make_wave_file "$SCPATH/wave.json" 201 w6-maxturns 0
printf '%s\n' "$FIXDIR/max_turns.jsonl" > "$SCPATH/state/claude-queue"
rc=$(run_loop --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "loop exits 0 after error_max_turns"
assert_contains "$out" "hit the turn cap" "log names the turn cap"
ghcalls=$(cat "$SCPATH/state/gh-calls.log")
assert_contains "$ghcalls" "issue comment 201" "gh issue comment posted for #201"
assert_contains "$ghcalls" "--resume sess-maxturns-1" "the comment tells the human how to resume"

echo "== scenario: usage-limit backs off and resumes the same session =="
setup_scenario ratelimit
make_wave_file "$SCPATH/wave.json" 301 w6-ratelimit 0
{
  printf '%s\n' "$FIXDIR/rate_limit.jsonl"
  printf '%s\n' "$FIXDIR/after_resume_success.jsonl"
} > "$SCPATH/state/claude-queue"
rc=$(run_loop --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "loop exits 0 once the resumed session succeeds"
assert_contains "$out" "usage-limit" "log names the usage-limit condition"
assert_contains "$out" "$RATE_LIMIT_RESET_ISO" "log names the reset time found in the result text"
slept=$(cat "$SCPATH/state/sleep-calls.log")
assert_contains "$slept" "slept" "the loop actually waited (via the sleep stub) before resuming"
calls=$(cat "$SCPATH/state/claude-calls.log")
callcount=$(claude_call_count)
assert "$([ "$callcount" -eq 2 ] && echo 0 || echo 1)" "claude invoked exactly twice (initial + one resume)"
assert_contains "$calls" "continue --resume sess-ratelimit-1" "the second call resumes the exact session id from the first result"

echo "== scenario: any other error stops the WHOLE loop, second package never runs =="
setup_scenario othererror
make_wave_file "$SCPATH/wave.json" 401 w6-error 402
printf '%s\n' "$FIXDIR/other_error.jsonl" > "$SCPATH/state/claude-queue"
rc=$(run_loop --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$([ "$rc" -ne 0 ] && echo 0 || echo 1)" "loop exits non-zero on an unhandled error"
assert_contains "$out" "FATAL" "the failure is reported as fatal"
callcount=$(claude_call_count)
assert "$([ "$callcount" -eq 1 ] && echo 0 || echo 1)" "the second package's session never started"

echo "== scenario: lock file prevents a second loop =="
setup_scenario lock
make_wave_file "$SCPATH/wave.json" 501 w6-lock 0
mkdir -p "$SCPATH/state/lock"
rc=$(run_loop --dry-run --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$([ "$rc" -ne 0 ] && echo 0 || echo 1)" "a second loop refuses to start while the lock directory exists"
assert_contains "$out" "already running" "the refusal names the reason"
callcount=$(claude_call_count)
assert "$([ "$callcount" = "0" ] && echo 0 || echo 1)" "no session started while locked"

echo "== scenario: refuses to run as root =="
setup_scenario root
make_wave_file "$SCPATH/wave.json" 601 w6-root 0
TEST_UID=0
rc=$(run_loop --dry-run --wave-file "$SCPATH/wave.json" --wave 6)
TEST_UID=""
out=$(cat "$SCPATH/state/output.log")
assert "$([ "$rc" -ne 0 ] && echo 0 || echo 1)" "root is refused"
assert_contains "$out" "refusing to run as root" "the refusal is explicit"
assert "$([ ! -d "$SCPATH/state/lock" ] && echo 0 || echo 1)" "no lock taken when root is refused"

echo "== scenario: refuses when scripts/doctor fails =="
setup_scenario doctorfail
make_wave_file "$SCPATH/wave.json" 701 w6-doctor 0
TEST_DOCTOR="$SCPATH/bin/doctor-fail.sh"
rc=$(run_loop --dry-run --wave-file "$SCPATH/wave.json" --wave 6)
TEST_DOCTOR=""
out=$(cat "$SCPATH/state/output.log")
assert "$([ "$rc" -ne 0 ] && echo 0 || echo 1)" "a failing doctor refuses the loop"
assert_contains "$out" "scripts/doctor failed" "the refusal names scripts/doctor"
assert_contains "$out" "FAIL docker daemon" "the doctor's own FAIL line is shown"
callcount=$(claude_call_count)
assert "$([ "$callcount" = "0" ] && echo 0 || echo 1)" "no session started when the doctor fails"

echo "== scenario: refuses without CLAUDE_CODE_OAUTH_TOKEN =="
setup_scenario notoken
make_wave_file "$SCPATH/wave.json" 801 w6-notoken 0
TEST_OAUTH_TOKEN=""
rc=$(run_loop --dry-run --wave-file "$SCPATH/wave.json" --wave 6)
TEST_OAUTH_TOKEN="test-oauth-token"
out=$(cat "$SCPATH/state/output.log")
assert "$([ "$rc" -ne 0 ] && echo 0 || echo 1)" "a missing OAuth token refuses the loop"
assert_contains "$out" "CLAUDE_CODE_OAUTH_TOKEN" "the refusal names the missing variable"

echo "== scenario: --dry-run lists a closed package as skipped, with its would-be model/effort/ports =="
setup_scenario dryrun
make_wave_file "$SCPATH/wave.json" 901 w6-dryrun-a 902
printf '901 1\n' > "$SCPATH/state/gh-issue-states"
rc=$(run_loop --dry-run --wave-file "$SCPATH/wave.json" --wave 6)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "dry-run itself exits 0"
assert_contains "$out" "SKIPPED (issue #901 already closed)" "the closed package is marked skipped, not omitted"
assert_contains "$out" "model=claude-sonnet-5" "the dry-run line names the model"
assert_contains "$out" "would start" "the still-open second package is listed as would start"
callcount=$(claude_call_count)
assert "$([ "$callcount" = "0" ] && echo 0 || echo 1)" "--dry-run starts no real session"

echo "== scenario: --queue skips a blocked issue and works the lowest-numbered unblocked one =="
setup_scenario queue
printf '601\n602\n' > "$SCPATH/state/gh-open-issues"
printf 'Needs #999 first.\n\nBlocked by #999\n' > "$SCPATH/state/gh-issue-bodies/601"
printf 'No blockers here.\n' > "$SCPATH/state/gh-issue-bodies/602"
# #999 (601's blocker) is left OPEN (the gh stub's default) - 601 stays
# blocked and 602, with no blockers, is the one the loop should pick.
printf '%s\n' "$FIXDIR/success.jsonl" > "$SCPATH/state/claude-queue"
rc=$(run_loop --queue)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "queue mode exits 0"
gitcalls=$(cat "$SCPATH/state/git-calls.log")
assert_contains "$gitcalls" "issue-602" "the worktree created is for the unblocked issue, #602"
assert_not_contains "$gitcalls" "issue-601" "the blocked issue #601 is never touched"
calls=$(cat "$SCPATH/state/claude-calls.log")
assert_contains "$calls" "issue #602" "the rendered prompt names the issue actually worked"
ghcalls=$(cat "$SCPATH/state/gh-calls.log")
assert_contains "$ghcalls" "issue view 999 --json state" "the blocker (#999) state was actually consulted, not assumed"

echo "== scenario: --issue <N> works exactly that issue, bypassing the queue's own routing =="
setup_scenario oneissue
# No gh-open-issues / gh-issue-bodies fixtures at all: --issue must never call
# next_queue_issue's own "issue list"/"issue view --json body" machinery.
printf '%s\n' "$FIXDIR/success.jsonl" > "$SCPATH/state/claude-queue"
rc=$(run_loop --issue 703)
out=$(cat "$SCPATH/state/output.log")
assert "$rc" "--issue mode exits 0"
gitcalls=$(cat "$SCPATH/state/git-calls.log")
assert_contains "$gitcalls" "issue-703" "the worktree created is for the named issue, #703"
calls=$(cat "$SCPATH/state/claude-calls.log")
assert_contains "$calls" "issue #703" "the rendered prompt names issue #703"
ghcalls=$(cat "$SCPATH/state/gh-calls.log")
assert_not_contains "$ghcalls" "issue list" "no queue routing call is made in --issue mode"
assert_not_contains "$ghcalls" "--json body" "no blocked-by body scan is made in --issue mode"

echo
echo "== summary: $passes passed, $failures failed =="
[ "$failures" -eq 0 ]
exit $?
