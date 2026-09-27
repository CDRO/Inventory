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

Run it so the **full log lands on disk and only the signal enters context**:

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

Spawn **all three reviewers in one message** so they run in parallel:

- `review-go`
- `review-tests`
- `review-docs`

Give each: the PR number, the issue number, the spec path, and the round
number. Each posts its own PR comment and returns a short summary.

**Do not pass a `model` argument when spawning them.** The Agent tool's
`model` parameter overrides frontmatter, which would silently undo the pinned
`sonnet`/`xhigh` in their definitions.

## 6. The gate — read verdicts back from GitHub

```bash
scripts/dev gate <PR>
```

**Decide from this, not from what the agents told you.** A subagent's report
is not visible to the user and is easy to remember generously; the verdict
marker each reviewer posts on the PR is the record, and `scripts/dev gate`
(`scripts/dev.d/gate`, H5) reads only those markers back from GitHub — never
the prose above them, and never a comment whose `sha=` does not match the
PR's current head commit, so a stale approval from before your last push can
never count.

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
3. Re-run tests, push, increment the round, re-review from step 5, then
   re-run `scripts/dev gate`.
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
