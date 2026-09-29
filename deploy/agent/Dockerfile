# deploy/agent/Dockerfile — a Claude Code container that checks its own
# environment before it does anything. Contract: issue #317 ("H18") and
# docs/plans/2026-09-harness-optimization.md, section 3.4 and decision D10
# (fetched with `git show origin/harness/optimization-plan:...` until PR #319
# merges it — no spec applies to this package). Read deploy/agent/README.md
# before you run this.
#
# "I started running claude in a docker container that could not run other
# docker containers because of rights" — the failure surfaced deep into a
# session. entrypoint.sh, beside this file, is what turns that into a
# container that refuses to start instead of one that fails mid-session.
#
# linux/amd64 only (D10). The Docker daemon is always the HOST's, reached
# through /var/run/docker.sock — this image installs the Docker CLI and the
# Compose plugin as CLIENTS only, never a second daemon.

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

# --- Base tools -------------------------------------------------------------
# git, curl, jq, ca-certificates, gnupg: what scripts/doctor and the ship loop
# need, plus what Docker's own apt repo setup below needs to add itself. Not
# pinned individually — they ride the base image's own digest above, the same
# convention the root Dockerfile's builder/dev stages use for their apk
# packages.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg git jq \
 && rm -rf /var/lib/apt/lists/*

# --- The `agent` user (uid 1000), per decision D10 --------------------------
# Created before anything is installed into its $HOME, so the claude install
# below (run as this user) lands under /home/agent rather than /root, where a
# later non-root process could not read it. -m gives it a real $HOME;
# useradd's own default (Debian: USERGROUPS_ENAB yes) creates a matching
# private group, which is what entrypoint.sh's group-add targets.
RUN useradd -m -u 1000 -s /bin/bash agent
ENV PATH="/home/agent/.local/bin:${PATH}"

# --- Docker CLI + Compose plugin (client only) -------------------------------
# Docker's own apt repository, pinned to exact package versions so a rebuild
# in six months does not silently change what "docker compose" means inside
# this image. scripts/doctor's own floor is Compose >= 2.24
# (meets_compose_floor); the Compose plugin's own version numbering moved past
# the 2.x line during this project's lifetime (2.31 -> 5.x), and that floor
# check already treats any major version greater than 2 as meeting it, so a
# later bump of the pin below stays compatible without touching scripts/doctor.
RUN install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
 && chmod a+r /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      docker-ce-cli=5:29.8.1-1~debian.12~bookworm \
      docker-compose-plugin=5.5.1-1~debian.12~bookworm \
 && rm -rf /var/lib/apt/lists/* /etc/apt/sources.list.d/docker.list

# --- gh (GitHub CLI) ----------------------------------------------------------
# Pinned tarball + checksum, the same pattern as the NAS runner's standalone
# docker-compose (deploy/synology/runner/Dockerfile): a released artifact that
# changes under the tag fails this build instead of installing something
# nobody reviewed. Checksum from cli/cli's own published
# gh_<version>_checksums.txt.
ARG GH_VERSION=2.96.0
ARG GH_SHA256=83d5c2ccad5498f58bf6368acb1ab32588cf43ab3a4b1c301bf36328b1c8bd60
RUN curl -fsSL -o /tmp/gh.tar.gz \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
 && echo "${GH_SHA256}  /tmp/gh.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/gh.tar.gz -C /tmp \
 && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh \
 && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_VERSION}_linux_amd64" \
 && gh --version

# --- gosu (root -> agent at container start, with no wrapper left behind) ---
# entrypoint.sh has to start as root (only root can join the mounted socket's
# group — its GID is only known at container start, never at build time) and
# then drop to `agent` before running a single line of the ship loop. gosu
# execs directly, so `agent`'s process replaces PID 1 instead of living under
# a `su`/`sudo` parent that would swallow signals. Pinned + checksummed for
# the same reason as gh above: this is the one piece of software in the image
# whose entire job is the privilege drop, so it gets the same scrutiny.
ARG GOSU_VERSION=1.19
ARG GOSU_SHA256=52c8749d0142edd234e9d6bd5237dff2d81e71f43537e2f4f66f75dd4b243dd0
RUN curl -fsSL -o /usr/local/bin/gosu \
      "https://github.com/tianon/gosu/releases/download/${GOSU_VERSION}/gosu-amd64" \
 && echo "${GOSU_SHA256}  /usr/local/bin/gosu" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/gosu \
 && gosu --version

# --- claude, the native installer, pinned ------------------------------------
# Run as `agent`, not root: the installer writes under $HOME
# (~/.local/bin/claude, ~/.local/share/claude/versions/…), and running it here
# as the user that will actually invoke `claude` at runtime is what makes that
# tree readable without a second install step. The installer verifies its own
# signed release manifest before it writes anything (docs: "Binary integrity
# and code signing") — re-checking that here would just be a second, worse
# copy of it. DISABLE_AUTOUPDATER=1 because this image's version is the
# Dockerfile's ARG, not whatever claude.ai serves an hour after the build —
# bumping the pin and rebuilding is how this image's claude version changes.
ARG CLAUDE_VERSION=2.1.268
ENV DISABLE_AUTOUPDATER=1
USER agent
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
 && claude --version
USER root

# --- The environment doctor as the entrypoint --------------------------------
COPY deploy/agent/entrypoint.sh /usr/local/bin/agent-entrypoint
RUN chmod 0755 /usr/local/bin/agent-entrypoint

# No WORKDIR: docker-compose.agent.yml sets working_dir to ${REPO_PATH}, which
# is only known at container start (it is the host's own path to this clone —
# see that file's comment on why "the identical path" matters). Started as
# root; entrypoint.sh drops to `agent` after it has done the one thing that
# needs root.
ENTRYPOINT ["/usr/local/bin/agent-entrypoint"]
CMD ["claude"]
