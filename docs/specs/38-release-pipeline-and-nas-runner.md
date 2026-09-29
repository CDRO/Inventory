# 38 — Release Pipeline and NAS Runner

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md)
("Deployment model", "Continuous integration", "Synology NAS variant"),
[`15-backup-restore-and-export.md`](15-backup-restore-and-export.md) (the
backup is the rollback),
[`18-operations-and-observability.md`](18-operations-and-observability.md)
("Version visibility", "Upgrades"), and
[`migrations/README.md`](../../migrations/README.md) (the marker lives with the
migrations).

## Why this spec exists

Deploying a release today is a person at a root shell on the NAS: open an SSH
session, take a backup by hand, read the pending migrations to decide whether
`--classic` is needed, run `sh deploy/synology/update`, watch it, and check
`/healthz` afterwards. Three of those steps are already **guarantees** written
down somewhere else — "back up first, the backup is the rollback"
(`18-operations-and-observability.md`, "Upgrades"), "the end-to-end suite must
pass before an image is promoted to production"
(`01-architecture-and-deployment.md`, "Deployment model"), and "a release whose
migration the previous release cannot run against needs `--classic`"
(`01-architecture-and-deployment.md`, "Synology NAS variant"). A guarantee that
is re-derived by a person per release, at whatever hour the release happens, is
not a guarantee; it is a habit.

This spec defines the pipeline that moves each of those decisions to the place
that can make it mechanically: **the tag is the release**, GitHub verifies that
the tag's commit is green, a self-hosted runner on the NAS runs the update
script that already exists, and the script decides classic-versus-rolling by
reading the pending migrations rather than by asking the operator to read them.
What it deliberately does not change is the update itself: the rolling swap, its
refusal paths and its guarantees are the ones
`01-architecture-and-deployment.md` already specifies, and this spec restates
them per phase rather than replacing them.

**Everything described here now exists.** `inventory migrate plan`, the `--ref`,
`--auto` and `--backup` flags of `deploy/synology/update`,
`deploy/synology/runner/`, `.github/workflows/release.yml` and
`scripts/dev release` were built by four packages that each own one layer.
Every criterion under "Acceptance criteria" names the package that
delivers it — **H14** (`migrate plan`), **H15** (the update script), **H16**
(the runner container), **H17** (`release.yml` and `scripts/dev release`) — so
a package can point at its own subset of this spec. An ordinary release on that
NAS is now `scripts/dev release <tag>` (`deploy/synology/README.md`, "A release,
start to finish"). The manual procedure in
`01-architecture-and-deployment.md` and `18-operations-and-observability.md`
stays the documented fallback: a NAS whose runner is down is deployed by hand,
with the same script, as is any deployment that is not that NAS.

Background — **not** contract — is the harness optimization plan (issue #293)
and the decisions it records as **D3** (classic-deploy signalling) and **D4**
(runner registration and privilege). Where this spec restates a value those
decisions fix — the marker line, the exit codes, the tag keyword, the
registration flags, the label set — it restates it exactly, because three
components have to agree on it. The plan itself decays as it executes and is
never something a review holds code to; this spec is.

## The release unit: an annotated tag

A release is an **annotated git tag** on a commit of `main`:

```
vYYYY.MM.DD        e.g. v2026.09.30
vYYYY.MM.DD.n      e.g. v2026.09.30.2 — the second release of the same day
```

Dates rather than semantic versions: there is no external API compatibility to
promise (the client contract in `12-client-api-contract.md` is versioned by its
own rules, not by the deployment), and the question actually asked of a NAS is
"how old is what is running", which a date answers directly.

- **Only that shape starts the pipeline.** `release.yml` triggers on
  `tags: ["v[0-9]*"]`, and its `gate` job refuses anything that is not
  `vYYYY.MM.DD[.n]` as its first action — before it resolves the tag, looks up a
  run, or reaches the NAS. `v-test`, `vendor-pin`, `v2` and `v1.4.0` all stop
  there. The glob and the gate are two halves of one guard because GitHub's tag
  filters are globs rather than regular expressions, so the glob alone cannot
  express the shape. The gate's check is written as the same `case` construction
  as `scripts/dev.d/release`'s, counter digit test included: one release shape
  must not mean two different things depending on which end enforced it.
  Before the pipeline could deploy at all, a stray `v*` tag was harmless — it
  refused at `phase=fetch`; it is not harmless now.
- **Annotated, not lightweight.** The tag object carries a message, and the
  message is the only place a forced-classic deploy can be declared (see
  "Classic or rolling"). A lightweight tag is still a valid release; it simply
  carries no message and therefore cannot force anything.
- **Only a green commit may be tagged.** The tag's commit must have a
  successful `test` run and a successful `e2e` run
  (`.github/workflows/test.yml`, `.github/workflows/e2e.yml`).
  `scripts/dev release <tag>` refuses to create the tag otherwise, and the
  pipeline's `gate` job checks it again on GitHub — a tag pushed by hand gets no
  benefit of the doubt.
- **`VERSION` is stamped from the tag.** `docker-compose.yml` already passes the
  build arg `VERSION: ${VERSION:-dev}` into the Dockerfile's
  `-ldflags "-X main.version=…"` (`18-operations-and-observability.md`,
  "Version visibility"), so a deploy exports `VERSION=<tag>` before it builds,
  and `GET /healthz` and the admin footer then report the tag. The deploy job
  asserts this afterwards: the `version` that `/healthz` reports must equal the
  tag that triggered the run, or the job fails even though the stack is up.

## Trigger and gate: `.github/workflows/release.yml`

One workflow, and it is the **only** workflow in the repository that names the
NAS runner. Two jobs decide and deploy; the two between them exist because a
reusable workflow is called from a `uses:` at **job** level and never from a
step, so "run `test` and `e2e` when this commit has no green run of them"
cannot live inside `gate` and has to be a conditional job of its own:

```yaml
on:
  push:
    tags: ['v[0-9]*']       # the gate refuses anything not vYYYY.MM.DD[.n]

jobs:
  gate:
    runs-on: ubuntu-latest          # GitHub-hosted, billed
  test:
    needs: gate
    if: needs.gate.outputs.need_test == 'true'
    uses: ./.github/workflows/test.yml
  e2e:
    needs: gate
    if: needs.gate.outputs.need_e2e == 'true'
    uses: ./.github/workflows/e2e.yml
  deploy:
    needs: [gate, test, e2e]        # `skipped` counts as satisfied
    runs-on: [self-hosted, nas]
    environment: production
    concurrency: { group: nas-deploy }
```

- **`gate`** resolves the tag to its commit SHA and looks for a successful run
  of `test` and of `e2e` **for that exact SHA** — by SHA, never "the newest run
  on the branch", the same trap `test.yml`'s own header documents for
  dispatch-and-poll. If either is missing, the gate runs it on the tag and waits
  for it; if either fails, the release stops here and nothing reaches the NAS.
  Because neither workflow is callable today, making them callable
  (`on: workflow_call`, keeping their existing triggers) is part of H17. A
  called workflow contributes its jobs to *this* run rather than creating a
  `test` or `e2e` run of its own, so the runs this lookup can find are the ones
  from the pull request or push that merged the commit — and a second tag on a
  commit whose first tag had to run them here runs them here again.
- **The gate reads the annotated tag through the API, not from a checkout.**
  `actions/checkout` fetches annotated tags peeled, which would lose the
  message the `deploy: classic` override lives in (see "The tag override"), and
  the runner image carries no `gh`. So the tag object is read in `gate` and the
  mode is handed to `deploy` as a job output.
- **`deploy`** does **no checkout**. It runs the clone's own script on the NAS,
  which is the only tree whose bind mounts resolve to the real data
  (`01-architecture-and-deployment.md`, "Synology NAS variant"); a checkout
  would produce a second tree that migrates the wrong database — the same
  failure `migrations/README.md` warns about for a missing `-f` layer.
- **`environment: production`** so every deploy leaves a deployment record, and
  the environment's own protection rules (a required reviewer, if one is ever
  wanted) apply without editing the workflow.
- **`concurrency: { group: nas-deploy }`** so two tags pushed minutes apart queue
  instead of interleaving. It is the *outer* guard only: a single runner takes
  one job at a time by construction, and `deploy/synology/update`'s own lock
  directory is the last line of defence, held even against a deploy run by hand
  at the same moment.
- **The gate's inputs are `test` and `e2e`, not the restore round trip.**
  `restore.yml` also triggers on a `v*` tag push (its own header, decision D8)
  and runs beside the release; the deploy job does not wait for it, because what
  it proves is that the *rollback* works, and the backup this deploy takes
  before migrating is that rollback. A red restore run on a release tag is a
  reason to stop releasing until it is green, not an interlock on this job.

## The runner: a container on the NAS

`deploy/synology/runner/` holds a Dockerfile, a `docker-compose.runner.yml` and
a README. The image is GitHub's `actions/runner` plus `git`, the Docker CLI and
a pinned standalone `docker-compose` ≥ 2.24 — the same three tools
`deploy/synology/update` already requires, and the same version floor it already
refuses to run below.

- **Mounts, and nothing else:** `/var/run/docker.sock`, and the clone at **the
  identical path it has on the host** (`/volume1/docker/inventory`). The
  identical path is load-bearing, not tidiness: `docker-compose.nas.yml`'s bind
  mounts are relative to the clone, and the paths in a container the runner
  starts through the socket are resolved by the **host's** daemon, so a clone
  mounted anywhere else would silently bind the wrong host directories.
- **Its own Compose project** (`inventory-runner`), so `dc down` on the app
  stack never stops the runner, and recreating the runner never disturbs the app
  stack.
- **One job at a time**, which a single runner process is by construction.
- **A smoke test at start**, before it takes work: `docker-compose config`
  against the app stack's two files, `git --version`, and a reachable socket. A
  runner that cannot do those three things fails loudly at start rather than
  accepting a deploy job and failing in the middle of it.
- **Built in CI only when its own files change** (a `paths` filter), so it costs
  nothing on an ordinary push.
- **An upgrade of the runner** is
  `docker-compose -f docker-compose.runner.yml build --pull` and a recreate,
  with the state directory kept — the registration survives, because it lives in
  the state directory and not in the image. GitHub's runner agent self-updates
  inside the container; rebuilding the image is how the *tools beside it* are
  updated.
- **`.env` is inside the clone**, so the runner can read every secret the
  deployment has. That is accepted rather than worked around: the runner holds
  the Docker socket, and anything that can talk to the socket can read any file
  on the host anyway (see "Security posture"). Mounting the clone read-only
  would not change that and would break `git fetch`.

### Registration and privilege (decision D4)

Verbatim, because the runner's compose file, the release workflow and the
operator runbook all have to agree:

- **Persistent registration**, not `--ephemeral`:
  `config.sh --replace --unattended --labels self-hosted,nas,synology`.
- **`.runner` and `.credentials` live in a bind-mounted state directory**, so a
  DSM reboot brings the runner back without anyone registering it again.
- **`restart: unless-stopped`.**
- **Registered once, by hand**, with a one-hour registration token minted from
  the operator's own machine — the token expires, so nothing long-lived is left
  on the NAS. `--ephemeral` was rejected precisely because it de-registers after
  one job and so needs a fresh token per job, and minting tokens on the NAS
  would mean storing a classic PAT with `repo` scope (or a GitHub App key) there
  permanently: a broader credential, kept longer, to defend against job-to-job
  residue on a runner that runs one job from one repository.
- **The container runs as root** (`RUNNER_ALLOW_RUNASROOT=1`) with the socket
  bind-mounted. Running as a non-root user with the socket's group is not a
  privilege reduction here: DSM's `/var/run/docker.sock` is root-owned with no
  group to hand over, a `chgrp` is undone by DSM updates, and any process that
  can talk to the socket is root on the host regardless of its own uid.
  `deploy/synology/README.md` already runs every one of these commands from
  `sudo -i`, and the clone stays root-owned, which is also what keeps `git fetch`
  free of `safe.directory` complaints.
- **One runner instance**, `concurrency: { group: nas-deploy }` in
  `release.yml`, and the update script's own lock as the last line.

## What the deploy job runs

```console
$ sh deploy/synology/update --ref <tag> --auto --backup
```

Run from the clone, by the runner, with nothing else in the job but reading the
script's exit code and its last line. Each flag is a contract, not a
convenience:

- **`--ref <tag|sha>`** replaces the default `git pull --ff-only`: fetch, verify
  the ref exists, check it out **detached**, and build with `VERSION=<ref>`. The
  existing refusal to deploy a clone with local changes to tracked files stays
  exactly as it is — what is deployed is what was fetched. A ref that does not
  exist, or a dirty clone, is a refusal **before** anything is built or
  migrated.
- **`--auto`** decides the mode instead of the operator: after the build, run
  `migrate plan` (below) and take rolling or classic from its exit code.
  `DEPLOY_MODE=classic` in the environment forces classic, which is how the
  tag-message override reaches the script from `release.yml`. Nothing forces
  *rolling*: see "Classic or rolling".
- **`--backup`** runs the `backup` service
  (`15-backup-restore-and-export.md`) **before `migrate up`**, into the
  `./backups` directory the NAS layer already mounts
  (`01-architecture-and-deployment.md`, "Synology NAS variant"). A failed or
  missing archive **aborts the update** before the schema is touched — an
  upgrade whose rollback was not taken is not an upgrade worth continuing.
  Afterwards it keeps the newest `BACKUP_KEEP` archives and deletes the older
  ones; `BACKUP_KEEP` is declared, commented, in `.env.example` and defaults to
  **5** when it is unset. This is step 1 of
  `18-operations-and-observability.md`, "Upgrades", automated — the step that
  used to be "still yours".
- **The summary line.** The run's last line is one line the runner can put
  straight into the job summary, and it names the four facts an operator asks
  for: the mode taken, the backup archive written, the version now served, and
  the phase reached. It is printed on failure too — a refusal says which phase
  it stopped in, which is what makes the table under "Failure and rollback" a
  lookup rather than a guess.
- **Exit codes.** `0` on success **and** on nothing-to-do (the `up --dry-run`
  check found nothing to recreate: a successful no-op release, not an error).
  Non-zero otherwise: `1` for every refusal and abort, which is what the
  script's `die` already does, and the signal exits (`130`, `143`, `129`) its
  traps already produce, unchanged. A non-zero `migrate plan` that is not `3`
  aborts the run before `migrate up`, reported as a refusal. The deploy job's
  success is this exit code and nothing else — never a grep of the log.

## Classic or rolling (decision D3)

The rolling update migrates while the **old** app is still serving, and keeps it
serving until the new instance is healthy
(`01-architecture-and-deployment.md`, "Synology NAS variant"). That is only safe
when the old binary can run against the migrated schema. Which releases those
are cannot be decided by a test — a migration test runs against the new binary
only, so an old-binary incompatibility passes green — and must not be decided at
03:00 on the NAS. So it is decided in review, by a human-readable line in the
migration, and read mechanically afterwards.

### The marker

The marker is the exact line

```sql
-- +inventory:classic
```

as the **first non-blank line after `-- +goose Up`**. The same line anywhere
else in the file is an **error**, not a marker: `migrate plan` is a strict
parser, so a misplaced marker is reported rather than guessed at. The
`+inventory:` prefix sits outside goose's own `-- +goose` directive namespace, so
the pinned goose (v3.22.1, `go.mod`) reads it as an ordinary SQL comment and can
never mistake it for a directive.

**"Exact" is about the characters of the line, not its terminator.** The
terminator is stripped before the comparison, so a CRLF checkout's trailing
carriage return is tolerated: nothing pins `migrations/**` to LF in
`.gitattributes`, and this repository is developed on Windows with
`core.autocrlf=true`, so a parser that read the terminator as part of the line
would reject every marker written here. Nothing else is tolerated — no leading
whitespace, no trailing space or tab, no trailing comment, no second statement
on the line.

### `inventory migrate plan`

`migrate plan` is the single place that reads the marker. Its first line is

```
migrate plan: rolling|classic|nothing pending (<n> pending: 00015_x.sql, …)
```

followed by one line per pending file. The parenthetical belongs to the two
modes that have pending files; with nothing pending the whole line is exactly

```
migrate plan: nothing pending
```

and no file lines follow it — a `(0 pending: )` rendering of that case is not
what this asks for.

Its **exit code** is the machine-readable answer, because the consumer is a `sh`
script under `set -eu` calling it through `docker-compose run`, which propagates
the container's exit code — parsing stdout would mean parsing past Compose's own
output:

| Exit | Meaning |
|---|---|
| `0` | rolling — pending migrations without a marker, **or** nothing pending |
| `3` | classic — at least one pending migration carries the marker |
| `1` | error, including a marker anywhere but the first non-blank line after `-- +goose Up` |
| `78` | configuration or database-schema error (`config.ExitConfig`, `internal/config/config.go`) |

`3` specifically, to stay clear of `1` (generic), `2` (shell misuse) and `78`
(already `EX_CONFIG` in this codebase). **Nothing pending is rolling**: there is
nothing for the old binary to break on.

### The tag override

An **annotated tag whose message contains the line**

```
deploy: classic
```

forces classic. `release.yml` reads it through
`gh api repos/<owner>/<repo>/git/tags/<sha>` rather than from a checkout,
because `actions/checkout` fetches annotated tags peeled and the message would
be gone, and passes it to the script as `DEPLOY_MODE=classic`. The keyword lives
in the message and not in the tag name, so forcing classic does not rename the
version. A lightweight tag has no message and simply carries no keyword.

**Nothing in a tag can force rolling over a marker.** The override is one-way:
it can only make a deploy more careful. An operator who believes a marker is
wrong edits the migration and cuts a new tag.

### When the marker is required

The marker is required when the **previous** release's binary cannot run
correctly against the schema the migration produces:

- a drop or rename of a column, table or enum value it reads or writes;
- a `NOT NULL` column without a default;
- a constraint or trigger its writes would violate;
- a backfill that must not race live writes.

Additive changes — a new table, a new nullable column, a new index, a new enum
value nothing yet reads — need **no** marker, and adding one buys an
interruption for nothing. The worked example of staying rolling on purpose is
`00014_job_lease.sql`, whose backfill grants a short grace claim to rows that
are already pending precisely so the update applying it cannot fail the old
instance's work.

### Who checks it

`review-go` checks **every hunk under `migrations/`** in a diff against the rule
above: a **missing** marker is a blocking finding, an **unneeded** marker is a
should-fix. This is a review duty rather than a test because no test can see it
— the suite only ever runs the new binary.

## Failure and rollback

The update script's existing per-phase guarantees are this pipeline's
guarantees, and a failed deploy leaves exactly what they say it leaves
(`01-architecture-and-deployment.md`, "Synology NAS variant", has the reasoning;
the script's own header is the source):

| Phase reached | What is serving | What the database is |
|---|---|---|
| refusal before the fetch: Compose < 2.24, the update lock already held, two app instances or a leftover stopped one, a dirty clone, an unknown ref | the old release, untouched | untouched |
| refusal after the fetch, before the build: the merged model lists `traefik`, or lacks `app`, `db` or `ts-inventory` | the old release, untouched | untouched — but the clone now stands at the new ref. This check is made on the files that were just fetched, which is why it cannot happen any earlier |
| the backup failed (`--backup`) | the old release, untouched | untouched — the run aborts before `migrate up` |
| fetch, image pull or build failed | the old release, untouched | untouched |
| `migrate up` failed | the old release | partially migrated: goose applies each pending migration in its own transaction and stops at the one that failed. Forward-only either way |
| a drain timed out, or the pending-job count could not be read | the old release | migrated. `DEPLOY_MODE=classic` — a re-tag with `deploy: classic`, or a deploy by hand — is the way through |
| the second instance never became healthy, or the job was cancelled | the old release; the new instance is removed again | migrated |
| between retiring the old instance and recreating the sidecar | the new release, but the tailnet URL is down until the sidecar is recreated — the script prints that command from its exit trap | migrated |

Two consequences for the pipeline:

- **A failed deploy job is not a rollback.** It is a stop, and in most rows
  above the previous release is still serving, which is the point of the rolling
  design.
- **Rollback is restoring the pre-upgrade backup**
  (`18-operations-and-observability.md`, "Upgrades";
  `15-backup-restore-and-export.md` has the procedure). Migrations are
  forward-only, and an old binary on a newer schema is a fatal start-up error by
  design, so there is no other way back across a migration. What changes here is
  only that the backup is now taken automatically by `--backup` instead of being
  remembered. Going back across a release that migrated **nothing** needs no
  restore at all: it is a deploy of the older tag,
  `sh deploy/synology/update --ref <older tag> --auto --backup`, and
  `git diff --stat <older tag> <tag> -- migrations/` printing nothing is how an
  operator knows that is the case.

## Security posture

The runner is the one component in this system that GitHub can cause to run code
on the NAS. While the repository is **public**, these controls are what stand
between a tag push and that:

- **The trigger is a tag push only.** A fork cannot push a tag to this
  repository, and a `pull_request` from a fork never runs this workflow.
- **`release.yml` checks `github.repository` and `github.actor`** before it runs
  anything, so a workflow file carried into a fork does not act on the NAS.
- **Actions is set to "allow selected actions"**, so only the pinned actions
  this repository names can run in it.
- **Fork-PR workflow runs require approval** — GitHub's own default for
  first-time contributors, kept on deliberately.
- **`release.yml` is the only workflow that names
  `runs-on: [self-hosted, nas]`**, and the runner mounts nothing beyond the
  clone and the socket.
- **`environment: production`** gives the deploy its own record and a place to
  add a required reviewer without editing the workflow.

**The residual risk, named:** the runner holds `/var/run/docker.sock`, and a
process that can talk to the Docker socket is **root on the host** — it can
start a privileged container, bind-mount `/`, and read or write anything on the
NAS, `.env` included. No uid, capability drop or read-only mount inside the
container changes that. It is accepted because the job's actual purpose is to
build images and recreate containers on that host, which *is* root-level work
(the manual procedure it replaces is run from `sudo -i`), and because the
controls above are what keep untrusted code from reaching the runner at all. The
mitigation is therefore about **reach**, not privilege: one workflow, one
trigger, one repository, no extra mounts.

After the repository goes **private**, the fork-related controls become moot and
one new gap opens: without runner groups (a paid feature), **any** workflow on
**any** branch in the repository can name `runs-on: self-hosted` and land on
this runner. No configuration prevents that on a Free plan, so the containment
is the three rules above, applied on purpose: the runner mounts nothing beyond
the clone and the socket, `release.yml` is the only workflow that names it, and a
new workflow that names it is a review finding. If the account ever becomes
paid, a runner group restricted to `release.yml` closes this properly.

### Going-private checklist

- **A read-only deploy key on the NAS.** Generate it on the NAS
  (`ssh-keygen -t ed25519 -N '' -f /root/.ssh/inventory_deploy`), add the public
  half to the repository as a deploy key with write access **off**, and point the
  clone's remote at the SSH URL so `git fetch` authenticates with it.
  `update --ref` fetches through the clone's own remote, so neither the script
  nor the workflow ever learns the key.
- **Actions permissions:** keep "allow selected actions"; workflow permissions
  read-only by default, with any write scope granted per job.
- **Secrets: none are needed.** The gate runs on the automatic `GITHUB_TOKEN`,
  and the runner authenticates with the credentials from its own registration. A
  release pipeline that needs no stored secret is the reason `--ephemeral` was
  rejected.
- **Runner group:** not available on a Free plan — see the gap above. If the plan
  changes, restrict a group to `release.yml`.
- **Notifications:** a failed `deploy` job has to reach the operator without
  anyone watching a browser tab. GitHub's workflow-failure notification to the
  repository owner is the mechanism; a release is not "done" until `/healthz`
  reports the tag, so silence is not success.

## Operator runbook pointers

`deploy/synology/README.md` is where an operator looks, and the implementation
packages add these sections to it. This spec deliberately does not duplicate the
commands: a runbook that disagrees with a spec is worse than either alone.

- **Registering the runner** (H16): minting the one-hour token, the state
  directory, the first `docker-compose -f docker-compose.runner.yml up -d`, and
  how to confirm the runner shows up idle in the repository's runner list.
- **Cutting the first release** (H17): `scripts/dev release <tag>`, what the gate
  does, where to watch the deploy job, and the `/healthz` check.
- **Forcing classic** (H17): writing `deploy: classic` into the tag message, and
  the marker as the ordinary case that needs no tag keyword.
- **When a deploy fails** (H15): the failure table above as a lookup — which
  phase the summary line names, what is serving, and the way through (re-tag with
  `deploy: classic`, restore the backup, or deploy the older tag).
- **Deploying by hand** when the runner is down (H15): the same script with the
  same flags, from `sudo -i` — which is also the procedure that existed before
  this pipeline.

## Acceptance criteria

Each criterion names the package that delivers it: **H14** `inventory migrate
plan`; **H15** `deploy/synology/update --ref --auto --backup`; **H16** the runner
container; **H17** `release.yml` and `scripts/dev release`.

### H14 — `inventory migrate plan`

- `migrate plan` prints
  `migrate plan: rolling|classic|nothing pending (<n> pending: …)` as its first
  line, then one line per pending migration.
- It exits `0` for pending-without-marker and for nothing-pending, `3` when a
  pending migration carries the marker, `1` on error, and `78` on a
  configuration or database-schema error.
- The marker is recognised **only** as the exact line `-- +inventory:classic` as
  the first non-blank line after `-- +goose Up`; the same line anywhere else in
  the file exits `1` and says where it was found.
- A marker in an **already applied** migration changes nothing — only pending
  files are read.
- Tests cover all of the above, including a marker that differs only in spacing
  or trailing text being rejected rather than accepted loosely — and a
  CRLF-terminated marker being **accepted**, since the terminator is not part of
  the line.
- `review-go`'s checklist gains the migration rule ("a migration the previous
  binary cannot serve carries the classic marker"), and `migrations/README.md`
  gains the example output.

### H15 — `deploy/synology/update --ref --auto --backup`

- `--ref <tag|sha>` fetches, verifies the ref, checks it out detached and builds
  with `VERSION=<ref>`, and refuses a clone with local changes to tracked files
  before anything is built or migrated.
- `--auto` takes the mode from `migrate plan`'s exit code; `DEPLOY_MODE=classic`
  forces classic; nothing forces rolling.
- `--backup` runs the `backup` service before `migrate up`, aborts the update
  when the service fails or writes no archive, and then keeps the newest
  `BACKUP_KEEP` archives (default `5`).
- The last line is the summary line and names mode, archive, version and phase —
  on success and on failure.
- Exit `0` on success and on nothing-to-do; `1` on every refusal and abort; the
  existing signal exits unchanged.
- Every new path has a case in `deploy/synology/update_test.go` beside the
  existing stub-driven tests — which for `VERSION=<ref>` means the compose stub
  has to record the environment it was called with, not only its arguments —
  and `deploy/synology/README.md` gains the scratch-stack entries and the
  runbook sections listed above.
- `.env.example` declares `BACKUP_KEEP` (commented, default `5`) — added by H13
  with this spec, so H15 only consumes it.

### H16 — the runner container

- `deploy/synology/runner/` holds a Dockerfile (`actions/runner` plus `git`, the
  Docker CLI and a pinned `docker-compose` ≥ 2.24), a `docker-compose.runner.yml`
  in its own project `inventory-runner`, and a README.
- Registration is persistent,
  `config.sh --replace --unattended --labels self-hosted,nas,synology`, with
  `.runner`/`.credentials` in a bind-mounted state directory and
  `restart: unless-stopped`; a DSM reboot brings the runner back with no token
  and no human.
- It mounts exactly `/var/run/docker.sock` and the clone at the same absolute
  path as on the host, and nothing else.
- It runs as root with `RUNNER_ALLOW_RUNASROOT=1`, and its README states the
  residual risk in the same terms as this spec: socket access is root on the
  host.
- A start-up smoke test fails the container loudly when `docker-compose config`,
  `git --version` or the socket is not usable.
- Its image is built in CI only when `deploy/synology/runner/**` changes.

### H17 — `release.yml` and `scripts/dev release`

- `release.yml` triggers on `push: tags: ['v[0-9]*']` only, and `gate` refuses
  any name that is not `vYYYY.MM.DD[.n]` before it resolves anything (see "The
  release unit"); `gate` runs on a hosted
  runner and resolves green `test` and `e2e` runs **by the tag's SHA**, running
  them on the tag when they are missing and failing the release when they are
  red.
- `deploy` runs with `runs-on: [self-hosted, nas]`, `environment: production`,
  `concurrency: { group: nas-deploy }`, `needs: [gate, test, e2e]` (a skipped
  reusable call counts as satisfied), no checkout, and calls
  `sh deploy/synology/update --ref <tag> --auto --backup` from the clone.
- The tag message's `deploy: classic` line is read through
  `gh api repos/.../git/tags/<sha>` and passed as `DEPLOY_MODE=classic`.
- After the deploy, the job asserts that `GET /healthz` reports the tag as its
  version, and puts the script's summary line into the job summary.
- `release.yml` checks `github.repository` and `github.actor`, and is the only
  workflow in the repository that names a self-hosted runner — greppable, and
  worth grepping in review.
- `scripts/dev release <tag>` creates the annotated tag only on a commit with
  green `test` and `e2e` runs, refuses a tag name that does not match
  `vYYYY.MM.DD[.n]`, and pushes it.
- The package ends with a **real** release performed with the operator: the
  runner registered, the deploy key installed, one rolling deploy and one forced
  classic deploy observed, and `/healthz` reporting the tag afterwards.

## Out of scope

- **Publishing images to a registry.** The NAS builds from the clone; a registry
  would add a credential and a second source of truth for what is deployed.
- **Zero downtime.** The sidecar shares the app's network namespace and has to be
  recreated with it (`01-architecture-and-deployment.md`, "Synology NAS
  variant"); the few seconds without the tailnet URL stay a documented gap.
- **Deploying anything but this NAS.** A second environment would need its own
  runner, its own environment and its own lock.
