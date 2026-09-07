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
docker compose run --rm app go test ./... > .claude/last-test.log 2>&1
echo "exit=$?"
grep -E '^(--- )?FAIL|^panic:|^\s+.*\.go:[0-9]+' .claude/last-test.log | head -40
tail -3 .claude/last-test.log
```

A green suite costs three lines instead of several hundred; a red one shows the
failures and their file:line. The complete output stays in
`.claude/last-test.log` (gitignored) — read it when a failure needs more than
the excerpt. Never summarize a run you did not perform, and never report an
exit code you did not see.

Do not push a red suite; the test reviewer will block and the round is wasted.

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
gh pr view <PR> --comments
```

**Decide from the posted comments, not from what the agents told you.** A
subagent's report is not visible to the user and is easy to remember
generously; the comment on the PR is the record, and it is what the user will
read later. Re-read it.

Merge only when **all** of these hold:

- three `VERDICT: APPROVE` headers for the current round — one per reviewer
- the test reviewer's `**Suite:**` line shows a passing exit code
- no unaddressed blocking finding anywhere in the current round

A missing reviewer comment is not an approval. Two approvals and a silence is
not a pass.

## 7. If anything blocks

1. Fix the blocking findings. Only those, plus anything genuinely required to
   make them work.
2. If you believe a finding is wrong, **reply to it on the PR** with your
   reasoning (`gh pr comment`) instead of ignoring it. A disputed finding that
   is argued in the open is resolved; one that is silently skipped is not.
3. Re-run tests, push, increment the round, and re-review from step 5.
4. **Round cap: 3.** After three rounds with unresolved blocks, stop and ask
   the user. Grinding on a finding you cannot resolve wastes tokens and usually
   means the spec is ambiguous — which is a question for a human.

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
