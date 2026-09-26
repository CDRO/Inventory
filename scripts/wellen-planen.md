# Planning waves for the wave orchestrator

**Audience of this document:** a Claude Code session (typically Sonnet,
effort high) tasked with planning a new wave, or a whole new wave plan, into
`scripts/wellen.json`. The prompt at the end is what starts such a session.

Ported from `roadmapforlpm`'s `scripts/wellen-orchestrator.ps1` /
`wellen-planen.md` for this repo. The mechanics are identical; the language
is English to match this repo's specs and issues, the JSON field names are
English to match, and the review loop names **three** reviewers
(`review-go`, `review-tests`, `review-docs`), not two — see "Model, effort,
and reviewers" below.

## How the pieces fit together

`scripts/wellen-orchestrator.ps1` reads a wave plan from a JSON file
(default: `scripts/wellen.json`) and works through it wave by wave:

> **There is more than one plan file now.** `scripts/wellen.json` is the
> Extended-core plan (#97, complete — kept as its record, not extended);
> `scripts/wellen-followups.json` is the deferred-follow-ups plan (#176). A
> finished plan's file stays where it is rather than being emptied, so **every
> command that names a wave or a slug needs `-WaveFile` unless it means
> `wellen.json`** — that default is silent, and a bare `-Wave 2` against the
> wrong plan asks about a different wave entirely. Plans do not run
> concurrently: one orchestrator, one plan, or two runs fight over the same
> main checkout and the same Docker daemon.

1. Per wave, it waits until the integration branch exists on origin.
2. Per package, it creates a worktree and starts a visible, interactive
   Claude session with prompt, model, effort, and advisor as CLI arguments
   (`claude --model … --effort … --advisor … --remote-control …`).
3. It polls GitHub until a package's spec issue closes, tears down that
   package's own Docker stack (`docker compose down -v --remove-orphans` in
   its worktree — see "Docker cleanup after a wave") and does the same for
   whichever package closes next — in a sequential wave that is file order;
   in an unsequential one, whichever package's issue actually closes first,
   not necessarily the one listed first. Once every package of the wave is
   closed it starts the consolidation session in the main checkout and waits
   until the wave issue is closed.
4. For a wave with `"dockerCleanup": true` it then removes what is left of
   the Docker resources the wave's package worktrees created — built images,
   and anything from a package whose session crashed before its issue ever
   closed (see "Docker cleanup after a wave"). Only then does the next wave
   begin.

The script itself never writes to git/GitHub (other than `worktree add` and
copying `.env`) — every substantive action is done by the sessions it
starts. The one other thing it does is that Docker cleanup, and that is
deliberate: removing Docker resources is mechanical and must be exact, and a
session that is told to tidy up Docker is a session that might run a prune.
**Planning a new wave therefore means: extend the JSON file and set up the
GitHub prerequisites. The script itself is never changed.**

Docker isolation between parallel package worktrees (`COMPOSE_PROJECT_NAME`,
`HTTP_PORT`, `TRAEFIK_PORT`) is automatic and needs no attention when
planning a wave — the script assigns each package a deterministic, unique
set of these from its position in the wave file the moment a worktree is
created, sanitizing the project name it derives so it is always a value
Compose actually accepts (`Get-SanitizedProjectName`, #115). See
`docs/specs/01-architecture-and-deployment.md`, "Running more than one
instance of the stack locally", for the mechanism itself.

## Docker cleanup after a wave

Every package worktree leaves Docker state behind: a database container, a
network, three named volumes (`pgdata`, `uploads`, `imagecache`) and the
images built for it, and package sessions sometimes start a further Compose
project under a name of their own to run a check. Left alone, this grows by
several volumes and images per package, per wave. Two layers remove it, at
different times. The per-package layer below is fully automatic and needs no
attention when planning a wave — it takes no field. The per-wave layer is
the one thing here a wave file DOES set (`"dockerCleanup"`, below).

**Per package**, the moment its issue closes, `Stop-PackageStack` runs
`docker compose down -v --remove-orphans` inside that worktree — no waiting
for the wave to finish. This is what actually stops the containers and
frees the host ports; it is unconditional (it does not depend on
`"dockerCleanup"`) because a finished package's own, uniquely-named project
is always safe to tear down on its own. It runs a bare `docker compose down`,
no `-f`, so it only ever selects the default files — never
`docker-compose.e2e.yml`, which Compose loads only via an explicit `-f` or
`COMPOSE_FILE`. The E2E stack stays out of its reach the other way round too:
it is **one project per machine**, `inventory-e2e`, brought up with
`-p inventory-e2e` — which beats the worktree's own `COMPOSE_PROJECT_NAME`, so
it is not in the project this teardown loads and its `--remove-orphans` cannot
sweep it. That is settled and measured now (#190, split out of #140 item 2);
`docker-compose.e2e.yml`'s own `name:` comment carries the measurements. What
planning a wave does have to respect is the other half of that answer: one
machine has one E2E stack, so **two packages may never run the E2E suite at
the same time.** In a `"sequential": true` wave that is automatic. In a wave
with `"sequential": false`, nothing enforces it — the second `up` does not
fail, it silently recreates the first's containers — so do not put two
packages that each need a full E2E run in the same unsequential wave.

**Per wave**, a wave with `"dockerCleanup": true` additionally gets what the
per-package step cannot reach removed **after its consolidation has
finished** (the wave issue is closed, so no session is using any of it), by
`scripts/wellen-docker-cleanup.ps1`: built images; a package's OWN further
Compose project under a name of its own (`Stop-PackageStack` only ever runs a
bare `docker compose down`, which touches just the worktree's default
project — a project a session started under a different name for some check
of its own is invisible to it); and any resource left by a package whose
session crashed before its issue ever closed. It must be `true` or `false`;
anything else fails `-Validate`. Which waves carry it is per plan:
`wellen.json` sets it on waves 3 to 7, `wellen-followups.json` on waves 2 to
8 — wave 1 there is deliberately `false`, because the package that *rewrites
the cleanup script* runs in it and the script is re-read at every call.

- **What is removed:** the containers (with their anonymous volumes), networks,
  named volumes and image tags of every Compose project that belongs to the
  wave's worktrees. Ownership comes from Docker's labels, not from names: a
  project belongs to the wave if a container of it has
  `com.docker.compose.project.working_dir` inside one of the wave's worktrees
  (whatever the project is called), or if its name is `<repo>-<slug>` or starts
  with that plus a hyphen (which finds a project whose containers are already
  gone). The name rule never lets a slug claim a longer sibling: with the
  slugs `w5-barcode` and `w5-barcode-hot-cache` in the wave file, cleaning the
  first never touches the second's project. That protection comes from the wave
  file, so **pass `-WaveFile` when the plan is not `wellen.json`** — without the
  file only the slugs on the command line are known, the longer sibling cannot
  be recognized, and the script says so with a `WARNING` rather than failing.
- **A container outside the worktrees vetoes the whole project**, under either
  rule: one container inside a worktree is not enough when another container of
  the same project lives in a directory that is no worktree of this wave. That
  is what keeps a wave from sweeping a Compose project whose name is pinned in
  a file rather than derived per worktree — `docker-compose.e2e.yml` pins one
  for every worktree. A container carrying the project label but no
  `working_dir` label at all vetoes the same way, since it cannot be placed.
  A container-less project has no directory to check, so for it the name rule
  stands alone: one left behind by another clone of this same repository under
  the same `<repo>-<slug>` name is indistinguishable from this wave's own and
  is removed.
- **What is never removed:** the main checkout's own stack; the shared
  `inventory-app-dev` image (the dev override gives it one fixed name, so the
  main checkout uses it too) and every other image without a worktree project's
  label and tag (`postgres`, `traefik`, `tailscale`, the Playwright image); the
  build cache; and anything whose name merely mentions a slug without being a
  Compose project the wave can claim — that is printed as "kept". Note this is
  a weaker promise than "anything of another repository": what protects another
  checkout's resources is a *container* tying them to a directory, so a
  container-less project sharing the `<repo>-<slug>` name is not protected, as
  the bullet above says.
- **The worktrees themselves and their branches stay.** Only Docker state goes,
  and a package worktree recreates what it needs the next time someone runs
  `docker compose` in it.
- **It refuses rather than guesses.** A real run with `-Wave` checks with `gh`
  that the wave issue is closed and refuses otherwise, including when `gh`
  cannot say; a run that cannot list or inspect Docker stops before removing
  anything — a *failed* listing is never read as "nothing to do". `-Slug`
  without `-Wave` cannot know whether the sessions are done and needs `-Force`.
  A `-Slug` given *alongside* `-Wave` that belongs to no package of that wave
  needs `-Force` too: the wave issue vouches for its own packages and for
  nothing else, so `-Wave 3 -Slug w5-barcode` would otherwise let wave 3's
  closed issue authorize removing wave 5's live stack. `-DryRun` never removes
  anything and says whether a real run would be allowed.
- **`-Force` switches off that whole layer**, not just the part about `-Slug`:
  with `-Force` the wave issue is not read at all, even when `-Wave` is given,
  and the cross-wave check on `-Slug` is skipped as well. Ownership by label is
  *not* affected — `-Force` never widens what counts as the wave's. Use it only
  for worktrees you know are finished, and look with `-DryRun` first.
- **A failure only logs.** Cleanup is housekeeping: the orchestrator runs it as a
  background job with a 15-minute limit and writes its output into its own log
  (lines starting `WARNING:` as warnings). If it fails, is refused or times out,
  the log names the command to run by hand and the next wave starts anyway. It
  is idempotent, so running it again is harmless. A wave that is already
  complete when the orchestrator starts is cleaned too.
- **The consolidation session is told not to clean Docker up itself** and never
  to run a prune.

By hand (look first, then remove). **Pass `-WaveFile` whenever the wave is not
from `scripts/wellen.json`** — the default resolves to that file, so a bare
`-Wave 3` against a different plan silently asks about the wrong plan's wave 3:

```powershell
.\scripts\wellen-docker-cleanup.ps1 -Wave 3 -DryRun
.\scripts\wellen-docker-cleanup.ps1 -Wave 3
.\scripts\wellen-docker-cleanup.ps1 -Slug w3-operations,w3-product-maintenance -Force

# A wave of another plan — note -WaveFile on every call, -DryRun included:
.\scripts\wellen-docker-cleanup.ps1 -Wave 1 -DryRun -WaveFile scripts\wellen-followups.json
.\scripts\wellen-docker-cleanup.ps1 -Wave 1 -WaveFile scripts\wellen-followups.json
```

`-Wave` only works once that wave's issue is closed. `-Slug ... -Force` is for
a set of worktrees you know are finished; `-RepoRoot` and `-GhRepo` point it at
another checkout or repository.

**When a change to this takes effect.** The orchestrator reads its own script
and the wave file once, when it starts (the cleanup script is read afresh at
every call). A run that was already going keeps the behavior it started with: a
wave that finishes under it is not cleaned, and the consolidation prompt it
builds for the wave it is already inside does not carry the note. The new files
reach the main checkout only when a consolidation merges `main` into the
integration branch, so the first opportunity is after the consolidation of the
wave that is running when the change lands. Restarting the orchestrator then is
safe and is what picks the change up: it finds the finished waves complete,
cleans the ones that carry `"dockerCleanup": true`, and applies the cleanup to
the waves still to come. `-StartWave n` skips the waves before `n` *without*
cleaning them — clean those by hand with `-Wave`.

Both layers have a test, each running against real Docker in a scratch
environment with an invented repository name, touching nothing of this
repository. Both need Docker and the busybox image:

```powershell
powershell -NoProfile -File scripts\tests\wellen-docker-cleanup.test.ps1   # ~2 min
powershell -NoProfile -File scripts\tests\wellen-orchestrator.test.ps1     # ~1 min
```

The first covers the cleanup script's ownership rules and every guard, driving
`docker` and `gh` through PATH shims for the cases a real daemon cannot be asked
for (a listing that fails after `docker info` succeeded; a wave issue that reads
CLOSED). The second covers the orchestrator's own Docker-facing functions —
`Get-SanitizedProjectName`/`Set-WorktreeEnvOverrides`, `Stop-PackageStack`, the
`"dockerCleanup"` validation, and the `Invoke-WaveDockerCleanup` job wrapper
against a stub cleanup script that returns, refuses or hangs on demand.

## GitHub prerequisites a wave needs

Before the orchestrator can work through a wave, these must exist:

- **One spec issue per package** (`/seed-issues` creates them from the specs
  if missing). The issue number goes into `specIssue`.
- **One wave issue per wave** (like #98–#102 for the specs-12-20 plan):
  carries the package list as a checklist. It is closed by the consolidation
  session — that is how the orchestrator recognizes the wave is done.
- **One wave-plan issue per plan** (like #97 for specs-12-20): describes
  waves, dependencies, and conventions (collective files, pre-assigned
  migration numbers). Its number goes into `plan.planIssue`.
- **The FIRST wave's integration branch** on origin. Every later wave's
  branch is created by the previous wave's consolidation session (the last
  step of its prompt); only the first one needs to be branched and pushed by
  the planning session.
- **Extending a plan while the orchestrator is running.** The orchestrator
  reads the wave file once, at start. A consolidation prompt built from that
  copy names the next wave from that copy, so the wave before a newly added
  one would finish without creating its branch. Get the extended file into
  the main checkout (merge its PR, pull `main`), then stop the running
  orchestrator and start it again. That is restart-safe (see the script's
  `.RESTART SAFETY`): packages whose worktree exists are not started twice.
  From then on the new wave's branch is created by the previous
  consolidation as usual. The one exception is a consolidation that had
  already started before the restart. It keeps its old prompt, so push the
  next wave's branch from `main` by hand once that consolidation has merged.
  Also record the new wave and any migration number it reserves in the
  plan issue's **body**, not only in a comment, because the next planner
  reads the body first. If the work the plan is built on has not merged into
  `main` yet, branch from that work's own branch instead of `origin/main` —
  the consolidation PR against `main` will carry it in naturally. Do not
  wait on a separate PR just to satisfy "branch from main" as a formality.

## Wave-file schema

```jsonc
{
  "plan": {
    "name": "Extended core (specs 12-20)", // appears in every prompt: "wave N of <name>"
    "planIssue": 97,                        // wave-plan issue number
    "conventions": "Keep to the …"          // optional; sentence inserted into every package AND consolidation prompt
  },
  "standards": {                            // defaults, overridable per wave/package
    "model": "claude-sonnet-5",             // model for package sessions
    "effort": "high",                       // low | medium | high | xhigh | max
    "consolidationModel": "claude-opus-5",  // model for consolidation sessions
    "consolidationEffort": "xhigh",
    "advisor": true,                        // advisor on/off (default: on)
    "advisorModel": null,                   // optional; else same model as the session
    "roundLimitPackage": 2,                 // review rounds after which a session stops and reports
    "roundLimitConsolidation": 4
  },
  "waves": [
    {
      "number": 2,                          // unique and strictly ascending
      "waveIssue": 93,                      // wave issue; closed = wave done
      "integrationBranch": "integration/specs-13-20-welle-2",
      "sequential": false,                  // true: packages run one after another, not in parallel
      "dockerCleanup": true,                // true: after the consolidation, remove the Docker
                                             //       state the wave's worktrees created (default: false)
      "external": false,                    // true: wave runs outside this script,
                                             //       only waveIssue is waited on
                                             //       (then "packages": [])
      "packages": [
        {
          "specIssue": 91,                  // spec issue; closed = package done
          "slug": "w2-backup-restore",      // unique; [a-z0-9-] only; becomes the worktree suffix
          "branch": "specs-13-20/w2-backup-restore", // unique; the package's working branch
          "spec": "Spec 15",                // display name; ends up in the window title - no '
          "focus": "…",                     // MANDATORY, see below
          "model": null,                    // optional: override per package
          "effort": null,                   // optional: override per package
          "advisor": true,                  // optional: turn the advisor off per package
          "advisorModel": null              // optional: a stronger advisor for one package
        }
      ]
    }
  ]
}
```

(JSON has no comments — the `//` notes above are explanation only. Optional
fields are omitted, not set to `null`.)

Text rules the validator enforces: `focus` and `plan.conventions` must not
contain a line starting with `'@` (the here-string terminator the prompt is
embedded in); `spec` must not contain a single quote.

## The focus paragraph

`focus` is the most important field: it is inserted verbatim into the
package session's prompt and carries the guardrails the spec and the wave
plan impose. A good focus paragraph:

- names the **silent invariants** (see `CLAUDE.md`'s "Invariants that fail
  silently") the spec touches;
- names a **pre-assigned migration number** verbatim ("Migration
  `00009_notifications.sql` adds `notification_settings`, no other number"),
  when the package has one;
- states what is **deliberately NOT** built ("no retry queue", "external
  UPC/EAN lookup is not in scope");
- names concrete verification aids when there are any (a specific test to
  write, a grep to run against captured logs).

Existing examples are in `scripts/wellen.json` (the specs-12-20 plan).

## Avoiding collisions between parallel packages

Before assigning packages to a wave, check what each one touches:

- **`internal/httpapi/router.go`** — every package that adds a route touches
  it. Treat it as append-only within a wave (new routes appended, nothing
  reordered) regardless of how packages are split; state this in
  `plan.conventions` rather than in every package's `focus`.
- **`internal/httpapi/admin.go` / `admin_input.go` / `web/templates/admin/*`**
  — any package that adds or changes an admin route or page. Spread these
  across different waves rather than parallelizing them in one.
- **A shared static page** (`web/static/settings.html`,
  `web/static/products.html`, …) — two packages that both add UI to the same
  page belong in different waves, not the same one, even if nothing else
  connects them.
- **Migrations** — pre-assign the next free number(s) from `migrations/` per
  package and write them into that package's `focus` verbatim. Keeping at
  most one migration-bearing package per wave removes any doubt about
  filename collisions, at the cost of a little parallelism; it is the
  conservative default, not a hard requirement of the tooling itself.
- **A package that touches nearly every page** (a localization pass, a
  design-token change) belongs in its **own wave, alone, run last** — after
  every other wave's UI work has landed, so it only has to annotate finished
  markup once instead of colliding with everything in flight.

## Model, effort, and reviewers

- **Default**: `claude-sonnet-5` at `high` for packages, `claude-opus-5` at
  `xhigh` for consolidation — set by the user, never silently inherited from
  the account default, which is why the script always passes the flags
  explicitly.
- **A stronger model/effort per package**, when a spec is tagged
  `risk:high` in its GitHub issue (the tenancy boundary, auth invariants,
  the image pipeline, an expiry or audit cascade, a privacy-invariant table
  like `catalog_barcodes`) — this repo's own Build Runbook already
  established the pattern of pinning `claude-opus-5` / `xhigh` to every
  `risk:high` milestone; a wave plan should follow the same rule rather than
  inventing a new one.
- **Sequential instead of parallel** (`"sequential": true`), when packages
  in a wave strongly overlap on the same collective files or build on each
  other.
- **Advisor**: on by default, same model as the session (`--advisor
  <model>`). The intended rule is "same model, one effort step above the
  session, `max` stays `max`" — the claude CLI has no configurable advisor
  effort yet, so only the model is steerable (`advisorModel`). Once the CLI
  offers an advisor effort, that rule belongs in `Get-AdvisorModel` in the
  orchestrator.
- **Three reviewers, not two.** This repo's `/ship` loop runs `review-go`,
  `review-tests`, and `review-docs`; the consolidation prompt names all
  three explicitly when it re-runs the review loop against the wave's full
  diff. A wave plan ported from a project with a different reviewer roster
  must not carry that roster's count over silently.
- **`/batch` is NOT used**: it is purely interactive (no CLI flag, cannot be
  triggered from the initial prompt) and its model — many small PRs from one
  instruction — does not fit the ship loop's adversarial review per spec
  issue.

## Workflow for the planning session

1. Read the wave-plan issue (`plan.planIssue`), the specs of the packages
   being planned under `docs/specs/`, and the issue queue (via the GitHub
   MCP tools or `gh issue list`).
2. Cut packages: independent packages into one parallel wave; whatever
   collides (shared collective files, shared migration numbers,
   dependencies) into later waves or marked `sequential`. Pre-assign
   migration numbers and record them in the wave-plan issue.
3. Set up the GitHub prerequisites (above): missing spec issues via
   `/seed-issues`, wave issues, the first integration branch pushed, the
   wave-plan issue created/extended.
4. Extend `scripts/wellen.json` (or, for a new plan, create a new file and
   start it later with `-WaveFile`). Set `"dockerCleanup": true` on every new
   wave; leave it off only for a wave whose Docker state someone wants to
   inspect afterwards.
5. Check:
   ```powershell
   powershell -NoProfile -File scripts\wellen-orchestrator.ps1 -Validate
   powershell -NoProfile -File scripts\wellen-orchestrator.ps1 -DryRun
   ```
   `-Validate` checks the schema, naming the path of every finding;
   `-DryRun` additionally shows which sessions would start with which
   model/effort/advisor — neither starts anything.
6. Report to the user what was planned, and the command that starts it. Do
   **not** start the orchestrator yourself — that is the user's call.

## Prompt for the planning session

This prompt (replace the `<…>` placeholders) is given to a fresh session
started with `claude --model claude-sonnet-5 --effort high`:

```
Read scripts/wellen-planen.md in full and follow the workflow it describes.
Plan <wave N / waves N-M> for <plan, e.g. "Extended core, next batch"> with
specs <list, e.g. "21, 24, 25">.

Cut packages so that parallel sessions never reorder the same collective
files or need the same migration number; pre-assign migration numbers and
record them in the wave-plan issue. Write a focus paragraph for every
package following the rules in wellen-planen.md - the silent invariants
from CLAUDE.md, what is deliberately not built, and the migration number
verbatim.

Set up the GitHub prerequisites (spec issues, wave issues, wave-plan issue,
first integration branch), extend scripts/wellen.json, and check with
-Validate and -DryRun. Do not start the orchestrator. Report at the end:
planned waves with packages and model/effort per session, issues and
branches created, and the command that starts the orchestrator.
```
