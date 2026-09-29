#!/bin/bash
# Start-up smoke test, register-once, then run forever.
#
# Contract: docs/specs/38-release-pipeline-and-nas-runner.md, "The runner: a
# container on the NAS" (the smoke test) and "Registration and privilege
# (decision D4)" (everything under "Registration state" below). The operator
# instructions are in deploy/synology/runner/README.md, beside this file.
#
# The smoke test exists because of the failure it prevents: a runner that starts,
# reports itself idle to GitHub, accepts a deploy job and only then discovers it
# cannot reach the Docker socket has already taken the release off the queue and
# left the NAS half-updated. Checking the things a deploy needs before taking any
# work turns that into a container that will not start, which is visible in
# "docker-compose ps" and in DSM's own container list.

set -eu

RUNNER_HOME=/home/runner
STATE_DIR=${RUNNER_STATE_DIR:-/runner/state}
CLONE=${INVENTORY_CLONE:-/volume1/docker/inventory}
WORK_DIR=${RUNNER_WORK_DIR:-$RUNNER_HOME/_work}

# .credentials_rsaparams is carried along when it is there but is not part of the
# "is this runner registered?" test: .runner plus .credentials are the two the
# listener cannot start without, and an agent version that stops writing the
# third must not make a registered runner look unregistered.
STATE_FILES=".runner .credentials .credentials_rsaparams"
REQUIRED_STATE=".runner .credentials"

say() { printf '%s\n' "runner: $*"; }
die() { printf '%s\n' "runner: FATAL: $*" >&2; exit 1; }

# --- The smoke test -----------------------------------------------------------

smoke_failed=0
ok()  { printf '%s\n' "smoke: ok    $*"; }
bad() { printf '%s\n' "smoke: FAIL  $*" >&2; smoke_failed=1; }

smoke_test() {
  # 1. The standalone docker-compose, at the version floor the compose files
  #    need. Same check, same numbers and same reason as
  #    deploy/synology/update's own preconditions: the long-form env_file with
  #    "required: false" is a 2.24 feature, and Container Manager bundles 2.20.1.
  local v major rest minor
  v=$(docker-compose version --short 2>/dev/null | sed 's/^v//') || v=
  if [ -z "$v" ]; then
    bad "docker-compose does not run. The image is broken - rebuild it."
  else
    major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
    if [ "$major" -gt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -ge 24 ]; }; then
      ok "docker-compose $v (>= 2.24)"
    else
      bad "docker-compose $v is below the 2.24 these compose files need. The image is broken - rebuild it."
    fi
  fi

  # 2. git. "update --ref" fetches and checks out with it; without it the deploy
  #    cannot even find out what it is deploying.
  if git --version >/dev/null 2>&1; then
    ok "$(git --version)"
  else
    bad "git does not run. The image is broken - rebuild it."
  fi

  # 3. The Docker socket. Split into "is it mounted" and "does it answer",
  #    because the fixes are different: a missing mount is this compose file, a
  #    socket that does not answer is the host's daemon.
  #    The literal path, deliberately not an override: both halves of the mount
  #    in docker-compose.runner.yml are that path, so a variable here could only
  #    ever disagree with the thing it is checking.
  local socket=/var/run/docker.sock
  if [ ! -S "$socket" ]; then
    bad "$socket is not a socket inside the container. Check the volumes in docker-compose.runner.yml."
  elif ! docker info >/dev/null 2>&1; then
    bad "$socket is mounted but the daemon does not answer. Is Docker running on the host?"
  else
    ok "docker socket $socket answers (server $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown))"
  fi

  # 4. The clone, at the path it has on the host. The identical path is
  #    load-bearing rather than tidy: docker-compose.nas.yml's bind mounts are
  #    relative to the clone, and a container this runner starts through the
  #    socket has those paths resolved by the HOST's daemon - so a clone mounted
  #    anywhere else would bind the wrong host directories and say nothing.
  if [ ! -d "$CLONE" ]; then
    bad "the clone is not mounted at $CLONE. It must be mounted at the same absolute path it has on the host (INVENTORY_CLONE)."
  elif ! git -C "$CLONE" rev-parse --git-dir >/dev/null 2>&1; then
    bad "$CLONE exists but is not a git checkout. \"update --ref\" fetches through this clone's own remote."
  else
    ok "clone $CLONE is a git checkout at $(git -C "$CLONE" rev-parse --short HEAD 2>/dev/null || echo 'an unknown commit')"
  fi

  # 5. The app stack's two compose files parse. This is the check that catches
  #    the interesting case: a Compose new enough to run but too old for - or
  #    incompatible with - the files it will be asked to deploy. Written out as
  #    the documented prefix, so the line in the log is the line an operator
  #    would type.
  if [ -f "$CLONE/docker-compose.yml" ] && [ -f "$CLONE/docker-compose.nas.yml" ]; then
    if docker-compose -p inventory \
         -f "$CLONE/docker-compose.yml" -f "$CLONE/docker-compose.nas.yml" \
         config -q; then
      ok "docker-compose config accepts the app stack's two files"
    else
      bad "docker-compose cannot parse $CLONE/docker-compose.yml + docker-compose.nas.yml. A deploy would fail in the middle."
    fi
  else
    bad "$CLONE holds no docker-compose.yml + docker-compose.nas.yml pair. Is INVENTORY_CLONE pointing at the clone?"
  fi

  # 6. The clone's remote is reachable FROM IN HERE. The check that would have
  #    caught #409 at start-up instead of at the first release: the container
  #    shares the clone's .git with the host, and therefore its remote URL, but
  #    not $HOME, so an SSH remote the NAS fetches from happily fails in here
  #    with "Host key verification failed". `ls-remote` asks exactly the
  #    question `update --ref` will ask, without fetching anything.
  if [ -d "$CLONE/.git" ]; then
    if git -C "$CLONE" ls-remote --exit-code origin HEAD >/dev/null 2>&1; then
      ok "git ls-remote origin answers from inside this container"
    else
      bad "git cannot reach origin ($(git -C "$CLONE" remote get-url origin 2>/dev/null || echo 'no origin')) from inside this container, though it may well work on the NAS itself. Every deploy would refuse at phase=fetch. See deploy/synology/runner/README.md, 'The git transport'."
    fi
  fi

  [ "$smoke_failed" -eq 0 ] || die "start-up smoke test failed (see the FAIL lines above). The runner has taken no work and registered nothing."
  say "smoke test passed"
}

# --- Registration state -------------------------------------------------------

# .runner and .credentials are what survive a DSM reboot, and they live in the
# bind-mounted state directory rather than in the container's writable layer so
# that "up --force-recreate", a rebuilt image and a power cut all come back with
# the same registration and no token (decision D4). config.sh writes them into
# its own directory ($RUNNER_HOME) and has no flag to put them elsewhere, so they
# are copied in at start and copied out once, after registering. Copied rather
# than symlinked on purpose: the listener is free to write them through a
# temporary file and a rename, which would replace a symlink with a regular file
# in the container layer and quietly stop persisting anything.
restore_state() {
  local f
  for f in $STATE_FILES; do
    if [ -f "$STATE_DIR/$f" ]; then
      cp -p "$STATE_DIR/$f" "$RUNNER_HOME/$f"
    fi
  done
}

save_state() {
  local f
  for f in $STATE_FILES; do
    if [ -f "$RUNNER_HOME/$f" ]; then
      cp -p "$RUNNER_HOME/$f" "$STATE_DIR/$f"
      chmod 600 "$STATE_DIR/$f"
    fi
  done
}

is_registered() {
  local f
  for f in $REQUIRED_STATE; do
    [ -f "$RUNNER_HOME/$f" ] || return 1
  done
  return 0
}

register() {
  if [ -z "${RUNNER_TOKEN:-}" ]; then
    die "this runner is not registered yet and RUNNER_TOKEN is empty. Mint a
  one-hour registration token on your own machine and start the container once
  with it:
    gh api -X POST repos/CDRO/Inventory/actions/runners/registration-token -q .token
    RUNNER_TOKEN=<that token> docker-compose -f docker-compose.runner.yml up -d
  See deploy/synology/runner/README.md."
  fi

  say "registering \"$RUNNER_NAME\" at $RUNNER_URL with labels $RUNNER_LABELS"
  # --replace so that re-registering the same name reclaims the existing runner
  # instead of leaving an offline duplicate behind; --unattended so a missing
  # answer is an error instead of a prompt nobody is there to read. Persistent,
  # NOT --ephemeral: --ephemeral de-registers after one job and so needs a fresh
  # registration token per job, which would mean a classic PAT with "repo" scope
  # living on the NAS permanently - a broader credential kept longer, to defend
  # against job-to-job residue on a runner that runs one job from one repository
  # (decision D4).
  ./config.sh \
    --url "$RUNNER_URL" \
    --token "$RUNNER_TOKEN" \
    --name "$RUNNER_NAME" \
    --labels "$RUNNER_LABELS" \
    --work "$WORK_DIR" \
    --replace \
    --unattended

  save_state
  say "registration stored in $STATE_DIR - later starts need no token"
}

# --- Start --------------------------------------------------------------------

cd "$RUNNER_HOME"

mkdir -p "$STATE_DIR" "$WORK_DIR"

# Not hygiene: without this git refuses every command on a clone whose owner is
# not the current user ("detected dubious ownership"), which is exactly what a
# bind-mounted clone looks like from inside a container anywhere but the NAS
# (where the clone is root-owned and this line is a no-op). Scoped to the one
# path this container is allowed to care about, never "*".
git config --global --add safe.directory "$CLONE" 2>/dev/null || true

# Also not hygiene, and for the same shape of reason: this container shares the
# clone's .git with the host, so it shares the remote URL — but it does NOT
# share $HOME, which is where known_hosts and keys live. An SSH remote that the
# NAS itself fetches from happily therefore fails in here with "Host key
# verification failed", and every release deploy dies at phase=fetch (#409).
#
# Rewriting the transport container-side fixes that without touching anything
# the host shares: the NAS's own `git pull` keeps using SSH, and the fix
# survives a re-clone, which a `git remote set-url` on the clone does not.
#
# CONDITIONAL, and the condition is the point: an anonymous HTTPS fetch only
# works while the repository is PUBLIC. Once it is private the container needs
# the read-only deploy key instead, and this rewrite would then send every fetch
# down a transport that cannot authenticate. So it applies only while no SSH
# private key is mounted — mount one (the going-private step) and the rewrite
# disables itself.
if ls "$HOME"/.ssh/id_* >/dev/null 2>&1; then
  echo "runner: an SSH key is present - leaving the git transport alone."
else
  # --add, not a plain set: both spellings of an SSH GitHub remote have to be
  # rewritten, and `git config` without --add REPLACES the previous value, so
  # setting the same key twice would silently keep only the second.
  git config --global --add url."https://github.com/".insteadOf "git@github.com:" 2>/dev/null || true
  git config --global --add url."https://github.com/".insteadOf "ssh://git@github.com/" 2>/dev/null || true
fi

smoke_test

# The escape hatch CI and a cautious operator use: prove the image can do the
# five things above without registering anything or taking a job.
if [ "${RUNNER_SMOKE_ONLY:-}" = "1" ]; then
  say "RUNNER_SMOKE_ONLY=1 - exiting without registering"
  exit 0
fi

restore_state

if is_registered; then
  say "already registered as \"$RUNNER_NAME\" (state from $STATE_DIR) - starting"
else
  register
fi

# The registration token is single-use and expires within the hour, but it has no
# business being readable by a workflow step either. Dropped before the listener
# - and therefore before any job - ever exists.
unset RUNNER_TOKEN

# exec, so the listener is PID 1 and DSM's stop (SIGTERM) reaches it directly.
# The base image sets RUNNER_MANUALLY_TRAP_SIG=1, which makes run.sh install its
# own INT/TERM handler and finish the job in flight before exiting.
say "starting the listener"
exec ./run.sh
