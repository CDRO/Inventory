---
name: pickup
description: Resume work on the Inventory implementation where it was left off. Reconstructs state from GitHub (open PRs, review verdicts, issue queue) and the local worklog, reports the situation, then continues. Use when the user says "pick up where you left off", "continue", "what's next", or starts a fresh session on this project.
allowed-tools: Read, Write, Edit, Grep, Glob, Bash, Agent
---

# Pick up where the work stopped

Reconstruct state from **GitHub first, worklog second**, report what you found,
then continue. GitHub is authoritative: if the worklog and GitHub disagree,
GitHub is right and the worklog is stale.

## Step 1 — Gather state (read-only, always do all of it)

```bash
git status --short && git branch --show-current
gh pr list --state open --json number,title,headRefName,isDraft
gh issue list --state open --limit 40 --json number,title,labels
```

If an open PR exists, get its review record:

```bash
gh pr view <PR> --json number,title,body,headRefName,mergeable,statusCheckRollup
scripts/dev gate <PR>
```

`scripts/dev gate` (`scripts/dev.d/gate`, H5) is the only source for verdicts:
it reads each reviewer's `<!-- verdict: ... -->` marker back from GitHub,
scoped to the PR's *current* head SHA, so a stale `APPROVE` from before the
last push can never count. Use `gh pr view <PR> --comments` only to read a
finding's prose when you need the detail — never to derive a verdict from it.

Then read `.claude/worklog.md` if it exists (mid-task scratch: the file being
edited, the next intended action). It is gitignored and may be absent or
stale — treat it as a hint, never as truth.

## Step 2 — Read the verdict

```bash
scripts/dev gate <PR>
```

It prints one line per reviewer — `go: APPROVE r2 @abc1234`, `tests: stale
APPROVE r1 @def5678 (head is @abc1234)`, or `docs: missing` — followed by
exactly one of `MERGE`, `WAIT <reviewers>` or `BLOCK <reviewers>` (exit 0, 3,
4 respectively). A `stale` or `missing` reviewer has **not** approved the
current head; treat it the same as `WAIT`, never as an approval. On a PR
against `main`, `MERGE` also depends on the `test` check (see the command's
own `--help` for the docs-only fallback).

For the **current round** (the `round=<n>` the gate's own output uses), read
each `BLOCK` reviewer's comment for its findings via `gh pr view <PR>
--comments`. Earlier rounds are history; do not re-fix findings that a later
round dropped.

## Step 3 — Route

| State | Action |
|---|---|
| Open PR, `scripts/dev gate` prints `BLOCK <reviewers>` | Fix the blocking findings, then re-run the review round via the `ship` skill |
| Open PR, `scripts/dev gate` prints `MERGE` | Merge per the `ship` skill's gate |
| Open PR, `scripts/dev gate` prints `WAIT <reviewers>` | Spawn only the missing or stale reviewers |
| Open PR, uncommitted local changes | Finish the change, run tests (`scripts/dev test`), push, then review |
| No open PR, issues remain | Pick the lowest-numbered unblocked issue; start it via `ship` |
| No open PR, no issues | Say so and ask what to do — do not invent work |
| Working tree dirty on `main` | Stop and ask. Never commit to `main` directly |

An issue is **unblocked** when every `Blocked by #N` in its body is closed.

## Step 4 — Report before acting

Always tell the user, in one short paragraph, before doing anything that
writes:

- where things stand (branch, PR, round, verdicts)
- what you are about to do
- anything that looks wrong (stale worklog, PR with no issue, dirty `main`)

Then proceed without waiting for confirmation, **except** when the state is
ambiguous or destructive — conflicting state, a PR whose branch no longer
exists, more than one open PR, or anything requiring a force-push. Ask then.

## Step 5 — Keep the worklog current

Rewrite `.claude/worklog.md` at every phase boundary — starting a task, before
running tests, before pushing, after a review round, after a merge:

```markdown
# Worklog (local scratch — gitignored; GitHub is authoritative)
Updated: <ISO timestamp>

Issue:   #12 — Spec 03: auth & multi-tenancy
PR:      #14 (round 2)
Branch:  spec/03-auth-multi-tenancy
Phase:   fixing review findings

Next action:
- Fix Go review blocking #1 (403 → 404 in RequireStorageMember)

Open blockers:
- none
```

Anything that must survive a reboot goes in the **issue or the PR**, not here.

## Guardrails

- Never `git push --force` on a branch with an open PR.
- Never commit directly to `main`.
- Never mark a reviewer's finding resolved without changing code or arguing
  the point in a PR reply.
- **Two review passes per PR, then stop.** Do not run a third. Open an issue
  for whatever is still outstanding, merge, and flag it in the report. This is
  a budget rule, not a quality one — see the same cap in the `ship` skill for
  why. When deferring a security finding, state the risk plainly so a human
  can overrule.
