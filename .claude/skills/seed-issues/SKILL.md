---
name: seed-issues
description: One-time bootstrap that turns docs/specs into the GitHub issue queue — labels, milestones, one issue per spec with its acceptance criteria as checkboxes and dependencies wired up. Use once before the first /ship run, or to add issues for newly written specs.
allowed-tools: Read, Grep, Glob, Bash
---

# Seed the work queue from the specs

Turns `docs/specs/` into GitHub issues. The specs are already build-ordered and
carry explicit acceptance criteria, so this is mechanical — but it is the step
that makes the work resumable, because **issues are the only durable record of
what is done**.

Run once. Re-running must not duplicate: check `gh issue list --state all`
first and skip specs that already have an issue.

## 1. Labels and milestones

```bash
gh label create "spec"      --color 0366d6 --description "Implements a docs/specs file" --force
gh label create "area:backend"  --color 5319e7 --force
gh label create "area:frontend" --color 1d76db --force
gh label create "area:infra"    --color 0e8a16 --force
gh label create "risk:high" --color b60205 --description "Security, tenancy, or data-integrity critical" --force

gh api repos/:owner/:repo/milestones -f title="Foundations (00-05)" 2>/dev/null || true
gh api repos/:owner/:repo/milestones -f title="Features (06-11)"    2>/dev/null || true
```

`risk:high` goes on specs `02`, `03`, `04`, `07`, and `08` — the multi-tenancy
boundary, the auth invariants, the image pipeline, and the expiry cascade.
These are where a silent failure is most expensive, and the label is what tells
a future session to slow down.

## 2. One issue per spec

Read each `docs/specs/NN-*.md` and create:

```bash
gh issue create \
  --title "Spec 03: Auth & multi-tenancy" \
  --label spec --label area:backend --label risk:high \
  --milestone "Foundations (00-05)" \
  --body "$(cat <<'EOF'
Implements `docs/specs/03-auth-and-multi-tenancy.md`.

**Read the spec first — it is the contract. This issue is a pointer to it.**

## Acceptance criteria
<verbatim from the spec's acceptance criteria / rules, one checkbox each>
- [ ] ...

## Watch for
<the invariants in this spec that fail silently — copy the specific ones>

Blocked by #<n>
EOF
)"
```

Rules:

- **Transcribe acceptance criteria verbatim.** Do not paraphrase or "improve"
  them. The reviewers check the diff against these, so a criterion reworded
  here is a criterion no longer enforced.
- Specs `50`–`52` (gamification) are a later phase — create them with a
  `Blocked by` on every core spec, so they never surface as next work by
  accident.
- Skip `00-overview.md`: it is a map, not an implementable unit.
- Split a spec into several issues only if it plainly contains independent
  units of work. Prefer one issue per spec; small PRs come from small specs,
  not from artificial slicing.

## 3. Dependencies

Specs are build-ordered, so each depends on the one before it. Add
`Blocked by #N` referencing the previous spec's issue. Cross-cutting extras
worth wiring explicitly:

- everything depends on `01` (Docker/Go skeleton) and `02` (schema)
- `06`–`11` depend on `04` (API conventions) and `05` (frontend foundations)
- `07`, `09` depend on `06` (shared review/job machinery)

## 4. Report

Print the created issue numbers in order and name the first unblocked one.
That is where `/pickup` will start.
