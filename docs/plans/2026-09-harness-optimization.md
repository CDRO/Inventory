# Harness optimization — deliver more with the same resources

**Status:** proposed · **Central issue:** #293 · **Wave plan:** `scripts/wellen-harness.json` · **Author:** Claude (Fable 5.1), 2026-09-26, for Tizian's review

This plan covers the *harness* around the product: GitHub Actions, the
three-reviewer ship loop, the wave orchestrator, local test tooling, and the
path from a green `main` to the Synology NAS. The product code is not in
scope, except for one small backend addition the deployment pipeline needs
(`inventory migrate plan`).

It is written for a reviewer who has not been in the room. Every number in it
was measured on 2026-09-26 against `main` at `3409730`; the commands that
produced them are in the appendix so they can be re-run after the work lands.

---

## 0. Summary

Six goals were set:

| Goal | Where the cost is today | What this plan does about it |
|---|---|---|
| Reduce test times | Every run that touches `internal/store` pays ~20 s of database-bound tests; a fresh CI runner recompiles everything (zero cache) | Postgres speed flags on throwaway test databases; Go build cache and image layers persisted in CI; one container invocation instead of two |
| Reduce review time | 40 recent PRs needed 182 reviewer runs (4.6 per PR, minimum is 3); 37 BLOCK verdicts, many for mechanical findings (missing doc comments, stale text) | A deterministic pre-gate catches the mechanical findings before a reviewer sees the PR; a pre-built review packet replaces 6–10 context-gathering tool calls per reviewer; round 2 reviews the delta, not the whole PR; a machine-readable verdict marker makes the merge gate a script |
| CI ≤ 2000 min/month (repo will go private) | ~1 150 billable min/30 days at today's activity, before the 2-core private-runner penalty; E2E builds the production image twice per push to `main` | Cache, concurrency groups, path filters, one image build shared by both E2E jobs, restore round-trip on a cadence, projected ~520 min/30 days |
| Optimize agent time | 227 k tokens of cached context re-read on the *average* Sonnet call; reviewers are 36 % of all Sonnet calls; sessions wait on permission prompts | Compact tool output by default (`scripts/dev`), review packets, delta rounds, an expanded allowlist, notification hooks so a waiting session is noticed |
| Optimize limited agent resources (subscription caps, Tizian's attention) | No token accounting per PR; no signal when a session is stuck; advisor doubles every package session's cost | Token accounting script with a per-PR baseline; heartbeat and stuck-session alerts in the orchestrator; advisor policy by risk; a headless agent container with an environment doctor |
| Automated deployment to a NAS that cannot be reached but can listen | Deploys are an SSH session and `sh deploy/synology/update` by hand; classic-vs-rolling is a human judgement; no backup is taken automatically | A self-hosted Actions runner in a container on the NAS; release tags trigger a gated deploy job that runs the existing update script with `--auto --backup --ref`; classic mode is decided by a migration marker and can be forced from the tag |

The work is cut into **19 packages in 6 waves** (section 8), executed by the
existing wave orchestrator from `scripts/wellen-harness.json`. Waves 1–2 are
quick wins with measurable effect; wave 3 writes the spec the deployment work
implements; waves 4–5 build the deployment pipeline; wave 6 builds the agent
runtime container.

---

## 1. Baseline — where the resources go today

### 1.1 CI minutes

Source: `gh run list` over the 30 days to 2026-09-26. GitHub bills each *job*
rounded up to the next minute, so wall-clock and billable differ.

| Workflow | Runs / 30 d | Avg wall clock | Jobs | Billable est. / 30 d |
|---|---|---|---|---|
| `test` (push to main, PR to main, dispatch) | 310 | 1.4 min | 1 | ~620 min |
| `e2e` (push to main, dispatch) | 88 | 2.3 min (max of two parallel jobs) | 2 (`e2e` ~3.2 min, `restore-round-trip` ~1.6 min) | ~530 min |
| **Total** | | | | **~1 150 min / 30 d** |

*Measured after the fact* with `scripts/dev ci-usage --month 2026-09` (H1,
#321, merged 2026-09-27): September actually billed 626 min for `test`
(314 runs — within 1 % of the estimate) and 279 min for `e2e` (89 runs),
905 in total. The `e2e` estimate above is the *current* per-run cost
(≈ 6 billed minutes) extrapolated over a month; September billed less
because the round-trip job only exists since 09-25 (#133) and the `e2e` job
grew from ~1.9 to ~3.2 minutes as the suite reached 157 tests. The per-run
figure is the one to plan with, and `ci-usage` is how the month is tracked
from now on.

Activity is bursty: 85 runs on 2026-09-26 alone, 60 on 09-21. A wave day
can spend 100+ minutes. Nothing is cached: `actions/cache` usage is 0 bytes.
Inside the `test` job the `go vet` step takes 43 s (it builds the dev image
and compiles everything), `go test` 21 s. Inside `e2e`, "Build the images"
is 65 s in *each* of the two jobs, and the builder stage runs `go test ./...`
again (51 s) before it builds the binary.

Going private changes two things: minutes start counting against 2 000 per
month, and `ubuntu-latest` drops from 4 to 2 vCPU for private repositories,
so every job gets slower. Extrapolated naively, today's activity would land
between 1 500 and 1 800 billable minutes — inside the cap, with no headroom
for a busy month or a runaway job (`test` has `timeout-minutes: 15`, `e2e`
20).

### 1.2 Test time (local, Docker Desktop, 24 cores)

`docker compose run --rm app go test ./...` on `main`:

| Run | Wall clock | Notes |
|---|---|---|
| `-count=1` (nothing cached) | 30.8 s | `internal/store` 19.7 s, `internal/migrate` 7.8 s, `internal/httpapi` 2.3 s, `deploy/synology` 2.5 s, everything else < 2 s |
| second run, no changes | 27.4 s | 15 packages `(cached)`; `store` 19.9 s and `migrate` 8.6 s still run because `-count=1` on the previous run left no result to cache |
| third run, no changes | ~6 s | all `(cached)` — the number PR #278 reported |

The build cache from PR #278 removed the *compile* cost. What remains is the
database-bound work: `internal/store` creates one throwaway database and
runs 40 test files against it, `internal/migrate` creates databases
repeatedly. Any PR that touches `internal/store` — most of them — pays those
~28 s on every ship-loop iteration and on every reviewer's own run. Postgres
defaults (`fsync=on`, `synchronous_commit=on`, `full_page_writes=on`) are
tuned for durability that a throwaway database does not need.

The E2E suite: 157 Playwright tests, 34.7 s locally with 12 workers; 66 s
in CI with 2 workers, plus `npm install` (2 s in CI because the image already
has most packages) and the image build.

### 1.3 Review rounds

Source: reviewer comments on the last 40 merged PRs (#212–#279).

| Outcome | PRs | Share |
|---|---|---|
| All three APPROVE in round 1 | 21 | 52 % |
| Needed round 2 | 16 | 40 % |
| Needed round 3 (consolidation PRs with a raised cap) | 3 | 8 % |

182 reviewer runs for 40 PRs → **4.55 reviewer runs per PR**, against a floor
of 3. 37 BLOCK verdicts. Reading the blocking findings on those PRs, the
recurring mechanical classes are: exported identifier without a doc comment,
a doc that still describes the old behaviour, a test that asserts only the
happy path, and scope creep. The first two are catchable by a linter or a
grep before any reviewer runs.

Each reviewer today spends its first 4–8 turns on the same context gathering
(`gh pr view`, `gh issue view`, reading the spec — spec 01 alone is 1 056
lines — `gh pr diff`, then surrounding files), and in round 2 it does all of
it again for the whole PR.

### 1.4 Agent tokens

Source: session transcripts under `~/.claude/projects/C--Users-tizia-Projekte-Inventory*`
modified in the last 3 days (10 main sessions, 59 subagent transcripts),
deduplicated per message, counting only the final usage record of each call
(see `token-usage-accounting` for the three traps).

| Model | Calls | Cache read | Cache write | Output | of which subagents (reviewers) |
|---|---|---|---|---|---|
| Sonnet 5 | 4 575 | 1 037 M | 10.3 M | 3.10 M | 1 645 calls · 221 M cache read · 1.19 M output |
| Opus 5 | 1 130 | 310 M | 3.0 M | 1.17 M | — |

Two readings matter. **The average Sonnet call re-reads 227 k tokens of
cached context** — sessions run long and carry everything they ever read.
Cached input is cheap per token but it is what the 5-hour and weekly caps are
consumed by at volume, so the lever is *context per call*, not calls.
**Reviewers are 36 % of Sonnet calls and 38 % of Sonnet output**: three
adversarial reviewers per PR, 4.55 runs per PR, each rebuilding its context
from scratch.

### 1.5 Attention

Not measurable from transcripts, but the failure modes are known from this
project's own history: a session waiting on a permission prompt nobody sees;
a session stuck in a loop with no signal to the orchestrator until its issue
never closes; Claude started inside a container that could not reach a
Docker daemon and only found out at the first `docker compose run`; a
consolidation that needed manual conflict work. The orchestrator polls issue
state every 120 s and knows nothing else about a session.

### 1.6 Deployment today

`deploy/synology/update` is a careful rolling update (second app instance,
health check, job drain, sidecar recreation, lock, refusal paths, 21 shell
tests). It is run **by hand over SSH**, after a manual backup, and the
operator decides whether `--classic` is needed by reading the migrations.
There is no runner, no environment, no tag, no deploy key, no secret in the
repository (all checked 2026-09-26). The NAS reaches out (git pull, image
pulls, Tailscale) but is not reachable from GitHub.

---

## 2. Principles and non-goals

- **The no-host-toolchain rule stays absolute.** Every new tool runs inside
  the dev image or a pulled image. Linters are installed in the Dockerfile's
  `dev` stage, pinned by version.
- **Specs stay the contract.** Work that changes an operations guarantee
  (deployment, upgrades, CI semantics) is specified first (spec 38, spec 01
  and 18 amendments) and implemented second. This document is not a contract
  (see `docs/plans/README.md`).
- **Every PR still gets all three reviewers.** Models and efforts may be
  tuned; the test reviewer runs the suite locally and uses CI only when a
  local Docker daemon is genuinely unavailable — never to save a local run.
- **Measure, then change, then measure again.** Every package that claims a
  saving states the before/after number in its PR body using the commands in
  the appendix.
- **Shared files are edited by section.** Packages in one wave touch
  `CLAUDE.md`, `.claude/skills/ship/SKILL.md`, the reviewer prompts and spec
  01 only in the section they own, appending rather than reflowing, so
  parallel PRs merge without conflicts (convention in the wave file).
- **Non-goals:** publishing images to a registry (decided against); zero
  downtime on the NAS (the Tailscale sidecar swap keeps a few seconds of gap,
  as documented); replacing the PowerShell orchestrator (the headless loop is
  an additional entry point); changing the product.

---

## 3. Target architecture

### 3.1 The loop today and after

```mermaid
flowchart LR
  subgraph today [Today]
    direction TB
    A1[ship session<br/>edit + go test] --> B1[push, PR]
    B1 --> C1[3 reviewers<br/>each: 6-10 gh/git calls,<br/>read spec, run suite]
    C1 --> D1{3 APPROVE<br/>read from comments}
    D1 -- BLOCK --> E1[fix, push] --> C1
    D1 -- yes --> F1[merge]
    F1 --> G1[main: test + e2e<br/>2 image builds, cold cache]
    G1 --> H1[SSH to NAS<br/>backup by hand<br/>sh update / --classic?]
  end
  subgraph after [After]
    direction TB
    A2[ship session<br/>scripts/dev test + check] --> B2[push, PR]
    B2 --> P2[scripts/dev packet]
    P2 --> C2[3 reviewers<br/>read one packet,<br/>run suite locally]
    C2 --> D2{scripts/dev gate<br/>parses verdict markers}
    D2 -- BLOCK --> E2[fix, push] --> P2b[packet: delta only] --> C2
    D2 -- yes --> F2[merge]
    F2 --> G2[main: test cached ~1 min<br/>e2e one build, conditional restore]
    G2 --> T2[tag vYYYY.MM.DD]
    T2 --> R2[release.yml: gate by SHA]
    R2 --> N2[self-hosted runner on NAS<br/>update --ref --auto --backup]
  end
```

### 3.2 Deployment pipeline

```mermaid
sequenceDiagram
  autonumber
  participant Dev as Tizian / scripts/dev release
  participant GH as GitHub (hosted runner)
  participant RN as Runner container on NAS
  participant UP as deploy/synology/update
  participant ST as Inventory stack (app, db, sidecar)

  Dev->>GH: git push tag v2026.09.30 (annotated; may say "deploy: classic")
  GH->>GH: release.yml gate job: find successful test + e2e runs for this SHA
  alt no green run for this SHA
    GH->>GH: run test and e2e reusable workflows on the tag
  end
  GH-->>RN: job "deploy" queued for runs-on [self-hosted, nas] (runner long-polls; nothing reaches the NAS)
  RN->>UP: sh deploy/synology/update --ref v2026.09.30 --auto --backup
  UP->>ST: git fetch + checkout tag (clean-clone check), build image with VERSION=tag
  UP->>ST: docker-compose run backup (pre-upgrade archive, keep last N)
  UP->>ST: run --rm app migrate plan → rolling | classic (+ tag override)
  alt rolling
    UP->>ST: migrate up, 2nd instance, /healthz, drain, retire old, recreate sidecar
  else classic
    UP->>ST: stop app + sidecar, migrate up, up -d
  end
  UP-->>RN: exit code, summary (mode, backup archive, version served)
  RN-->>GH: job status, deployment record in environment "production"
  GH-->>Dev: notification on failure; /healthz reports the tag
```

The runner container (`deploy/synology/runner/`) is GitHub's
`actions/runner` image plus `git`, the Docker CLI and a pinned standalone
`docker-compose` ≥ 2.24 — the same tools the update script already expects.
It mounts `/var/run/docker.sock` and the clone at **the same path as on the
host** (`/volume1/docker/inventory`), so the relative bind mounts in
`docker-compose.nas.yml` resolve to the right host directories. It is a
separate Compose project (`inventory-runner`) so `dc down` on the app stack
never touches it. Until the repository is private, `release.yml` is the
*only* workflow allowed to target the runner, it triggers only on tag pushes
(which forks cannot make), and it checks `github.repository` and
`github.actor` before running anything.

### 3.3 Classic or rolling?

```mermaid
flowchart TD
  S[update --auto after build] --> P[run --rm app migrate plan]
  P --> Q{pending migrations<br/>carry the classic marker?}
  Q -- yes --> C[classic]
  Q -- no --> T{tag message says<br/>deploy: classic?}
  T -- yes --> C
  T -- no --> R[rolling]
  C --> C1[stop app + sidecar → migrate up → up -d]
  R --> R1[migrate up → second instance → health → drain → retire]
```

A migration carries the marker when the *previous* release's binary cannot
serve against the schema it produces: dropping or renaming a column or table
the previous binary reads, adding `NOT NULL` without a default, changing a
column type, or any rewrite the previous release would misread. The marker
is a comment line the review-go agent is told to look for on every migration
in a diff, so the decision is made in review, by a human-readable line, not
at 03:00 on the NAS. `inventory migrate plan` is the single place that reads
it: it lists pending migrations, prints `rolling` or `classic`, and its exit
code lets the update script branch without parsing text. Decision D3 fixed
the syntax spec 38 will carry: the marker is the exact line
`-- +inventory:classic` as the first non-blank line after `-- +goose Up`;
`migrate plan` exits 0 for rolling and nothing-pending, 3 for classic, 1 on
error (a marker anywhere else in the file is an error), 78 on config or
schema errors; an annotated tag whose message contains the line
`deploy: classic` forces classic, read through the GitHub API rather than a
checkout; nothing in a tag can force rolling over a marker.

### 3.4 Agent runtime container and doctor

```mermaid
flowchart LR
  subgraph host [PC now, NAS later]
    D[(Docker daemon)]
    V[(inventory-go-build-cache volume)]
    C[claude-agent container<br/>claude CLI + gh + git + docker CLI + compose]
    C -- docker.sock --> D
    C -- doctor at start --> R{all checks pass?}
    R -- no --> M[print what the administrator must fix<br/>and exit non-zero]
    R -- yes --> L[headless loop:<br/>next unblocked issue → claude -p /pickup …<br/>stop on usage limit, resume later]
    L --> W[worktree per issue,<br/>same ship loop, same reviewers]
  end
```

`scripts/doctor` is the same check on the host: daemon reachable, Compose
≥ 2.24, a container can actually start, the external build-cache volume
exists (or is created), ports free, `gh auth status` with the scopes the
loop needs, git identity and LF settings, `claude` on PATH and authenticated,
disk space. It prints one remediation line per failure — the message the
container-without-rights incident lacked — and the orchestrator runs it
before it opens a single window.

---

## 4. Workstreams

Each workstream lists its packages (H-numbers map to the issues and to
`scripts/wellen-harness.json`). Effort estimates are in reviewer-round terms
because that is what the budget is measured in.

### A — CI minutes and speed

**A1 · H2 `h2-ci-cache` — cache the test job, run it once, cancel stale runs.**
`test.yml`: create the external build-cache volume as a bind to a runner
directory saved by `actions/cache` (key: OS + `go.sum` hash + SHA, prefix
restore); build the dev image with BuildKit's GitHub Actions cache so the
`go mod download` layer is restored instead of rebuilt; one
`docker compose run --rm app sh -c 'go vet ./... && go test ./...'` instead of
two container starts; `concurrency: test-${{ github.ref }}` with
`cancel-in-progress` for pull requests; `timeout-minutes: 15 → 8`; a
`paths-ignore` for documentation-only changes handled per decision D7. Spec
01's "Continuous integration" section is updated to describe the cached job.
*Expected:* 1.4 min → ≤ 1 min wall clock on a 2-core runner, billed 1 min
instead of 2; fewer runs through cancellation. *Measure:* average job time
over the next 30 runs.

**A2 · H8 `h8-e2e-workflow` — one image build, cached node modules, restore
round-trip on a cadence.** `e2e.yml`: a `build` job builds the production
image once and hands it to both jobs (`docker save` artifact or BuildKit
cache — the package measures both and keeps the faster); `e2e/node_modules`
cached by `package-lock.json` hash; `restore-round-trip` moves into its own
`restore.yml` (decision D8: path-filtered to the files that can break it,
plus `v*` tags, a weekly schedule and dispatch — path filters are per
workflow, so a job-level check would still bill a minute); concurrency group
per ref; timeouts tightened. The same package wires H3's
`docker-compose.ci.yml` into the workflows through `COMPOSE_FILE` (decision
D5). *Expected:* ~6 billed min per push to `main` → ~3. *Measure:* billed
minutes per `e2e` run.

**A3 · H17 (part) — release gate reuses green runs.** `release.yml` looks up
successful `test` and `e2e` runs for the tag's commit before running anything;
a release from an already-green `main` costs no hosted minutes beyond the
lookup job. Self-hosted minutes are free of charge.

**Considered and not done: sharding the Playwright suite across runners.**
Tizian raised it as a trade of minutes for wall clock. The suite is 66 s of
a 3.2-minute job; the other 2.5 minutes are fixed per job (checkout, image,
migrate, stack, teardown) and every job is billed rounded up to a full
minute, so two shards would cost ~5 billed minutes to save ~30 s of wall
clock, and four shards ~8 minutes to save ~50 s. E2E is not on the PR path,
so nobody waits on its wall clock today; the one place it could matter — a
release tag whose commit has no green run yet — is covered by the gate
reusing `main`'s run. Revisit only if E2E ever becomes a per-PR gate.

**A4 · H1 (part) — `scripts/dev ci-usage`.** Prints billable minutes per
workflow for the current month and a projection against the 2 000 cap, from
the same API this baseline used, so the number is one command away instead
of a spreadsheet.

### B — Test time

**B1 · H3 `h3-test-db-speed` — Postgres flags on throwaway databases.**
`synchronous_commit=off`, `fsync=off`, `full_page_writes=off` on the `db`
service of both `docker-compose.e2e.yml` and `docker-compose.override.yml`
(decision D5: the measured cost is in the local loop, and the flags trade
only crash durability of a volume the spec already calls recreatable). A
new `docker-compose.ci.yml` adds a tmpfs data directory for runners that are
destroyed anyway, selected through `COMPOSE_FILE` so the documented command
stays byte-identical; the developer's volume keeps its data across `down`.
The package measures `internal/store` and `internal/migrate` before and after
and records both numbers in its PR. *Expected:* `store` 20 s → under 10 s,
`migrate` 8 s → under 4 s, cutting the "touched store" ship-loop iteration
from ~28 s to ~12 s.

**B2 · H1 `h1-dev-dispatcher` — `scripts/dev`, the one command agents run.**
A POSIX `sh` dispatcher (`scripts/dev <command>`) over `scripts/dev.d/<command>`
files, so later packages add commands without editing a shared script.
Wave 1 ships `test [pkg…]` (full log to `.claude/last-test.log`, only
FAIL/panic lines and the last three lines on stdout — the pattern the ship
skill already prescribes, now in one place), `vet`, `ci-status <sha>` (the
dispatch-and-poll recipe that is currently copied into three files), and
`ci-usage`. The ship and pickup skills point at it. *Expected:* fewer
tokens per test run (a green suite is three lines), fewer mistakes in the
poll recipe, one place to fix.

### C — Review time and rounds

**C1 · H6 `h6-pre-gate` — `scripts/dev check`, the deterministic gate.**
`gofmt -l`, `go vet`, `staticcheck`, `revive` with the exported-doc-comment
rule (the single most frequent docs-review BLOCK), a Go test that asserts
`.env.example` names every key `internal/config` reads and nothing else, and a
check that every `docs/specs/*.md` referenced from an issue-style pointer
exists. Tools are pinned in the Dockerfile `dev` stage. Runs in the ship
loop before the first push and as the first step of the `test` job.
*Expected:* the mechanical BLOCK classes disappear from round 1; the
round-1 approval rate rises from 52 % towards the 75 % target.

**C2 · H7 `h7-review-packet` — one file instead of ten calls.**
`scripts/dev packet <PR>` writes `.claude/review-packet.md`: PR title and
body, issue body, the spec sections the issue names (with their acceptance
criteria) rather than the whole spec, `git diff --stat`, the diff, the list
of exported identifiers added or changed and whether they carry a doc
comment, test files changed, and the current CI status by SHA. The three
reviewer prompts start with "read the packet"; their `maxTurns` drop to
20 (go), 25 (tests) and 15 (docs) per decision D2. *Expected:* 4–8 fewer turns per reviewer per round and a
smaller context per call; measured with `scripts/dev agent-usage` on the
reviewer transcripts before and after.

**C3 · H5 `h5-verdict-gate` — machine-readable verdicts and a scripted gate.**
Each reviewer appends an HTML comment `<!-- verdict: APPROVE|BLOCK round=N
sha=<head> reviewer=go|tests|docs -->` to its post. `scripts/dev gate <PR>`
reads the comments, finds the current round for the PR's head SHA and prints
`MERGE` or the missing/blocking reviewers. The ship skill's step 6 calls it
instead of asking the session to re-read every comment into context — which
also removes the "generous memory" failure the skill warns about.

**C4 · H12 `h12-reviewer-tuning` — models, efforts, delta rounds.** Applies
decision D2 (which reviewer runs on which model and effort) and D9 (round 2
verifies the round-1 findings and reviews the delta since the last reviewed
SHA). Validated by replaying the tuned reviewers on six past PRs — three that
were blocked in round 2, three approved in round 1 — and comparing verdicts;
the results table goes into this plan.

### D — Agent time, subscription caps, attention

**D1 · H4 `h4-attention` — allowlist and notifications.** Scan the recent
transcripts for the commands sessions actually run and add the read-only and
loop-standard ones to `.claude/settings.json` (`gh run *`, `gh workflow run`,
`gh api` reads, `gh pr checks`, `gh issue comment/edit`, `git fetch/worktree/
rev-parse/ls-remote/merge`, `docker compose logs/ps/exec/config`, `docker
volume create/ls/inspect`, `scripts/dev *`). Keep the deny list. Add a
`Notification` hook that raises a Windows toast when a session waits for
input, and a `Stop` hook that raises one when a session ends, so a blocked
session is seen within seconds instead of at the next glance at nine
windows.

**D2 · H9 `h9-agent-usage` — token accounting.** `scripts/dev agent-usage
[--since <date>] [--pr <n>]`: the transcript parser used for the baseline
(deduplicated, final-record-only, per model, main vs subagent), plus a per-PR
view by matching a session's worktree branch to its PR. The baseline in
section 1.4 becomes the first row of a table this command keeps up to date.

**D3 · H11 `h11-orchestrator-attention` — the orchestrator notices.** Runs
`scripts/doctor` in its pre-flight (replacing the inline `docker info` check);
tracks a heartbeat per package session (last commit on the package branch,
`.claude/worklog.md` mtime, last PR comment) and logs plus toasts a warning
when nothing moved for a configurable time; records how many review rounds
each package used. Tested in `scripts/tests/wellen-orchestrator.test.ps1`.

**D4 · advisor policy** — decision D1, applied in `scripts/wellen-harness.json`
and recorded in `scripts/wellen-planen.md`.

**D5 · H10 `h10-doctor`, H18 `h18-agent-image`, H19 `h19-agent-loop` — the
agent runtime.** Section 3.4. `scripts/doctor` first (wave 2, host and
container), then the image (`deploy/agent/Dockerfile`: `claude` native
install, `gh`, `git`, Docker CLI with the Compose plugin, non-root user with
the socket's group, auth via `CLAUDE_CODE_OAUTH_TOKEN` and `GH_TOKEN` or
mounted config directories, entrypoint runs the doctor), then the loop
(`scripts/agent-loop.sh`: next unblocked issue → one headless `claude -p`
session per issue with `--max-turns`, structured output logged, stop on a
usage-limit response and resume after the window). Decision D10 made the
loop a separate Linux-side entry point over the same wave JSON — no
`-Headless` switch on the PowerShell orchestrator, whose job is opening
visible windows on Windows — with the package prompt extracted into a
template both scripts fill, and fixed the invocation: `--permission-mode
dontAsk --permission-prompts none --max-turns 400 --output-format
stream-json`, non-root, never `--dangerously-skip-permissions`. PC first;
the image is `linux/amd64` so the DS923+ can run it later (slower tests, no
PC needed overnight).

### E — Deployment

**E1 · H13 `h13-spec-38` — the contract.** `docs/specs/38-release-pipeline-and-nas-runner.md`:
release tags (`vYYYY.MM.DD[.n]`, annotated; `VERSION` is stamped from the tag
so `/healthz` reports it); the gate ("a tag deploys only a commit with
successful `test` and `e2e` runs"); the runner container and its security
posture (public-repo phase and private phase); `update --ref`, `--auto`,
`--backup` with retention; the migration marker and `migrate plan` contract;
the tag override; what a failed deploy leaves behind (the update script's
existing guarantees, restated per phase); the going-private checklist (read-
only deploy key on the NAS for `git fetch`, Actions permissions, runner
group, secrets none); rollback (restore the pre-upgrade backup — spec 18's
rule, now taken automatically). Specs 00, 01 ("Deployment model", "Continuous
integration", "Synology NAS variant") and 18 ("Upgrades") are amended to point
at it, `CLAUDE.md`'s numbering line marks 38 as taken (decision D6), and
`migrations/README.md` gets the marker section.

**E2 · H14 `h14-migrate-plan` — the backend piece.** `inventory migrate plan`
in `cmd/inventory` and `internal/migrate`: lists pending migrations, reads
the marker, prints the mode, exits per D3. Tests cover: no pending, pending
without marker, pending with marker, marker in an already-applied migration
(ignored), malformed marker (refused). `review-go.md` gets one checklist line:
"a migration the previous binary cannot serve carries the classic marker".

**E3 · H15 `h15-update-auto` — the script.** `deploy/synology/update` gains
`--ref <tag|sha>` (fetch, verify the ref exists, detached checkout, same
clean-clone refusal, `VERSION` from the ref), `--auto` (run `migrate plan`
after the build and pick rolling or classic; `DEPLOY_MODE=classic` forces),
`--backup` (run the `backup` service before `migrate up`, abort the update if
it fails, keep the newest N archives), and a final summary line the runner
can put in its job summary. Every path gets a case in `update_test.go`
(21 tests today, all stub-driven) plus the scratch-stack table in
`deploy/synology/README.md`. `risk:high`: this script touches production
data.

**E4 · H16 `h16-runner-container` — the listener.** `deploy/synology/runner/
Dockerfile`, `docker-compose.runner.yml`, README: persistent registration
with a one-hour token minted from the PC, credentials in a bind-mounted state
directory, labels `self-hosted,nas,synology`, the container running as root
because DSM's socket is root-owned and any socket client is root on the host
anyway (decision D4, with the residual risk written into spec 38), one job at
a time, restart policy, exact mounts, and a smoke test (`docker-compose
config`, `git --version`, socket reachable) the image runs at start. Built in CI only
when its files change (path filter), so it costs nothing on ordinary pushes.

**E5 · H17 `h17-release-workflow` — the trigger.** `.github/workflows/
release.yml`: `on: push: tags: ['v*']`; job `gate` on a hosted runner
(lookup by SHA, else run the reusable `test`/`e2e` workflows); job `deploy`
with `runs-on: [self-hosted, nas]`, `environment: production`, `concurrency:
deploy-nas`, `needs: gate`, no checkout (it runs the clone's own script),
tag-message override → `DEPLOY_MODE`, post-deploy `/healthz` version
assertion, job summary. `scripts/dev release <tag>` creates the annotated
tag only from a SHA with green runs and pushes it. The package ends with a
first real release together with Tizian (runner registered, deploy key
installed, one rolling and one forced-classic deploy observed) — the only
step in this plan that needs a person at the NAS.

---

## 5. What changes for the people and agents in the loop

| Who | Before | After |
|---|---|---|
| Ship session | `docker compose run … go test ./... > log; grep …` by hand; reads every reviewer comment | `scripts/dev test`, `scripts/dev check` before push; `scripts/dev gate <PR>` decides |
| Reviewer agents | Gather context with 6–10 calls; re-review the whole PR in round 2 | Read `.claude/review-packet.md`; round 2 verifies own findings + delta; post a verdict marker |
| Orchestrator | Polls issue state; inline `docker info` check | Runs `scripts/doctor`; heartbeat per session; toast on stuck or waiting sessions |
| Tizian | SSH, backup by hand, decide classic, run update, watch | `scripts/dev release v2026.09.30`; a GitHub notification if it failed; `/healthz` shows the tag |
| A new machine or container | Discovers missing rights at the first `docker compose run` | `scripts/doctor` says what to fix before anything starts |

---

## 6. Projected effect

| Metric | Baseline | Target after waves 1–2 | Target after wave 5 |
|---|---|---|---|
| CI billable min / 30 d (today's activity, private 2-core) | ~1 150 (public 4-core) → est. 1 500–1 800 private | ≤ 700 | ≤ 550 incl. release gates |
| `test` job billed minutes | 2 | 1 | 1 |
| `e2e` billed minutes per push to `main` | 6 | 3 | 3 |
| Go suite, PR touching `internal/store` | ~28 s | ≤ 12 s | ≤ 12 s |
| Round-1 approval rate | 52 % | ≥ 70 % | ≥ 75 % |
| Reviewer runs per PR | 4.55 | ≤ 3.8 | ≤ 3.5 |
| Reviewer cache-read tokens per round | 221 M / 3 d (baseline) | −40 % | −50 % |
| Permission prompts per package session | unmeasured | ~0 for loop-standard commands | ~0 |
| Time from tag to NAS serving it | manual | — | < 15 min, no SSH |

Targets are commitments to *measure*, not promises; each package's PR
records its own before/after and the final section of this plan collects
them.

---

## 7. Risks and how they are contained

| Risk | Containment |
|---|---|
| A self-hosted runner on a still-public repository executes untrusted code | Only `release.yml` targets the runner; it triggers on tag pushes only (forks cannot push tags to this repository); it checks repository and actor; Actions setting "allow selected actions" and fork-PR approval are part of the going-private checklist; the runner is `linux/amd64` in a container with nothing but the clone and the socket — which is still root-equivalent on the NAS, so the socket mount is the accepted risk, stated in spec 38 |
| `fsync=off` corrupts a developer's staging database | Applied to CI and E2E unconditionally; the local dev override per D5 (`synchronous_commit=off` alone loses only the last transactions on a crash, never integrity) |
| Path filters skip a test run that mattered | Ignore list is short and explicit (decision D7: `docs/**`, `**.md`, `.claude/**`, `LICENSE` — nothing under `scripts/`); `deploy/**` and every Go, SQL, compose, Dockerfile and `web/**` change still runs; `cmd/inventory/compose_test.go` and `deploy/synology/update_test.go` read files that stay on the run list; no required status check exists, so a skipped run can never leave a PR pending |
| Delta review misses a regression the round-2 fix introduced elsewhere | Per D9: review-go re-reads the full diff when the delta touches routing, middleware or store transactions; the test reviewer always runs the whole suite |
| Cheaper reviewer models miss real findings | Replay on six past PRs before the tuning ships (H12); revert is a frontmatter edit |
| `migrate plan` says rolling but the old binary cannot serve | The marker rule is reviewed on every migration diff (review-go checklist); the tag override exists for the case a human knows better; the rolling update's own health check removes a new instance that fails, and the pre-upgrade backup is the rollback |
| Cache poisoning / stale build cache in CI | Keys include the `go.sum` hash and the SHA with prefix restore; a wrong cache costs a slower run, never a wrong verdict — Go validates cache entries by content hash |
| The orchestrator's heartbeat produces false "stuck" alerts during long test runs | Threshold configurable per plan; the alert is a toast, never an action |
| Two plans running at once | The harness plan starts only after the follow-ups plan (#176) has closed its last wave; the orchestrator's one-plan rule stands |

---

## 8. Execution — the wave plan

`scripts/wellen-harness.json` (plan issue #293). Standards: Sonnet 5 /
high for packages, Opus 5 / xhigh for consolidation; per package overrides
where the file is `risk:high`; advisor on for H4 (the allowlist that guards
headless sessions), H15, H16, H17, H18, H19 and every consolidation session,
off for the other thirteen packages (decision D1 — implemented as
`standards.advisor: true`, which is what the orchestrator's consolidation
sessions fall back to, plus an explicit `advisor` flag on every package);
round limit 2 per package, 4 per consolidation; `dockerCleanup: true` on
every wave.

```mermaid
flowchart LR
  subgraph W1 [Wave 1 · measure and stop the bleeding]
    H1[H1 dev dispatcher]
    H2[H2 CI cache]
    H3[H3 test DB speed]
    H4[H4 attention: allowlist + hooks]
  end
  subgraph W2 [Wave 2 · cheaper rounds]
    H5[H5 verdict gate]
    H6[H6 pre-gate check]
    H7[H7 review packet]
    H8[H8 e2e workflow]
    H9[H9 agent usage]
    H10[H10 doctor]
  end
  subgraph W3 [Wave 3 · tune and specify]
    H11[H11 orchestrator attention]
    H12[H12 reviewer tuning]
    H13[H13 spec 38]
  end
  subgraph W4 [Wave 4 · deploy pipeline I]
    H14[H14 migrate plan]
    H15[H15 update --auto/--backup/--ref]
    H16[H16 runner container]
  end
  subgraph W5 [Wave 5 · deploy pipeline II]
    H17[H17 release workflow + first release]
  end
  subgraph W6 [Wave 6 · agent runtime]
    H18[H18 agent image] --> H19[H19 agent loop]
  end
  W1 --> W2 --> W3 --> W4 --> W5 --> W6
  H1 -.-> H5
  H1 -.-> H6
  H1 -.-> H7
  H1 -.-> H9
  H1 -.-> H10
  H2 -.-> H6
  H2 -.-> H8
  H5 -.-> H7
  H10 -.-> H11
  H13 -.-> H14
  H13 -.-> H15
  H13 -.-> H16
  H7 -.-> H12
```

| Wave | Package | Slug | Files it owns | Model / effort | Risk |
|---|---|---|---|---|---|
| 1 | H1 dev dispatcher | `h1-dev-dispatcher` | `scripts/dev`, `scripts/dev.d/{test,vet,ci-status,ci-usage}`, ship skill §3 | Sonnet / high | — |
| 1 | H2 CI cache | `h2-ci-cache` | `.github/workflows/test.yml`, spec 01 "Continuous integration" | Sonnet / high | — |
| 1 | H3 test DB speed | `h3-test-db-speed` | `docker-compose.override.yml` (db), `docker-compose.e2e.yml` (db), `docker-compose.ci.yml` (new), `compose_test.go` assertions, spec 01 override section | Sonnet / high | — |
| 1 | H4 attention | `h4-attention` | `.claude/settings.json`, `scripts/hooks/*`, `scripts/wellen-planen.md` (hooks note) | Sonnet / high | — |
| 2 | H5 verdict gate | `h5-verdict-gate` | `scripts/dev.d/gate`, reviewer prompts (Output section), ship skill §6 | Sonnet / high | — |
| 2 | H6 pre-gate | `h6-pre-gate` | `scripts/dev.d/check`, Dockerfile dev stage, `test.yml` (one step), ship skill §3 (one line), `internal/config` parity test | Sonnet / high | — |
| 2 | H7 review packet | `h7-review-packet` | `scripts/dev.d/packet`, reviewer prompts (Task section), ship skill §5 | Sonnet / high | — |
| 2 | H8 e2e workflow | `h8-e2e-workflow` | `.github/workflows/e2e.yml`, `.github/workflows/restore.yml` (new), `test.yml` (`env:` block only), `docker-compose.e2e.yml` (e2e service), spec 01 E2E paragraph | Sonnet / high | — |
| 2 | H9 agent usage | `h9-agent-usage` | `scripts/dev.d/agent-usage`, this plan §1.4 | Sonnet / high | — |
| 2 | H10 doctor | `h10-doctor` | `scripts/doctor`, `scripts/dev.d/doctor`, README section | Sonnet / high | — |
| 3 | H11 orchestrator attention | `h11-orchestrator-attention` | `scripts/wellen-orchestrator.ps1`, its test, `scripts/wellen-planen.md` | Sonnet / high | — |
| 3 | H12 reviewer tuning | `h12-reviewer-tuning` | reviewer prompts (frontmatter + round rule), ship skill §7, this plan (replay table) | Opus / xhigh | judgement |
| 3 | H13 spec 38 | `h13-spec-38` | `docs/specs/38-*.md`, specs 00/01/18 pointers, `CLAUDE.md` numbering line, `migrations/README.md`, `.env.example` | Opus / xhigh | contract |
| 4 | H14 migrate plan | `h14-migrate-plan` | `cmd/inventory/main.go`, `internal/migrate/*`, `review-go.md` (one line) | Sonnet / high | — |
| 4 | H15 update --auto | `h15-update-auto` | `deploy/synology/update`, `update_test.go`, `deploy/synology/README.md` | Opus / xhigh | `risk:high` |
| 4 | H16 runner container | `h16-runner-container` | `deploy/synology/runner/*`, `test.yml` (path-filtered build job), `deploy/synology/README.md` (one row in its file table — H15 owns the rest of that file) | Opus / xhigh | `risk:high` |
| 5 | H17 release workflow | `h17-release-workflow` | `.github/workflows/release.yml`, `scripts/dev.d/release`, README runbook, spec 38 acceptance | Opus / xhigh | `risk:high` |
| 6 | H18 agent image | `h18-agent-image` | `deploy/agent/*` | Sonnet / high | — |
| 6 | H19 agent loop | `h19-agent-loop` | `scripts/agent-loop.sh`, `scripts/package-prompt.template`, orchestrator (`Get-PackagePrompt` reads the template, nothing else), `wellen-planen.md` | Sonnet / high | — |

No package adds a migration; no migration number is reserved. Wave 6 is
sequential (H19 builds on H18). Every other wave runs its packages in
parallel; the shared-file convention in section 2 is in the wave file's
`plan.conventions` so every package prompt carries it.

The plan can start once the follow-ups plan (#176) has closed its last wave
issue — the orchestrator runs one plan at a time. Start command:

```powershell
powershell -NoProfile -File scripts\wellen-orchestrator.ps1 -WaveFile scripts\wellen-harness.json
```

---

## 9. Decision record

Decisions that were not settled with Tizian directly were put to a Fable
advisor with the context above and are recorded here verbatim for review.
Overruling one is an edit to the corresponding issue before its wave starts.

The advisor (Claude Fable 5.1, run as a separate read-only session on
2026-09-27 with the baseline, the settled points and the ten questions as
its brief) flagged four claims it could not verify and that a reviewer
should check before the corresponding package starts: the exact environment
variable name for a `claude setup-token` token (`CLAUDE_CODE_OAUTH_TOKEN`),
the exact result subtype of a usage-limit stop in headless mode, whether
`--max-budget-usd` applies under subscription auth, and whether branch
rulesets are available on a Free-plan private repository (believed to need
Pro). Where a decision names a value the issues use (labels, concurrency
group, exit codes, the marker line), the issues carry that value verbatim.

### D1 — Advisor policy for the harness waves
**Decision:** (b) with an explicit list — advisor on (same model as the session) only for the packages tagged `risk:high`: wave 1 permission allowlist + hooks, wave 4 `update --auto --backup --ref` and the runner container, wave 5 `release.yml`, wave 6 agent container + headless loop, and every consolidation; off for everything else, set per package via the existing `advisor: false` field in `scripts/wellen-harness.json`.
**Rationale:** The advisor is a second model call on the same subscription, and the named constraints are the 5 h / weekly caps, so it has to be spent where a wrong line is expensive rather than everywhere. The expensive lines in this plan are shell that runs as root on the production NAS (`deploy/synology/update` already documents that it runs from `sudo -i`), the runner that holds the Docker socket, the release workflow that triggers it, and the allowlist that becomes the only guard once sessions run unattended in wave 6 — those are the packages that get it. Dev dispatcher, CI cache, Postgres flags, lint pre-gate, packet builder, token accounting, `scripts/doctor`, `migrate plan` and the heartbeat are testable Go, YAML or scripts on the owner's machine, and the three reviewers already read every PR. `scripts/wellen-planen.md` already ties stronger model/effort to `risk:high`, so this is the same rule applied to the advisor, not a new one. Wave 2's token accounting gives the advisor's real share; revisit after it reports.
**Reversal cost:** low — one boolean per package in the wave JSON, no code, and a running orchestrator picks it up on restart.

### D2 — Reviewer model/effort/maxTurns once the pre-gate and packet exist
**Decision:** review-go Sonnet/high maxTurns 20; review-tests Sonnet/high maxTurns 25; review-docs Haiku 4.5/high maxTurns 15 — each conditional on a replay of six past PRs, falling back one step (Haiku→Sonnet, high→xhigh) for any reviewer that fails it.
**Rationale:** The pre-gate takes the deterministic findings (gofmt, vet, staticcheck, missing exported doc comments — the `Nits` example in `review-go.md` and the first "must be documented" rule in `review-docs.md`) out of the reviewers' work, and the packet removes the four to six turns each currently spends on `gh pr view`, `gh issue view`, `gh pr diff` and spec reads, so lower effort and fewer turns are removing work that no longer exists rather than cutting judgment. review-go keeps Sonnet because its remaining job is the cross-file invariant list (404-not-403, `is_admin` re-query, paired `inventory_logs` rows), which needs reasoning over context, not pattern matching; review-tests keeps Sonnet because "would this test fail if the code were broken" is the same kind of judgment, and its turn budget stays highest because it runs the suite and may poll CI. review-docs is the candidate for Haiku because what remains after the lint is grep-and-compare work (stale statements, `.env.example` vs spec 01), and it is the reviewer whose findings are cheapest to miss and re-catch later. Validation: replay each reviewer under the new frontmatter against six closed PRs with `gh pr comment` swapped for a file write — PR #274 (round-1 BLOCKs from docs and tests, both fixed in commits 9605749 and 8c0df3d) plus, from the 40 measured PRs, the two most recent round-1 BLOCKs per reviewer whose finding was fixed rather than disputed, and three round-1 APPROVEs with no regression since; a configuration fails if it misses any of those accepted blocking findings, raises a new BLOCK on an APPROVE PR that the owner judges not real, or exits on maxTurns (a reviewer that posts nothing costs a whole round, because a missing comment is not an approval). I could not list the verdict history myself (no `gh` here), so the selection beyond #274 is a rule, not a list; if the CLI rejects `effort` for Haiku, the frontmatter simply omits it.
**Reversal cost:** low — three frontmatter lines per agent file; `ship/SKILL.md`'s "do not pass a model argument" rule already keeps the frontmatter authoritative.

### D3 — Classic-deploy signalling syntax
**Decision:** (i) the exact line `-- +inventory:classic` as the first non-blank line after `-- +goose Up`; (ii) `inventory migrate plan` prints one first line `migrate plan: rolling|classic|nothing pending (<n> pending: 00015_x.sql, …)` then one line per pending file, exits 0 for rolling and nothing-pending, 3 for classic, 1 on error, 78 (`config.ExitConfig`) on config/DB-schema errors as today; (iii) an annotated tag whose message contains the line `deploy: classic` forces classic, read by `release.yml` through `gh api .../git/tags/<sha>`; (iv) the marker is required when the previous release's binary cannot run correctly against the migrated schema — a drop/rename of a column, table or enum value it reads or writes, a NOT NULL column without default, a constraint or trigger its writes would violate, or a backfill that must not race live writes; additive changes need none; (v) yes, review-go checks every `migrations/**` hunk against rule (iv) — missing marker is blocking, unneeded marker is should-fix.
**Rationale:** A prefix outside the `-- +goose` namespace is a plain SQL comment to goose v3.22.1 (pinned per `internal/migrate/migrate_test.go:298`), so the marker can never be misread as a directive, and pinning its position lets `migrate plan` be a strict parser that rejects a marker anywhere else (exit 1) instead of guessing. Exit codes rather than stdout words because the consumer is `deploy/synology/update` under `set -eu` via `docker-compose run`, which propagates the container's exit code, while stdout would need parsing past Compose output; 3 avoids 1 (generic), 2 (shell misuse) and 78 (already `EX_CONFIG` in `internal/config/config.go:22`), and "nothing pending" is rolling because there is nothing for the old binary to break on. The tag keyword lives in the message, not the tag name, so a forced classic does not rename the version; reading it through the API rather than the runner's checkout avoids `actions/checkout` fetching annotated tags peeled (actions/checkout#290 — from memory, verify); a lightweight tag simply has no keyword and the plan decides, and nothing in a tag can force rolling over a marker. Rule (iv) is the contract the rolling update already states in spec 01 lines 1019–1020 ("the old app runs on the migrated schema until it is retired"), and migration 00014's grace-claim block is the worked example of staying rolling on purpose. review-go must check it because tests cannot: a migration test runs against the new binary only, so an old-binary incompatibility passes green.
**Reversal cost:** medium — the marker only matters in pending files (applied history is never re-read) and nothing persists in the database, but the exit-code contract is shared by `migrate plan`, `update` and `release.yml`, so a later change touches three places and spec 38.

### D4 — Runner registration and privilege
**Decision:** Persistent registration (`config.sh --replace --unattended --labels self-hosted,nas,synology`, `.runner`/`.credentials` in a bind-mounted state dir, `restart: unless-stopped`), registered once by hand with a one-hour registration token minted from the owner's machine; the container runs as root with `RUNNER_ALLOW_RUNASROOT=1` and the socket bind-mounted; one runner instance, `concurrency: {group: nas-deploy}` in `release.yml`, and the update script's own lock as the last line.
**Rationale:** `--ephemeral` de-registers after one job, so every job needs a fresh registration token, and minting one needs a classic PAT with `repo` scope or a GitHub App credential stored on the NAS (verified against the REST docs: `repo` scope, token expires after one hour) — a broader credential sitting permanently on the NAS to protect against a threat (job-to-job residue on a shared runner) that a single-repo, single-purpose runner does not have. The runner's own credentials can only take jobs for this repository, and the state dir plus `restart: unless-stopped` is what survives a DSM reboot without anyone touching it. Non-root with the socket's GID is not a privilege reduction here: DSM's `docker.sock` is root-owned with no group to hand over, a `chgrp` is undone by DSM updates, and any process that can talk to the socket is root on the host anyway, so root-in-container is honest about what the job already is (`deploy/synology/README.md` already runs everything from `sudo -i`, and the clone stays root-owned, which keeps `git pull` free of `safe.directory` complaints). One job at a time is inherent to a single runner process; the `concurrency` group makes two tags queue instead of interleave. The unfixable part must be written into spec 38: on a private repo without runner groups, any workflow on any branch can name `runs-on: self-hosted`, so the runner mounts nothing beyond the clone and the socket and `release.yml` is the only workflow that names it.
**Reversal cost:** medium — re-registering is minutes, but moving to ephemeral later adds a token-minting sidecar and a PAT, and moving to non-root means DSM socket-group work outside the repo.

### D5 — Postgres speed flags scope
**Decision:** `fsync=off`, `synchronous_commit=off`, `full_page_writes=off` on the `db` service in both `docker-compose.e2e.yml` and `docker-compose.override.yml` (via `command: postgres -c …`), plus a tmpfs data dir for CI only through a `docker-compose.ci.yml` selected with `COMPOSE_FILE` in the workflows; the dev override keeps its persistent volume.
**Rationale:** The measured cost is in the local loop, not CI: `internal/store` 19.7 s and `internal/migrate` 7.8 s are database-bound and run on every ship iteration and every reviewer run (the suite migrates throwaway databases on the override's `db`, per the comment in `docker-compose.override.yml:69–72`), so flags that stay out of the override miss most of the runs. The three flags trade durability across a host crash for write speed; on `docker compose down`, container restart or a Postgres process crash `synchronous_commit=off` loses at most the last commit window and `fsync=off` loses nothing, and the override's volume is by the spec's own framing dev/staging data that a backup (spec 15) or a re-seed recreates. tmpfs is the one setting that deletes data on every container stop, which is right for a runner that is destroyed anyway and wrong for the volume a developer keeps. A separate CI file selected through `COMPOSE_FILE` keeps the documented command `docker compose run --rm app go test ./...` byte-identical in `test.yml`, which is what `review-tests.md` and the workflow header promise. Production is untouched: `docker-compose.yml` and `docker-compose.nas.yml` pin the base file explicitly.
**Reversal cost:** low — one `command:` line per file; a developer who wants durability back deletes it, and the only data at risk was declared recreatable.

### D6 — Where the plan lives and whether to write spec 38
**Decision:** (a) — `docs/plans/2026-09-harness-optimization.md` (new folder with a README stating it is not a contract, like `docs/explanations/`), containing this decision record, plus a new `docs/specs/38-release-pipeline-and-nas-runner.md` as the contract for the deploy work, with specs 01 ("Synology NAS variant") and 18 ("Upgrades") amended to point at it and `CLAUDE.md`'s numbering line updated.
**Rationale:** The three reviewers review against `docs/specs/` and `review-docs.md` blocks on any doc that "now says something that is no longer true"; wave 4 and 5 change what spec 01 lines 995–1011 and spec 18 currently guarantee (a root shell on the NAS, backup-first, `--classic` as a manual choice), so without a spec the deploy PRs would be held to the text they are replacing. The marker syntax, `migrate plan` exit codes, the tag keyword, the runner's trust boundary and the rollback rule are operations guarantees an operator will look up, which is exactly what a numbered spec is for, and 38 is the first free number per `CLAUDE.md`. The plan itself — waves, budgets, the 1150-minute baseline, these ten decisions — decays as it executes and must not become something reviewers hold code to, so it stays outside the contract. `docs/explanations/` is product narrative and `scripts/` is tooling how-to; neither is the place for a dated plan with measurements. Amending only 01 and 18 (b) would spread one pipeline across two specs that exist for other things, and the cross-references are cheaper than the split.
**Reversal cost:** medium — a spec, once accepted, is what every later PR is reviewed against; retiring it means re-amending 01 and 18 and touching the CLAUDE.md numbering.

### D7 — Required-check strategy after going private with path filters
**Decision:** (a) — no required status checks; `paths-ignore: [docs/**, '**.md', .claude/**, LICENSE]` on `test.yml`; the ship gate treats "no checks reported" as satisfied only when `git diff --name-only <base>...HEAD` matches the ignore list entirely and the test reviewer's `**Suite:**` line shows a local run, and as a failure that dispatches a run otherwise.
**Rationale:** There is no branch protection today and none is available on a Free-plan private repository (branch rules and rulesets on private repos need Pro — from memory, verify), so the loop's gate in `ship/SKILL.md` step 6 is the merge gate already; adding a required check would only add the pending-forever failure mode on docs-only PRs. The real test signal is the reviewer's local suite (`review-tests.md` step 4), and CI on PRs to `main` is the backstop for sessions without a daemon, so skipping it for a docs-only diff loses nothing that the gate checks. Options (b) and (c) buy a green tick nobody requires at about a billed minute per PR against a 2000-minute cap. The ignore list stays narrow on purpose: `scripts/backup`, the compose files and `deploy/**` are read by tests (`docker-compose.override.yml:74–95`) and `.github/workflows/**` must trigger itself. If the account is ever Pro, add a ruleset requiring a PR and blocking force-push on `main` — still without a required status check.
**Reversal cost:** low — a `paths-ignore` block and one paragraph in the ship skill; nothing is configured on GitHub that would have to be undone.

### D8 — `restore-round-trip` cadence
**Decision:** (a) plus a weekly schedule — move the job into its own `.github/workflows/restore.yml` triggered on `push` to `main` filtered to `scripts/backup`, `docker-compose*.yml`, `migrations/**`, `e2e/restore/**`, `e2e/fixtures/**`, `Dockerfile`; on `v*` tags; `schedule` weekly; and `workflow_dispatch`.
**Rationale:** The round trip proves the backup script, the compose volume layout, the migration-after-restore step and the fixtures, and only changes to those files can break it, so the path filter is the exact set of causes; at ~1.6 min it drops from ~140 to a few minutes a month on a cap of 2000. Path filters are per workflow (`on.push.paths`), not per job, so the job has to leave `e2e.yml` — a job-level changed-files check would still boot a runner and bill a minute. Tags keep it as the deployment gate spec 18 calls "the only rollback", running on GitHub-hosted runners before the NAS deploy job. The weekly run costs ~7 min a month and catches what no path filter can see: drift in the `postgres` and Playwright images that the compose files reference by tag rather than digest. The `e2e.yml` header, which currently explains why the job lives there, must be updated in the same PR or review-docs will block.
**Reversal cost:** low — trigger blocks in one workflow file; the job's steps do not change.

### D9 — Delta review for round 2
**Decision:** Amend and accept — round 2 reviews `git diff <round-1 sha>..HEAD` with each reviewer marking every round-1 blocking finding resolved/open with `file:line`, subject to: review-tests always runs the whole suite; review-go re-reads the full PR diff when the delta touches `internal/httpapi/router.go`, any middleware, `internal/store` transaction code or `migrations/`, or when the packet builder finds a merge commit in `<round-1 sha>..HEAD`; review-docs keeps its global stale-doc grep; the full round-2 diff is referenced in the packet by path, not inlined.
**Rationale:** 48 % of PRs go to round 2 and each round-2 run today re-reads the whole diff plus spec, so the delta is where the reviewer cost per PR actually is. A fix that regresses elsewhere is caught first by the whole suite — that is the one whole-tree check in the loop and it is cheap and deterministic, so review-tests never narrows it. The files where a regression would be silent rather than red are the ones `CLAUDE.md` lists as failing silently (routing, handlers, store transactions) and migrations (D3), so those force a full re-read; everything else is covered by review-go's own rule 4 of reading the surrounding context of a hunk. A merge from the base branch between rounds turns a two-dot delta into upstream noise, so the packet builder detects it and falls back rather than trusting the range. Requiring per-finding resolved/open status is what makes a delta review auditable from the PR comment alone, which is what the gate reads.
**Reversal cost:** low — packet builder logic and a paragraph in each agent prompt; round 1 is unchanged.

### D10 — Agent runtime container scope
**Decision:** (i) three issues, with `scripts/doctor` (host + `--container` mode) landing in wave 2 as planned and wave 6 holding two PRs, `deploy/agent/Dockerfile` and `scripts/agent-loop.sh`; (ii) `claude -p "<package prompt>" --model … --effort … [--advisor …] --permission-mode dontAsk --permission-prompts none --max-turns 400 --output-format stream-json --verbose` as a non-root user with the project allowlist extended for edits (`Edit`, `Write`) and everything the ship loop runs, not `--dangerously-skip-permissions`; one session per issue, done = the issue closed on GitHub (same signal the orchestrator uses), the JSONL log kept per session, `session_id` read from the final `result` line, a stop with `is_error` after `system/api_retry` events carrying `error: rate_limit` (or a result text matching usage-limit wording) treated as paused and resumed with `claude -p "continue" --resume <session_id>` after the reset time or a capped back-off, `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` set; auth via `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` passed as a secret file, never baked into the image; not `--bare`; (iii) a separate Linux entry point over the same wave JSON, no `-Headless` switch, with the package-prompt template extracted to a file both scripts fill.
**Rationale:** Verified against the CLI reference and headless docs: `--permission-mode` accepts `dontAsk`, which denies rather than prompts (the right failure for an unattended run — it stops, it does not hang), `--permission-prompts none` (v2.1.259+) makes the denial final, `--max-turns` and `--output-format stream-json` are print-mode flags, the `result` line carries `session_id` and `--resume <id>` continues it from any directory, `system/api_retry` events carry an `error` category including `rate_limit`, `claude setup-token` exists for CI tokens, and `--dangerously-skip-permissions` is refused as root on Linux with the exception only for "a recognized sandbox" — which is why non-root plus the allowlist is the shape, and why wave 1's allowlist package must be written for this use. `--bare` is documented as the scripted-mode recommendation but skips CLAUDE.md, skills, agents and OAuth login, all of which the ship loop needs, so it is out. Three issues because the image is verifiable alone (`claude --version`, `claude auth status`) and the loop alone (against a stub `claude`), and doctor is already a wave 2 package. A separate Linux entry point because the PowerShell orchestrator's job is opening visible windows on the owner's Windows machine, and teaching it to `docker run` Linux sessions would mean re-implementing its restart-safety and Docker-cleanup rules for containers; sharing the wave JSON and a prompt template file keeps one plan and one prompt. Not verified: the exact env var name `CLAUDE_CODE_OAUTH_TOKEN` (absent from the truncated env-vars page I fetched), the exact result subtype or text of a usage-limit stop, whether `--max-budget-usd` applies under subscription auth, and what "recognized sandbox" means — so `--max-turns` is a runaway guard and the two-round cap stays the real budget.
**Reversal cost:** medium — flags and the resume logic are lines in one script, but the entry-point choice means two implementations of prompt building and polling, and folding the loop into PowerShell later would be a rewrite of that logic rather than an edit.

---

## 10. Settled with Tizian (2026-09-26)

- The repository will go private; 2 000 Actions minutes per month is a hard
  cap to design for.
- Deployment: a self-hosted GitHub Actions runner, in a container on the
  DS923+. No image is published to a registry; the runner runs
  `deploy/synology/update`, which builds on the NAS as today.
- Trigger: an explicit release tag. Every green `main` stays deployable but
  is not auto-deployed.
- Classic-vs-rolling: decided by a marker in the migration file; a keyword
  in the release tag can force classic.
- Reviewers: all three stay on every PR; models and efforts may be tuned;
  the test reviewer runs the suite locally and falls back to CI only when
  local Docker is impossible.
- An environment doctor and a `claude-cli` container that self-tests and can
  run Claude in a loop: PC first, NAS later.
- Subissues are executed as a wave plan for the orchestrator.
- This plan is presented as a draft PR, gets the three adversarial reviewers,
  and is mirrored as a Claude artifact.

---

## 11. How to review this plan

1. Check the baseline (section 1) against the appendix commands; if a
   number looks wrong, the plan's targets are wrong with it.
2. Read the decision record (section 9) — each decision names its reversal
   cost; overrule by editing the issue, not this document.
3. Check the wave table (section 8) for file collisions you know about that
   the collision rules missed.
4. Check the deployment sequence (section 3.2) against how you actually
   operate the NAS: paths, the compose prefix, Task Scheduler, the sidecar.
5. Anything you want added is a new sub-issue under #293; anything
   you want dropped is a close with a comment — the wave file is regenerated
   from the issue list.

---

## Appendix A — Measurement commands

```bash
# CI runs, wall clock per workflow, last 30 days
gh run list --limit 500 --created ">=$(date -d '30 days ago' +%F)" --json name,createdAt,updatedAt \
  -q '[.[] | {name, min: (((.updatedAt|fromdateiso8601)-(.createdAt|fromdateiso8601))/60)}]
      | group_by(.name) | .[] | "\(.[0].name): runs=\(length) total_min=\(map(.min)|add|floor)"'

# Step timing of one run
gh api repos/CDRO/Inventory/actions/runs/<run-id>/jobs \
  --jq '.jobs[] | "\(.name) \(.started_at) -> \(.completed_at)", (.steps[] | "  \(.name): \(.started_at) -> \(.completed_at)")'

# Go suite, per package, uncached then cached
docker compose run --rm app go test -count=1 ./...
docker compose run --rm app go test ./...

# Review rounds on the last 40 merged PRs (see scripts/dev gate once H5 lands)
for pr in $(gh pr list --state merged --limit 40 --json number -q '.[].number'); do
  gh pr view "$pr" --json comments -q '.comments[].body' | awk -v pr="$pr" '
    /^## (Go|Test|Docs) Review/ {c++; if ($0 ~ /VERDICT: BLOCK/) b++}
    /\*\*Round:\*\* *[0-9]+/ {match($0,/\*\*Round:\*\* *[0-9]+/); n=substr($0,RSTART,RLENGTH); gsub(/[^0-9]/,"",n); if (n+0>r) r=n+0}
    END {printf "PR %s verdicts=%d blocks=%d rounds=%d\n", pr, c, b, r}'
done

# Token usage, last 3 days (until scripts/dev agent-usage exists, H9):
# deduplicate by message uuid, count only usage records carrying output_tokens,
# take the last usage object on a line — see the token-usage-accounting notes.
```

## Appendix B — Reference facts checked on 2026-09-26

- Repository: public; no branch protection, no rulesets; Actions
  `allowed_actions: all`; default workflow permissions `read`; no runners,
  environments, tags, deploy keys or secrets.
- Hosted runners: `ubuntu-latest` is 4 vCPU for public repositories and
  2 vCPU for private ones; each job is billed rounded up to the minute;
  self-hosted runner minutes are not billed.
- NAS: DS923+ (x86_64), Container Manager, standalone `docker-compose` ≥ 2.24,
  clone at `/volume1/docker/inventory`, `docker-compose -p inventory -f
  docker-compose.yml -f docker-compose.nas.yml` on every call, Tailscale
  sidecar shares the app's network namespace.
- Local: Docker Desktop, 24 CPU, 32 GB; the external volume
  `inventory-go-build-cache` exists.
