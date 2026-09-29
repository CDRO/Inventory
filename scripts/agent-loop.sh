#!/bin/sh
# scripts/agent-loop.sh - runs Claude headlessly over the issue queue, one
# `claude -p` session per issue, stopping on an unhandled error and pausing
# across a usage-limit window. Contract: issue #318 ("H19") and
# docs/plans/2026-09-harness-optimization.md decision D10 (fetch it with
# `git show origin/harness/optimization-plan:docs/plans/2026-09-harness-optimization.md`
# until PR #319 merges it - no spec applies to this package).
#
# This is the Linux-side counterpart to scripts/wellen-orchestrator.ps1
# (decision D10, "a separate Linux entry point over the same wave JSON, no
# -Headless switch"): the orchestrator opens visible windows on the owner's
# Windows machine and is never taught to `docker run` a Linux session: this
# script is that session, run unattended, inside deploy/agent's container or
# on any Linux host with the same tools (git, gh, jq, claude, a writable
# Docker socket for scripts/doctor). Both scripts render the identical
# package prompt from scripts/package-prompt.template - see
# scripts/wellen-orchestrator.ps1's Get-PackagePrompt for the PowerShell half.
#
# Every gate stays inside the ship skill: this script only decides WHICH
# issue and WHEN. It never merges, reviews or closes anything itself - a
# session it starts does that, through /pickup and the ship skill, exactly
# as an interactive session would.
#
# Usage:
#   scripts/agent-loop.sh --wave-file <path> --wave <N> [options]
#   scripts/agent-loop.sh --queue [options]
#   scripts/agent-loop.sh --issue <N> [options]
#
# --issue <N> works exactly one named issue, bypassing --queue's own
# lowest-numbered-unblocked routing - for an operator (or a first real
# verification run) who already knows which issue to work, rather than
# whichever one the queue would pick next.
#
# Options:
#   --dry-run                     print the sessions that would run; start none
#   --limit-backoff-minutes <N>   usage-limit backoff cap when no reset time
#                                  is named in the result (default 60)
#   --max-resume-attempts <N>     usage-limit backoffs before giving up on one
#                                  issue and stopping the loop (default 24)
#   --model <name>                --queue/--issue only: model for the session
#                                  (default claude-sonnet-5)
#   --effort <level>               --queue/--issue only: effort (default high)
#   --advisor <model>               --queue/--issue only: enable the advisor
#   -h, --help                     this text
#
# Safety, in order, before any worktree or session is created:
#   1. refuses to run as root (never --dangerously-skip-permissions either -
#      D10: the non-root user plus the project allowlist is what CLI
#      "recognized sandbox" plans for permission bypass; this loop is not one)
#   2. one loop per repository: an exclusive lock directory
#   3. scripts/doctor must pass (the same doctor the orchestrator and
#      deploy/agent's entrypoint use) - a bad environment refuses before a
#      single session starts, not partway through one
#   4. CLAUDE_CODE_OAUTH_TOKEN must be set (never baked into an image - see
#      deploy/agent/.env.agent.example; never printed by this script)
#
# "Done" for an issue is the issue closed on GitHub - the same signal
# scripts/wellen-orchestrator.ps1 polls for - never the claude process's own
# exit code, which a round-limit report or a mid-turn resume can leave
# non-terminal in perfectly normal operation.
#
# Stop conditions, checked in this order once a session's stream ends
# (D10, verbatim from issue #318):
#   - the issue closed                              -> next issue
#   - result subtype error_max_turns                -> comment on the issue
#     ("resume with --resume <session_id>"), next issue
#   - a system/api_retry event with error "rate_limit", OR the result text
#     itself reading like a usage-limit message      -> pause (until a named
#     reset time, else --limit-backoff-minutes) and `claude -p "continue"
#     --resume <session_id>` on the SAME issue
#   - any other error (is_error true, no recognized signal above)
#                                                     -> STOP THE WHOLE LOOP,
#     printing the transcript's log path
#   - anything else (success, but the issue is still open - a round-limit
#     report, a PR still waiting on review) is treated like the first bullet
#     never fires: conservatively logged and treated as "next issue" rather
#     than retried unattended. Not one of D10's named conditions; called out
#     here because it is the one gap in that list this script had to decide.
#
# D10 flagged the exact usage-limit subtype/wording as unverified against a
# real run ("Capture one real result line on the first run and pin the field
# names in the test fixtures"); the two signals above are what the CLI's
# headless-mode docs confirm exist (system/api_retry's error field including
# "rate_limit"; a result subtype set including "success", "error_max_turns",
# "error_during_execution") - scripts/tests/agent-loop.test.sh's fixtures
# encode both, and the one real headless run (this package's own acceptance
# criterion) is what pins them for good.
#
# Test seams (not a supported interface, just what
# scripts/tests/agent-loop.test.sh overrides to drive this without touching
# the real machine or a real claude/gh/git):
#   AGENT_LOOP_UID          overrides `id -u` for the root-refusal check
#   AGENT_LOOP_LOCK_DIR     overrides the lock directory (also --lock-dir)
#   AGENT_LOOP_DOCTOR       overrides the doctor script path (default:
#                           scripts/doctor) - the same idea as
#                           wellen-orchestrator.ps1's own $DoctorScript
#                           variable, "purely so a test can point it at a
#                           stub without touching the real scripts/doctor"
#   AGENT_LOOP_LOG_DIR      overrides the per-session log directory (default:
#                           scripts/agent-loop.log, gitignored)
set -u

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
repo_name=$(basename "$root")
parent_dir=$(dirname "$root")

log()  { printf 'agent-loop: %s\n' "$*"; }
warn() { printf 'agent-loop: WARN: %s\n' "$*" >&2; }
die()  { printf 'agent-loop: FATAL: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
}

# --- Port allocation, kept in step with wellen-orchestrator.ps1 -------------
# Set-WorktreeEnvOverrides there uses the identical base numbers and the
# identical "package's ordinal position across the whole wave file" index, so
# a worktree this script creates and one the orchestrator creates never
# collide on host ports even if the same wave file is later opened by both
# (they are not meant to run at once - one metronome per wave file - but nothing
# stops an operator from checking with --dry-run while the orchestrator idles).
HTTP_PORT_BASE=18000
TRAEFIK_PORT_BASE=19000

MAX_RESUME_ATTEMPTS_DEFAULT=24
LIMIT_BACKOFF_MINUTES_DEFAULT=60

# --- Argument parsing --------------------------------------------------------
mode=""
wave_file=""
wave_num=""
issue_num=""
dry_run=0
limit_backoff_minutes=$LIMIT_BACKOFF_MINUTES_DEFAULT
max_resume_attempts=$MAX_RESUME_ATTEMPTS_DEFAULT
queue_model="claude-sonnet-5"
queue_effort="high"
queue_advisor_model=""
lock_dir_override=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --wave-file) wave_file=$2; shift 2 ;;
    --wave) mode="wave"; wave_num=$2; shift 2 ;;
    --queue) mode="queue"; shift ;;
    --issue) mode="issue"; issue_num=$2; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    --limit-backoff-minutes) limit_backoff_minutes=$2; shift 2 ;;
    --max-resume-attempts) max_resume_attempts=$2; shift 2 ;;
    --model) queue_model=$2; shift 2 ;;
    --effort) queue_effort=$2; shift 2 ;;
    --advisor) queue_advisor_model=$2; shift 2 ;;
    --lock-dir) lock_dir_override=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'agent-loop: unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$mode" in
  wave)
    [ -n "$wave_file" ] || die "--wave requires --wave-file <path>"
    [ -n "$wave_num" ] || die "internal error: wave mode with no wave number" ;;
  queue) : ;;
  issue)
    [ -n "$issue_num" ] || die "--issue requires an issue number" ;;
  *) printf 'agent-loop: pass --wave-file <path> --wave <N>, --queue, or --issue <N>\n' >&2; usage >&2; exit 2 ;;
esac

command -v jq >/dev/null 2>&1 || die "jq is required (present in deploy/agent's image; on another host, install it first)"
command -v gh >/dev/null 2>&1 || die "gh is required"
command -v git >/dev/null 2>&1 || die "git is required"
command -v claude >/dev/null 2>&1 || die "claude is required"

# --- Safety gate 1: never root -----------------------------------------------
effective_uid=${AGENT_LOOP_UID:-$(id -u)}
if [ "$effective_uid" = "0" ]; then
  die "refusing to run as root - run this as a non-root user under the project's .claude/settings.json allowlist (deploy/agent's own 'agent' user, uid 1000, or an equivalent). Never pass --dangerously-skip-permissions to work around this."
fi

# --- Safety gate 2: one loop per repository ----------------------------------
lock_dir=${lock_dir_override:-${AGENT_LOOP_LOCK_DIR:-"$root/scripts/agent-loop.lock"}}
if ! mkdir "$lock_dir" 2>/dev/null; then
  die "another agent-loop.sh is already running against this repository (lock: $lock_dir) - if you are certain none is, remove the directory and retry"
fi
printf '%s\n' "$$" > "$lock_dir/pid" 2>/dev/null || true
release_lock() { rm -f "$lock_dir/pid" 2>/dev/null; rmdir "$lock_dir" 2>/dev/null; }
trap release_lock EXIT INT TERM

# --- Safety gate 3: scripts/doctor must pass ---------------------------------
doctor_script=${AGENT_LOOP_DOCTOR:-"$root/scripts/doctor"}
doctor_output=$(sh "$doctor_script" 2>&1)
doctor_rc=$?
if [ "$doctor_rc" -ne 0 ]; then
  printf '%s\n' "$doctor_output" >&2
  die "scripts/doctor failed (exit $doctor_rc) - fix the FAIL line(s) above before running the loop"
fi

# --- Safety gate 4: the OAuth token, never printed ---------------------------
if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  die "CLAUDE_CODE_OAUTH_TOKEN must be set (mint one with 'claude setup-token' on a trusted machine, outside this container - see deploy/agent/README.md). Never bake it into an image."
fi

log_dir=${AGENT_LOOP_LOG_DIR:-"$root/scripts/agent-loop.log"}
mkdir -p "$log_dir" 2>/dev/null || die "cannot create log directory $log_dir"

# --- Template rendering (shared text, scripts/package-prompt.template) ------
# sed with '|' as the delimiter (branch names and prompt text both routinely
# contain '/'); every substituted value is escaped for '\', '&' and '|' so it
# is always read as a literal replacement, never as part of the pattern.
sed_escape() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'
}

render_package_prompt() {
  # args, in order: spec spec_issue wave_number plan_name integration_branch
  #                 plan_issue focus conventions wave_issue round_limit
  tmpl="$root/scripts/package-prompt.template"
  content=$(cat "$tmpl")
  content=$(printf '%s\n' "$content" | sed "s|{{SPEC}}|$(sed_escape "$1")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{SPEC_ISSUE}}|$(sed_escape "$2")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{WAVE_NUMBER}}|$(sed_escape "$3")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{PLAN_NAME}}|$(sed_escape "$4")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{INTEGRATION_BRANCH}}|$(sed_escape "$5")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{PLAN_ISSUE}}|$(sed_escape "$6")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{FOCUS}}|$(sed_escape "$7")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{CONVENTIONS}}|$(sed_escape "$8")|g")
  content=$(printf '%s\n' "$content" | sed "s|{{WAVE_ISSUE}}|$(sed_escape "$9")|g")
  shift 9
  content=$(printf '%s\n' "$content" | sed "s|{{ROUND_LIMIT}}|$(sed_escape "$1")|g")
  printf '%s' "$content"
}

# --- .env for a package worktree --------------------------------------------
# Mirrors New-PackageWorktree/Set-WorktreeEnvOverrides in
# wellen-orchestrator.ps1: copy the root clone's own .env when there is one
# (a PC session, same secrets as the operator's dev stack), otherwise the
# same CI-style ephemeral values .github/workflows/test.yml writes for a
# throwaway stack (no openssl in deploy/agent's image - /dev/urandom + od is
# the same entropy source, POSIX-portable, no extra package).
random_hex() {
  n=${1:-16}
  head -c "$n" /dev/urandom | od -An -tx1 | tr -d ' \n'
}

write_worktree_env() {
  wt=$1
  if [ -f "$root/.env" ]; then
    cp "$root/.env" "$wt/.env"
    return
  fi
  db_password="ci-$(random_hex 8)"
  session_secret="ci-$(random_hex 16)"
  admin_password="ci-$(random_hex 8)"
  cat > "$wt/.env" <<EOF
APP_ENV=dev
HTTP_PORT=8000
STATIC_DIR=/src/web/static
POSTGRES_USER=inventory
POSTGRES_PASSWORD=${db_password}
POSTGRES_DB=inventory
DATABASE_URL=postgres://inventory:${db_password}@db:5432/inventory?sslmode=disable
SESSION_SECRET=${session_secret}
ADMIN_INITIAL_USERNAME=admin
ADMIN_INITIAL_PASSWORD=${admin_password}
GEMINI_API_KEY=
GEMINI_MODEL=gemini-3.6-flash
GEMINI_IMAGE_MODEL=
SERPAPI_API_KEY=
EOF
}

set_env_value() {
  file=$1; key=$2; value=$3
  if grep -q "^${key}=" "$file" 2>/dev/null; then
    tmp="$file.tmp.$$"
    sed "s|^${key}=.*|${key}=${value}|" "$file" > "$tmp" && mv "$tmp" "$file"
  else
    printf '%s=%s\n' "$key" "$value" >> "$file"
  fi
}

sanitize_project_name() {
  v=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9_-]/-/g')
  case "$v" in
    [a-z0-9]*) printf '%s' "$v" ;;
    *) printf 'x-%s' "$v" ;;
  esac
}

apply_worktree_env_overrides() {
  # $1 worktree path, $2 slug, $3 port index (0-based, empty = no override)
  wt=$1; slug=$2; idx=$3
  [ -n "$idx" ] || return 0
  project_name=$(sanitize_project_name "${repo_name}-${slug}")
  http_port=$((HTTP_PORT_BASE + idx))
  traefik_port=$((TRAEFIK_PORT_BASE + idx))
  set_env_value "$wt/.env" COMPOSE_PROJECT_NAME "$project_name"
  set_env_value "$wt/.env" HTTP_PORT "$http_port"
  set_env_value "$wt/.env" TRAEFIK_PORT "$traefik_port"
  log "worktree env for '$slug': COMPOSE_PROJECT_NAME=$project_name HTTP_PORT=$http_port TRAEFIK_PORT=$traefik_port"
}

# --- Worktree lifecycle -------------------------------------------------------
# Sets $ensured_worktree on success - deliberately NOT "prints the path on
# stdout" for the caller to capture via $(...): a command substitution runs
# in a subshell, and die()'s `exit 1` inside one only kills that subshell,
# leaving the caller to silently continue with an empty result instead of
# actually stopping (#465 - observed live: a worktree-add failure fell
# through to running a real session directly against the primary checkout).
# Calling this directly, with no subshell around it, means die() here exits
# the real process.
ensure_worktree() {
  # $1 slug, $2 branch, $3 base branch
  slug=$1; branch=$2; base=$3
  wt="$parent_dir/$repo_name-$slug"
  if [ -d "$wt" ]; then
    warn "worktree '$wt' already exists - not created again"
    ensured_worktree="$wt"
    return 0
  fi
  git -C "$root" fetch origin >/dev/null 2>&1
  if ! git -C "$root" worktree add --no-track -b "$branch" "$wt" "origin/$base" >/dev/null 2>&1; then
    die "git worktree add failed for '$slug' (branch $branch from origin/$base)"
  fi
  ensured_worktree="$wt"
}

# --- One claude session -------------------------------------------------------
# One issue's whole transcript lives in a single .jsonl file (a usage-limit
# resume APPENDS to it, per the issue's own "log the stream to
# scripts/agent-loop.log/<slug>.jsonl", singular). Stop-condition detection,
# though, must only ever look at the LATEST call's own lines: a rate_limit
# api_retry event from the first call is still physically present in the file
# after a resume, and re-scanning the whole file on every pass would read that
# stale event as a fresh one and resume forever even once the resumed call
# succeeds. SESSION_SEGMENT (a plain "from this call's first new line to
# EOF" slice, refreshed every call) is what decide_stop_action actually reads;
# SESSION_LOG (the whole file) is only ever shown to a human in a log path.
#
# Globals set by run_claude_session / run_claude_resume:
#   SESSION_RC, SESSION_RESULT_LINE, SESSION_ID, SESSION_LOG, SESSION_SEGMENT
run_claude_session() {
  # $1 worktree, $2 prompt, $3 model, $4 effort, $5 advisor model (empty = off), $6 log file
  wt=$1; prompt=$2; model=$3; effort=$4; advisor_model=$5; logf=$6
  : > "$logf"
  advisor_args=""
  [ -n "$advisor_model" ] && advisor_args="--advisor $advisor_model"
  (
    cd "$wt" || exit 90
    CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1 \
    exec claude -p "$prompt" \
      --model "$model" --effort "$effort" $advisor_args \
      --permission-mode dontAsk --permission-prompts none \
      --max-turns 400 --output-format stream-json --verbose
  ) >"$logf" 2>"$logf.stderr"
  SESSION_RC=$?
  SESSION_LOG=$logf
  capture_segment "$logf" 0
  parse_result_line
}

run_claude_resume() {
  # $1 worktree, $2 session id, $3 log file (appended)
  wt=$1; sid=$2; logf=$3
  prev_lines=$(wc -l < "$logf" 2>/dev/null || printf '0')
  (
    cd "$wt" || exit 90
    CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1 \
    exec claude -p "continue" --resume "$sid" \
      --permission-mode dontAsk --permission-prompts none \
      --max-turns 400 --output-format stream-json --verbose
  ) >>"$logf" 2>>"$logf.stderr"
  SESSION_RC=$?
  SESSION_LOG=$logf
  capture_segment "$logf" "$prev_lines"
  parse_result_line
}

capture_segment() {
  # $1 log file, $2 line count before this call
  tail -n +"$(($2 + 1))" "$1" > "$1.segment"
  SESSION_SEGMENT="$1.segment"
}

parse_result_line() {
  # jq, not a substring grep: stream-json's actual spacing is not part of its
  # contract, and jq already reads a file of newline-separated JSON values
  # (JSON Lines) as a stream without needing --slurp.
  SESSION_RESULT_LINE=$(jq -c 'select(.type == "result")' "$SESSION_SEGMENT" 2>/dev/null | tail -n 1)
  SESSION_ID=""
  [ -n "$SESSION_RESULT_LINE" ] && SESSION_ID=$(printf '%s' "$SESSION_RESULT_LINE" | jq -r '.session_id // empty')
}

# --- Stop-condition decision (D10's list, verbatim - see header comment) ----
# Sets STOP_ACTION to one of: next | comment_next | resume | fatal
# Sets STOP_REASON to a human-readable line for the log.
decide_stop_action() {
  spec_issue=$1
  issue_state=$(gh issue view "$spec_issue" --json state -q .state 2>/dev/null)
  if [ "$issue_state" = "CLOSED" ]; then
    STOP_ACTION="next"; STOP_REASON="issue #$spec_issue closed"
    return
  fi
  if [ -z "$SESSION_RESULT_LINE" ]; then
    STOP_ACTION="fatal"; STOP_REASON="no result record in the stream (claude exited $SESSION_RC) - log: $SESSION_LOG"
    return
  fi
  subtype=$(printf '%s' "$SESSION_RESULT_LINE" | jq -r '.subtype // empty')
  is_error=$(printf '%s' "$SESSION_RESULT_LINE" | jq -r '.is_error // false')
  result_text=$(printf '%s' "$SESSION_RESULT_LINE" | jq -r '.result // empty')

  if [ "$subtype" = "error_max_turns" ]; then
    STOP_ACTION="comment_next"; STOP_REASON="issue #$spec_issue: session hit the turn cap"
    return
  fi

  retry_hit=$(jq -c 'select(.type == "system" and .subtype == "api_retry" and .error == "rate_limit")' "$SESSION_SEGMENT" 2>/dev/null | tail -n 1)
  rate_limited=0
  [ -n "$retry_hit" ] && rate_limited=1
  if [ "$rate_limited" -eq 1 ] || printf '%s' "$result_text" | grep -qiE 'usage limit|rate limit|resets at|try again (later|after)'; then
    STOP_ACTION="resume"; STOP_REASON="issue #$spec_issue: usage-limit signal (subtype=$subtype)"
    return
  fi

  if [ "$is_error" = "true" ]; then
    STOP_ACTION="fatal"; STOP_REASON="issue #$spec_issue: subtype=$subtype - $result_text - log: $SESSION_LOG"
    return
  fi

  # Success, but the issue is still open (a round-limit report, a PR still
  # waiting on review): not one of D10's four listed conditions. The
  # conservative choice - move to the next package rather than immediately
  # re-running an unattended session against the same still-open issue.
  STOP_ACTION="next"
  STOP_REASON="issue #$spec_issue: session ended (subtype=$subtype) but the issue is still open - not retried automatically this run"
}

backoff_and_wait() {
  # $1 result text (may name a reset time)
  reset_iso=$(printf '%s' "$1" | grep -oE '[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2})?(Z|[+-][0-9]{2}:?[0-9]{2})?' | head -n 1)
  if [ -n "$reset_iso" ]; then
    now_epoch=$(date -u +%s)
    reset_epoch=$(date -u -d "$reset_iso" +%s 2>/dev/null || printf '')
    if [ -n "$reset_epoch" ] && [ "$reset_epoch" -gt "$now_epoch" ] 2>/dev/null; then
      wait_seconds=$((reset_epoch - now_epoch))
      log "usage-limit: waiting until $reset_iso (${wait_seconds}s)"
      sleep "$wait_seconds"
      return
    fi
  fi
  wait_seconds=$((limit_backoff_minutes * 60))
  log "usage-limit: no reset time found in the result text, backing off ${limit_backoff_minutes}m"
  sleep "$wait_seconds"
}

# --- One package/issue, start to (a) done (b) resumed until done (c) fatal -
process_issue() {
  # args: spec_issue slug branch base_branch model effort advisor_model port_index(may be empty)
  spec_issue=$1; slug=$2; branch=$3; base=$4; model=$5; effort=$6; advisor_model=$7; port_index=$8

  state=$(gh issue view "$spec_issue" --json state -q .state 2>/dev/null)

  # --dry-run always shows the full would-be session (model/effort/ports),
  # an already-closed package included - marked SKIPPED rather than omitted,
  # so `--dry-run --wave-file scripts/wellen-harness.json --wave 6` still
  # names H18 and H19 themselves once both are closed (this package's own
  # acceptance criterion).
  if [ "$dry_run" -eq 1 ]; then
    ports=""
    if [ -n "$port_index" ]; then
      ports=" HTTP_PORT=$((HTTP_PORT_BASE + port_index)) TRAEFIK_PORT=$((TRAEFIK_PORT_BASE + port_index))"
    fi
    advisor_note=""
    [ -n "$advisor_model" ] && advisor_note=" advisor=$advisor_model"
    status_note=" - would start"
    [ "$state" = "CLOSED" ] && status_note=" - SKIPPED (issue #$spec_issue already closed)"
    log "[dry-run] issue #$spec_issue slug=$slug branch=$branch<-origin/$base model=$model effort=$effort${advisor_note}${ports}${status_note}"
    return 0
  fi

  if [ "$state" = "CLOSED" ]; then
    log "skip: issue #$spec_issue ('$slug') is already closed"
    return 0
  fi

  ensure_worktree "$slug" "$branch" "$base"
  wt="$ensured_worktree"
  write_worktree_env "$wt"
  apply_worktree_env_overrides "$wt" "$slug" "$port_index"

  logf="$log_dir/$slug.jsonl"
  prompt=$9

  attempt=0
  run_claude_session "$wt" "$prompt" "$model" "$effort" "$advisor_model" "$logf"
  while :; do
    decide_stop_action "$spec_issue"
    case "$STOP_ACTION" in
      next)
        log "$STOP_REASON"
        return 0 ;;
      comment_next)
        log "$STOP_REASON"
        gh issue comment "$spec_issue" --body "headless session hit the turn cap; resume with \`--resume $SESSION_ID\`" >/dev/null 2>&1
        return 0 ;;
      resume)
        attempt=$((attempt + 1))
        if [ "$attempt" -gt "$max_resume_attempts" ]; then
          printf 'agent-loop: FATAL: %s - too many usage-limit backoffs (%s) - log: %s\n' "$STOP_REASON" "$attempt" "$logf" >&2
          return 1
        fi
        result_text=$(printf '%s' "$SESSION_RESULT_LINE" | jq -r '.result // empty')
        log "$STOP_REASON (backoff attempt $attempt/$max_resume_attempts)"
        backoff_and_wait "$result_text"
        run_claude_resume "$wt" "$SESSION_ID" "$logf"
        continue ;;
      fatal)
        printf 'agent-loop: FATAL: %s\n' "$STOP_REASON" >&2
        return 1 ;;
    esac
  done
}

# --- Wave-file mode ------------------------------------------------------------
run_wave_mode() {
  [ -f "$wave_file" ] || die "wave file not found: $wave_file"
  wave_json=$(jq -c --argjson n "$wave_num" '.waves[] | select(.number == $n)' "$wave_file")
  [ -n "$wave_json" ] || die "wave $wave_num not found in $wave_file"

  plan_name=$(jq -r '.plan.name' "$wave_file")
  plan_issue=$(jq -r '.plan.planIssue' "$wave_file")
  plan_conventions=$(jq -r '.plan.conventions // empty' "$wave_file")
  std_model=$(jq -r '.standards.model' "$wave_file")
  std_effort=$(jq -r '.standards.effort' "$wave_file")
  std_advisor=$(jq -r '.standards.advisor' "$wave_file")
  round_limit=$(jq -r '.standards.roundLimitPackage // 2' "$wave_file")

  wave_plan_issue=$(printf '%s' "$wave_json" | jq -r '.planIssue // empty')
  wave_plan_name=$(printf '%s' "$wave_json" | jq -r '.planName // empty')
  effective_plan_issue=${wave_plan_issue:-$plan_issue}
  effective_plan_name=${wave_plan_name:-$plan_name}
  integration_branch=$(printf '%s' "$wave_json" | jq -r '.integrationBranch')
  wave_issue=$(printf '%s' "$wave_json" | jq -r '.waveIssue')

  conventions_filled=""
  [ -n "$plan_conventions" ] && conventions_filled="$plan_conventions "

  count=$(printf '%s' "$wave_json" | jq '.packages | length')
  i=0
  while [ "$i" -lt "$count" ]; do
    pkg=$(printf '%s' "$wave_json" | jq -c ".packages[$i]")
    spec_issue=$(printf '%s' "$pkg" | jq -r '.specIssue')
    slug=$(printf '%s' "$pkg" | jq -r '.slug')
    branch=$(printf '%s' "$pkg" | jq -r '.branch')
    spec=$(printf '%s' "$pkg" | jq -r '.spec')
    focus=$(printf '%s' "$pkg" | jq -r '.focus')
    pkg_model=$(printf '%s' "$pkg" | jq -r '.model // empty')
    pkg_effort=$(printf '%s' "$pkg" | jq -r '.effort // empty')
    pkg_advisor=$(printf '%s' "$pkg" | jq -r '.advisor // empty')
    model=${pkg_model:-$std_model}
    effort=${pkg_effort:-$std_effort}
    advisor_enabled=${pkg_advisor:-$std_advisor}
    advisor_model=""
    [ "$advisor_enabled" = "true" ] && advisor_model=$model

    port_index=$(jq -r --argjson si "$spec_issue" '[.waves[].packages[]?.specIssue] | index($si)' "$wave_file")

    prompt=$(render_package_prompt "$spec" "$spec_issue" "$wave_num" "$effective_plan_name" \
      "$integration_branch" "$effective_plan_issue" "$focus" "$conventions_filled" "$wave_issue" "$round_limit")

    process_issue "$spec_issue" "$slug" "$branch" "$integration_branch" "$model" "$effort" "$advisor_model" "$port_index" "$prompt"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      die "stopping the loop: package '$slug' (issue #$spec_issue) hit an unhandled error"
    fi
    i=$((i + 1))
  done
}

# --- Queue mode -----------------------------------------------------------
# Mirrors /pickup's own routing ("the lowest-numbered issue whose every
# 'Blocked by #N' is closed"), one issue per invocation of this loop's body -
# a fresh --queue run picks the next one once the previous issue closes.
next_queue_issue() {
  numbers=$(gh issue list --state open --json number -q 'sort_by(.number) | .[].number' 2>/dev/null)
  for n in $numbers; do
    body=$(gh issue view "$n" --json body -q .body 2>/dev/null)
    blockers=$(printf '%s' "$body" | grep -oE 'Blocked by #[0-9]+' | grep -oE '[0-9]+')
    blocked=0
    for b in $blockers; do
      bstate=$(gh issue view "$b" --json state -q .state 2>/dev/null)
      [ "$bstate" = "CLOSED" ] || { blocked=1; break; }
    done
    if [ "$blocked" -eq 0 ]; then
      printf '%s' "$n"
      return 0
    fi
  done
  return 1
}

work_single_issue() {
  # $1 = the issue number to work, chosen by the caller (next_queue_issue's
  # routing, or an operator naming one directly with --issue).
  spec_issue=$1
  slug="issue-$spec_issue"
  branch="agent-loop/issue-$spec_issue"
  advisor_model=""
  [ -n "$queue_advisor_model" ] && advisor_model=$queue_advisor_model
  prompt=$(printf '/pickup\n\nWork issue #%s through the full ship loop. Stop and report if you hit the round limit (2).\n' "$spec_issue")
  process_issue "$spec_issue" "$slug" "$branch" "main" "$queue_model" "$queue_effort" "$advisor_model" "" "$prompt"
  rc=$?
  [ "$rc" -eq 0 ] || die "stopping the loop: issue #$spec_issue hit an unhandled error"
}

run_queue_mode() {
  spec_issue=$(next_queue_issue) || { log "no unblocked issue in the queue"; return 0; }
  work_single_issue "$spec_issue"
}

# --- Entry point --------------------------------------------------------------
case "$mode" in
  wave) run_wave_mode ;;
  queue) run_queue_mode ;;
  issue) work_single_issue "$issue_num" ;;
esac
