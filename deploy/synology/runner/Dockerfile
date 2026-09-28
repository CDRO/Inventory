# The self-hosted GitHub Actions runner that deploys this project onto the
# Synology NAS. Contract: docs/specs/38-release-pipeline-and-nas-runner.md,
# "The runner: a container on the NAS" and its H16 acceptance criteria;
# registration and privilege are decision D4 of
# docs/plans/2026-09-harness-optimization.md, quoted verbatim in that spec.
#
# Read deploy/synology/runner/README.md before you run this on the NAS. The
# short version of why this image is small: the only job it ever runs is
#   sh deploy/synology/update --ref <tag> --auto --backup
# from the clone, so the only tools it needs are the three that script needs —
# git, the Docker CLI and a standalone `docker-compose` >= 2.24.
#
# GitHub's own runner image is the base rather than a hand-built one because the
# runner agent self-updates itself in place: pinning the agent is pinning a
# starting point, not a version, and everything beside the agent is what this
# file actually controls.

# Pinned by tag AND digest. The digest is the OCI *index* (verified with
# `docker buildx imagetools inspect`), so it resolves on linux/amd64 and
# linux/arm64 alike — the operator's NAS is a DS923+ (amd64), and arm64 is
# carried because a DSM box on arm is the one plausible move that would
# otherwise silently install an amd64 binary below.
FROM ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4

# Root for the whole image, not just for the build. Decision D4: DSM's
# /var/run/docker.sock is root-owned with no group to hand over, a `chgrp` is
# undone by the next DSM update, and a process that can talk to that socket is
# root on the host whatever its own uid is. Running as the image's `runner` user
# would therefore buy no privilege reduction and cost a mount permission
# problem. RUNNER_ALLOW_RUNASROOT is what makes config.sh and run.sh accept it.
# The residual risk is named in README.md and in spec 38's "Security posture".
USER root
ENV RUNNER_ALLOW_RUNASROOT=1

# The three tools deploy/synology/update requires. Two of them are already in
# the base image at versions the digest above pins (git 2.55.0, Docker CLI
# 29.7.2, curl 8.5.0), so they are asserted rather than installed a second time:
# a second Docker CLI earlier on PATH than /usr/bin/docker is version skew
# nobody would notice until a deploy behaved differently from the same commands
# typed by hand. The assertion is the part that matters — a future base image
# that drops one of them fails THIS build, in CI, instead of failing a deploy on
# the NAS.
RUN set -eu; \
    for t in git docker curl; do \
      command -v "$t" >/dev/null || { echo "FATAL: the base image no longer ships $t" >&2; exit 1; }; \
    done; \
    git --version; docker --version; curl --version | head -1

# The one tool the base image does not carry. Version and source are deliberate:
# v2.31.0 is the standalone binary docs/specs/01-architecture-and-deployment.md
# ("Compose version", line ~1075) tells the operator to install on the NAS by
# hand, and this runner executes the very same `deploy/synology/update` from the
# very same clone. Runner and operator running different Compose versions is how
# "it works when I do it" starts, so this tracks that spec rather than the
# newest release. It clears update's own floor either way: that script refuses
# anything below 2.24 (deploy/synology/update, "Preconditions").
#
# Installed at /usr/local/bin/docker-compose because that is where `update`
# looks for it: its DOCKER_COMPOSE defaults to the bare name `docker-compose` on
# PATH.
#
# The checksums are the ones GitHub publishes beside each asset
# (docker-compose-linux-<arch>.sha256); an artifact that changes under the tag
# fails the build.
ARG COMPOSE_VERSION=v2.31.0
ARG COMPOSE_SHA256_AMD64=8b5d2cb358427e654ada217cfdfedc00c4273f7a8ee07f27003a18d15461b6cd
ARG COMPOSE_SHA256_ARM64=a1f85584584d0c3c489f31f015c97eb543f1f0949fdc5ce3ded88c05a5188729
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) asset=docker-compose-linux-x86_64;  sha="$COMPOSE_SHA256_AMD64" ;; \
      arm64) asset=docker-compose-linux-aarch64; sha="$COMPOSE_SHA256_ARM64" ;; \
      *) echo "FATAL: no pinned docker-compose for TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/docker-compose \
      "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/${asset}"; \
    echo "${sha}  /usr/local/bin/docker-compose" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/docker-compose; \
    docker-compose version

# The start-up smoke test plus the register-once/run-forever logic. Kept as a
# script rather than an inline ENTRYPOINT so that its failure messages can say
# what to do about them, and so `docker-compose.runner.yml` needs no command.
COPY entrypoint.sh /usr/local/bin/runner-entrypoint
RUN chmod 0755 /usr/local/bin/runner-entrypoint

# Defaults for every knob docker-compose.runner.yml can override. RUNNER_NAME in
# particular must NOT be left empty: config.sh then falls back to the container
# hostname, which Docker regenerates on every recreate, and `--replace` (which
# replaces the registration *of the same name*) would silently register a second
# runner instead of reclaiming the first.
ENV RUNNER_URL=https://github.com/CDRO/Inventory \
    RUNNER_NAME=nas-inventory \
    RUNNER_LABELS=self-hosted,nas,synology \
    RUNNER_STATE_DIR=/runner/state \
    RUNNER_WORK_DIR=/home/runner/_work \
    INVENTORY_CLONE=/volume1/docker/inventory

WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/runner-entrypoint"]
