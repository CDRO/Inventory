---
name: ship
description: Implement one issue end to end — branch, code, test in Docker, open a PR, run the three adversarial reviewers, fix what they block on, and auto-merge when all three approve and tests pass. Use to start or continue work on a single spec issue.
allowed-tools: Read, Write, Edit, Grep, Glob, Bash, Agent
---

# Ship one issue

One issue → one branch → one PR → reviewed → merged. Keep PRs small; a diff
that does not fit comfortably in a reviewer's context gets a worse review.

## 1. Start

```bash
gh issue view <N>                      # requirements + acceptance criteria
git switch main && git pull
git switch -c spec/<NN>-<slug>
```

Read the spec file(s) the issue names from `docs/specs/` **before writing
code**. The spec is the contract; the issue is a pointer to it.

## 2. Implement

Only what the issue covers. If you find an unrelated problem, open an issue for
it — do not fix it here. The reviewers block on scope creep, and they are right
to: an out-of-scope fix in a reviewed PR is a change nobody agreed to.

Update `.claude/worklog.md` as you go.

## 3. Test before pushing

Everything runs in Docker — no host toolchain
(`docs/specs/01-architecture-and-deployment.md`).

Run `scripts/dev check` (`sh scripts/dev check` if the executable bit is
missing, #336) before `scripts/dev test`; do not push on a nonzero exit — it
is the mechanical half of what a reviewer's round would otherwise catch (H6).

Run the suite so the **full log lands on disk and only the signal enters context**:

```bash
scripts/dev test                      # docker compose run --rm app go test ./...
scripts/dev test ./internal/store/    # one package while iterating; the full suite before pushing
```

`scripts/dev test` (`scripts/dev.d/test`) runs the documented command, writes
the complete output to `.claude/last-test.log` (gitignored) and prints only
the `FAIL`/`panic:` lines with their file:line, the last three lines and
`exit=<code>` — a green suite costs four lines instead of several hundred; a
red one shows the failures. Read the log when a failure needs more than the
excerpt. Never summarize a run you did not perform, and never report an exit
code you did not see.

Do not push a red suite; the test reviewer will block and the round is wasted.

**If this session has no working local `docker compose`** (no daemon, no
`CAP_NET_ADMIN` — see issue #48), there is no local suite to run before the
first push. Use the `test` GitHub Actions workflow as a pre-PR fallback
instead of skipping this step. `scripts/dev ci-status` (`scripts/dev.d/ci-status`)
is the dispatch-and-poll recipe as a command: it dispatches the workflow on
the branch, finds the run by the exact commit SHA (never "the newest run in
the list", which can briefly still be an older one right after a dispatch),
and blocks on `gh run watch --exit-status`, so its exit code is the run's:

```bash
git push -u origin spec/<NN>-<slug>
scripts/dev ci-status "$(git rev-parse HEAD)" --dispatch spec/<NN>-<slug>   # non-zero = red
```

Slower than local Docker — each round-trip is a push and a runner boot — but
it means a red suite is still caught before opening the PR, not after.

### If you run the E2E gate: `scripts/dev e2e`, and you owe the teardown

The E2E suite is the deployment gate, not a per-PR check — `review-tests`
treats E2E journeys as "not verified here", and `e2e.yml` never runs on a pull
request (`docs/specs/01-architecture-and-deployment.md`). So most package PRs
never touch it. When something does need it — a consolidation, a frontend
change you want to see driven for real — run it as one command:

```bash
scripts/dev e2e                 # claim the gate, run the sequence, tear down
scripts/dev e2e status          # who holds it, and whether a suite is really running
scripts/dev e2e break-lock      # release a hold whose owner is gone
```

Never paste the compose commands from `docker-compose.e2e.yml`'s header
instead. A machine has exactly one `inventory-e2e` project shared by every
checkout on it, and the wrapper is what takes the lock that keeps two sessions
out of each other's stack (#389), tags the image per checkout so another
checkout's build cannot serve its frontend under your tests (#391), and checks
that the frontend being served is this working tree's before it believes a
single result.

**Bringing that stack up obliges you to tear it down.** `scripts/dev e2e` does
it from a trap, so it survives an interrupt; a hand-run sequence does not. A
session that stopped one command early once left its containers `Up` and
`healthy` for over half an hour while the next checkout waited, reading them as
a run in progress (#377). If you are ever unsure whether a stack is yours to
clear, `scripts/dev e2e status` answers it with evidence — is there a container
for the `e2e` service at all, has `app` logged anything lately, what does the
holder's own worklog say — and says `ACTIVE` or `STALE` rather than leaving you
to guess from three healthy containers.

**Reading a local result.** The wrapper caps workers and sets one retry, both
for local runs only (#382: 28 failed and 5 did not run on an unmodified commit
that CI passed 191/191 twice, on a machine also running five other compose
projects). So a local run can end with a `flaky` count, which CI can never
produce — CI keeps `retries: 0`. A `flaky` line means the test passed on the
retry: contention, not a regression, and not something to "fix". A `failed`
line still means failed on both attempts. A `did not run` count is a reset
signal, not a pass — see spec 01's note on the suite not being idempotent.

## 4. Open the PR

```bash
git add -A && git commit -m "<type>: <what changed>

Implements #<N> (docs/specs/NN-name.md).

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
git push -u origin spec/<NN>-<slug>

gh pr create --title "Spec <NN>: <title>" --body "$(cat <<'EOF'
Implements #<N> — `docs/specs/NN-name.md`

## What changed
- ...

## Acceptance criteria
- [x] ...
- [ ] ... (why not)

## Tests
`docker compose run --rm app go test ./...` → exit 0

## Review round
1

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

## 5. Review round

Generate the packet once before spawning the three reviewers —
`scripts/dev packet <PR>` on round 1, `scripts/dev packet <PR> --since
<previous round's head SHA>` on round ≥ 2 — so all three read one file
instead of each re-gathering the PR, the issue, the spec and the diff
themselves (`scripts/dev.d/packet`, H7).

Spawn **all three reviewers in one message** so they run in parallel:

- `review-go`
- `review-tests`
- `review-docs`

Give each: the PR number, the issue number, the spec path, the round number,
and — on round ≥ 2 — the head SHA the previous round reviewed (the `--since`
value from step 7), so a reviewer can rebuild the packet itself if it is
missing or stale. Each posts its own PR comment and returns a short summary.

**Do not pass a `model` argument when spawning them.** The Agent tool's
`model` parameter overrides frontmatter, which would silently undo the pinned
`sonnet`/`xhigh` in their definitions.

### Turn budget: size it to the diff

Each reviewer's `maxTurns` is fixed in its frontmatter — `review-go` 20,
`review-docs` 15, `review-tests` 45 (raised from 25 by #471, after four runs on
one package PR stopped at the limit without posting a verdict) — and sized for a
package PR of a handful of files. A consolidation diff is several times that, and the budget does not
stretch: on PR #360 (14 files, ~1,820 lines) all three lanes overran, and four
of the six lane-runs stopped **before posting a verdict** (#364).

That failure is invisible, which is what makes it worth spending a line on.
`scripts/dev gate` reads only the posted marker, so a reviewer that did the
entire review and ran out one step before `gh pr comment` counts as `missing`
— indistinguishable from one that never ran — and the PR sits at `WAIT` with
nothing saying why.

The Agent tool has no per-spawn `maxTurns` override, so the budget travels in
the spawn prompt, next to the round number. Read the size off the packet's own
`## Diff --stat` section before spawning (or `git diff --stat <base>...<head>
| tail -1`), and when the diff is **over 10 files or 800 changed lines** —
every consolidation PR, and the occasional large package — add one line to
each of the three spawn prompts:

> This diff is <F> files / <L> changed lines, several times the size your
> frontmatter turn budget assumes. Reach a defensible position and post your
> verdict comment before you run out of turns; an unposted verdict reads as
> `missing` to `scripts/dev gate` and stalls the PR at `WAIT` with no signal
> that anything went wrong.

Same precedent as the round cap, which a consolidation already raises from 2
to 4 (`roundLimitPackage` / `roundLimitConsolidation` in the wave plan's JSON,
read at `scripts/wellen-orchestrator.ps1:873` and `:893`): the loop's limits
are sized for a package PR, and a PR several times that size states its own.

### A stalled reviewer — no verdict comment posted

An agent can spend its whole turn budget investigating and stop, `maxTurns`
reached, without ever calling `gh pr comment` (#370, #387, #393). To
`scripts/dev gate` that is indistinguishable from a reviewer nobody spawned
this round — both read as `missing` — so only the spawning session can tell
the difference, because it is the only party that knows which three reviewers
it actually dispatched.

Check each reviewer's own returned summary (the two-line report its prompt's
last instruction asks for) as soon as it comes back. A summary with no verdict
line, or one that says it stopped at its turn limit, is a **stall** — a third
state, distinct from `BLOCK` and from "has not reviewed yet". Treat it as
neither.

On a stall:

1. **Resume it once, automatically, before this counts as a round.** Try
   resuming the same agent first (`SendMessage` to its id or name) — cheaper,
   since it keeps the context it already gathered. Resume frequently fails
   (`No transcript found for agent ID: …`, observed on #387); when it does,
   respawn that one reviewer fresh instead of retrying the resume.
2. Either way, the resume/respawn prompt must say explicitly: *"A previous
   agent on this PR stopped at its turn limit without posting a verdict. Your
   PR comment is the only output that counts — post it with turns to spare,
   and do not call the advisor."*
3. If it stalls a second time, stop and tell the user instead of retrying
   again. Two stalls on the same reviewer in the same round is no longer a
   routine hiccup this loop should paper over.

Only once all three reviewers have a posted verdict for the round — or you
have stopped to report a repeat stall — does step 6 run.

## 6. The gate — read verdicts back from GitHub

```bash
scripts/dev gate <PR>
```

**Decide from this, not from what the agents told you.** A subagent's report
is not visible to the user and is easy to remember generously; the verdict
marker each reviewer posts on the PR is the record, and for the merge
decision itself `scripts/dev gate` (`scripts/dev.d/gate`, H5) reads only
those markers back from GitHub — never the prose above them, and never a
comment whose `sha=` does not match the PR's current head commit, so a stale
approval from before your last push can never count. (The one exception: on
a PR against `main`, its documentation-only fallback also reads the current
round's own test-reviewer comment for a `**Suite:**` line — never a stale
one — see the command's own `--help`.)

It prints one line per reviewer and then exactly one of `MERGE`, `WAIT
<reviewers>` or `BLOCK <reviewers>`, exiting 0, 3 or 4 respectively. Merge
only on `MERGE`. `WAIT` means a reviewer has not posted a verdict for the
current head commit yet — that is not an approval, however many times it ran
before. On a PR against `main`, `MERGE` also depends on the `test` check
(pending or failed keeps it from printing `MERGE`; see the command's own
`--help` for the docs-only exception).

## 7. If anything blocks

1. Fix the blocking findings. Only those, plus anything genuinely required to
   make them work.
2. If you believe a finding is wrong, **reply to it on the PR** with your
   reasoning (`gh pr comment`) instead of ignoring it. A disputed finding that
   is argued in the open is resolved; one that is silently skipped is not.
3. Re-run tests, push, then regenerate the packet for the new head —
   `scripts/dev packet <PR> --since <the head SHA the previous round
   reviewed>` — before re-reviewing from step 5, and re-run `scripts/dev
   gate` after. The `--since` packet is what makes round ≥ 2 a delta review
   (decision D9 of the harness plan): it inlines only the diff since that
   SHA, points at the full diff instead of repeating it, and carries each
   reviewer's own previous verdict, which their "Round 2 and later" sections
   read to account for every round-1 blocking finding as `resolved` or
   `still open` before they raise anything new. Increment the round number
   you pass the reviewers.
4. **Round cap: 2.** After two review passes, stop. Do not run a third.
   Open a GitHub issue for whatever is still outstanding — the finding, its
   file and line, a reproduction if there is one, and why it was deferred —
   then merge and flag it in the report.

   This is a budget rule, not a quality judgement. It exists because
   "every round found a real bug" is not a reason to keep going: spec 07
   (PR #32) produced a genuine, proof-of-concept-verified security finding
   in five consecutive rounds, every fix was correct, and it still cost far
   more than the feature was worth.

   When deferring a security finding, say so plainly in the report along
   with the risk, so a human can overrule. The cap limits the review loop,
   not the honesty about what is shipping.

## 8. Merge

```bash
gh pr merge <PR> --squash --delete-branch
gh issue close <N> --comment "Shipped in #<PR>."
git switch main && git pull
```

Then clear `.claude/worklog.md` and report: what shipped, what the reviewers
caught, and what is next.

## Rules

- Never `--force` push a branch with an open PR.
- Never commit to `main` directly.
- Never merge on your own judgment that the code is fine — the gate in step 6
  is the only path to a merge.
- Never edit or delete a reviewer's comment.
- If tests cannot run at all (Docker down, DB unreachable), stop and tell the
  user. Do not merge with an unrun suite, and do not describe an unrun suite as
  passing.
- Never leave an E2E stack up. If you brought `inventory-e2e` up, it comes down
  before you finish — `scripts/dev e2e` does that for you, and `scripts/dev e2e
  down` is the manual escape if you ran the raw commands (#377).
