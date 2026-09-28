#!/bin/bash
# deploy/agent/entrypoint.sh — refuses to start a session until
# scripts/doctor passes. Contract: issue #317 ("H18"),
# docs/plans/2026-09-harness-optimization.md decision D10.
#
# Runs as root, the image's default user, because only root can join the
# group that owns the mounted Docker socket — its GID is a property of the
# HOST and is therefore only known at container start, never at build time.
# It fixes that up, then drops to the non-root `agent` user via gosu (a real
# exec, so `agent`'s process replaces this one instead of running as a `su`
# child that would swallow SIGTERM) before running a single line of
# scripts/doctor or the ship loop.
set -euo pipefail

SOCKET=/var/run/docker.sock
AGENT_USER=agent

say() { printf 'agent-entrypoint: %s\n' "$*"; }
die() { printf 'agent-entrypoint: FATAL: %s\n' "$*" >&2; exit 1; }

# Not `: "${REPO_PATH:?message}"`: bash's own parameter-expansion error prints
# "<script>: line N: REPO_PATH: message" straight to stderr, bypassing die()
# and its "agent-entrypoint: FATAL:" prefix entirely — the exact prefix
# README.md's troubleshooting table documents for this failure.
if [ -z "${REPO_PATH:-}" ]; then
  die "REPO_PATH must be set to the absolute host path of this repository, see deploy/agent/README.md"
fi

if [ "$#" -eq 0 ]; then
  set -- claude
fi

# 1. Join the socket's group, when the socket is mounted at all. Dynamic
#    rather than a documented --group-add: the socket's GID differs by host
#    (Docker Desktop, a Linux distro's own "docker" group, the NAS's
#    root-owned socket — deploy/synology/runner/Dockerfile's decision D4
#    hits the same fact from the other side), and reading it here means the
#    operator never has to look it up and pass it through Compose.
if [ -S "$SOCKET" ]; then
  sock_gid=$(stat -c '%g' "$SOCKET")
  group_name=$(getent group "$sock_gid" | cut -d: -f1)
  if [ -z "$group_name" ]; then
    group_name=docker-host
    groupadd -g "$sock_gid" "$group_name"
  fi
  usermod -aG "$group_name" "$AGENT_USER"
  say "joined $AGENT_USER to group $group_name (gid $sock_gid) for $SOCKET"
else
  say "$SOCKET is not mounted - scripts/doctor below will say so"
fi

# 2. Git identity from the environment. scripts/doctor's own check
#    (check_git_identity_and_line_endings) reads `git config`, not the
#    GIT_AUTHOR_*/GIT_COMMITTER_* variables `git commit` honours directly —
#    so those variables alone would make every commit work while still
#    failing the doctor check. Translated into config here so both agree.
#    GIT_COMMITTER_NAME/EMAIL are accepted the same way but are not required:
#    git already falls back to the author identity for the committer when
#    only that is set.
#
#    Best-effort, deliberately not fatal on its own: `git config --global`
#    can itself fail here (for example when $REPO_PATH is a worktree whose
#    .git file points at a gitdir outside the mount — git's repository
#    discovery runs before the global write lands). scripts/doctor's own
#    identity check is the authoritative gate below; a raw crash here would
#    only hide its clean FAIL-with-remediation behind an unhelpful one.
if [ -n "${GIT_AUTHOR_NAME:-}" ] && [ -n "${GIT_AUTHOR_EMAIL:-}" ]; then
  gosu "$AGENT_USER" git config --global user.name "$GIT_AUTHOR_NAME" || true
  gosu "$AGENT_USER" git config --global user.email "$GIT_AUTHOR_EMAIL" || true
  if [ -n "${GIT_COMMITTER_NAME:-}" ]; then
    gosu "$AGENT_USER" git config --global committer.name "$GIT_COMMITTER_NAME" || true
  fi
  if [ -n "${GIT_COMMITTER_EMAIL:-}" ]; then
    gosu "$AGENT_USER" git config --global committer.email "$GIT_COMMITTER_EMAIL" || true
  fi
fi

cd "$REPO_PATH"

# 3. The doctor itself, as `agent` — the same user, and therefore the same
#    group membership and the same failure mode, as whatever runs after it.
#    `sh scripts/doctor`, not `scripts/doctor` or `./scripts/doctor`: the
#    file is checked into git without the execute bit (like the rest of
#    scripts/, see scripts/dev.d/doctor's own forward), and a bind mount
#    from a host that preserves that bit would otherwise fail this step with
#    a plain "Permission denied" instead of the doctor's own remediation.
doctor_failed=0
doctor_json=$(gosu "$AGENT_USER" sh scripts/doctor --json) || doctor_failed=1
printf '%s\n' "$doctor_json" | jq -r '.[] | [.status, .check, .remediation] | @tsv' |
  while IFS="$(printf '\t')" read -r status check remediation; do
    if [ -n "$remediation" ]; then
      printf '%-4s %-40s %s\n' "$status" "$check" "$remediation"
    else
      printf '%-4s %-40s\n' "$status" "$check"
    fi
  done

if [ "$doctor_failed" -eq 1 ]; then
  die "scripts/doctor failed - fix the FAIL line(s) above, then re-run"
fi
say "doctor passed"

# 4. Run what was asked for, as `agent`. The same execute-bit accommodation
#    as step 3: `docker compose run --rm agent scripts/doctor` (this
#    package's own acceptance criterion) names the file directly, so a
#    non-executable checkout would otherwise turn a deliberate re-run of the
#    doctor into the same "Permission denied" step 3 exists to avoid.
cmd="$1"
if [ -f "$cmd" ] && [ ! -x "$cmd" ]; then
  set -- sh "$@"
fi
say "starting: $*"
exec gosu "$AGENT_USER" "$@"
