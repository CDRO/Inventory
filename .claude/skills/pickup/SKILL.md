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
gh pr view <PR> --comments
```

Then read `.claude/worklog.md` if it exists (mid-task scratch: the file being
edited, the next intended action). It is gitignored and may be absent or
stale — treat it as a hint, never as truth.

## Step 2 — Parse the review verdicts

Reviewer comments start with a fixed header:

- `## Go Review — VERDICT: APPROVE|BLOCK`
- `## Test Review — VERDICT: APPROVE|BLOCK`
- `## Docs Review — VERDICT: APPROVE|BLOCK`

For the **current round only** (the highest `**Round:**` value present), record
each reviewer's verdict and its blocking findings. Earlier rounds are history;
do not re-fix findings that a later round dropped.

A reviewer with no comment in the current round has **not reviewed yet** —
that is not an approval.

## Step 3 — Route

| State | Action |
|---|---|
| Open PR, any `BLOCK` in current round | Fix the blocking findings, then re-run the review round via the `ship` skill |
| Open PR, all three `APPROVE`, tests green | Merge per the `ship` skill's gate |
| Open PR, fewer than three verdicts this round | Spawn only the missing reviewers |
| Open PR, uncommitted local changes | Finish the change, run tests, push, then review |
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
- If the same finding survives three rounds, stop and ask the user.
