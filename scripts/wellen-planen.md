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

1. Per wave, it waits until the integration branch exists on origin.
2. Per package, it creates a worktree and starts a visible, interactive
   Claude session with prompt, model, effort, and advisor as CLI arguments
   (`claude --model … --effort … --advisor … --remote-control …`).
3. It polls GitHub until every spec issue of the wave is closed, then starts
   the consolidation session in the main checkout and waits until the wave
   issue is closed. Only then does the next wave begin.

The script itself never writes to git/GitHub (other than `worktree add` and
copying `.env`) — every substantive action is done by the sessions it
starts. **Planning a new wave therefore means: extend the JSON file and set
up the GitHub prerequisites. The script itself is never changed.**

Docker isolation between parallel package worktrees (`COMPOSE_PROJECT_NAME`,
`HTTP_PORT`, `TRAEFIK_PORT`) is automatic and needs no attention when
planning a wave — the script assigns each package a deterministic, unique
set of these from its position in the wave file the moment a worktree is
created. See `docs/specs/01-architecture-and-deployment.md`, "Running more
than one instance of the stack locally", for the mechanism itself.

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
  the planning session. If the work the plan is built on has not merged into
  `main` yet, branch from that work's own branch instead of `origin/main` —
  the consolidation PR against `main` will carry it in naturally. Do not
  wait on a separate PR just to satisfy "branch from main" as a formality.

## Wave-file schema

```jsonc
{
  "plan": {
    "name": "Extended core (specs 12-20)", // appears in every prompt: "wave N of <name>"
    "planIssue": 97,                        // wave-plan issue number
    "conventions": "Keep to the …"          // optional; sentence inserted into every package prompt
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
   start it later with `-WaveFile`).
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
