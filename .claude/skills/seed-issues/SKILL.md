---
name: seed-issues
description: Builds and keeps the work queue — turns docs/specs into GitHub issues (labels, milestones, verbatim acceptance criteria, dependencies) and mirrors the queue into the Build Runbook artifact. Use before the first /ship run, when new specs are written, and whenever issues are added, closed or packaged into PRs so the runbook shows them.
allowed-tools: Read, Grep, Glob, Bash, Write, Edit, Artifact, Skill
---

# Seed the work queue from the specs

Turns `docs/specs/` into GitHub issues, then mirrors the queue into the Build
Runbook. The specs are already build-ordered and carry explicit acceptance
criteria, so this is mechanical — but it is the step that makes the work
resumable, because **issues are the only durable record of what is done**. The
runbook is the human-facing view of that record, never a second source of truth.

Safe to re-run. It must not duplicate: check `gh issue list --state all` first
and skip specs that already have an issue. A re-run with nothing new to create
still does step 4, which is how the runbook catches up with the queue.

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

## 4. Mirror the queue into the runbook

The Build Runbook lives at **https://claude.ai/artifact/U1PVho6yiWZDhbmd2mAQoY**.
It lists every unit of work with the prompt to paste and a status a person
clicks through. Update it in place — publish to that URL, never a new artifact.

**The page stores its data in two JSON blocks, and this step edits only those.**
The markup, the CSS and the script stay as they are:

- `<script id="plan" type="application/json">` — `{"phases":[{key, name, note, items:[…]}]}`
- `<script id="state" type="application/json">` — `{"status":{"<item id>":"todo|working|done"}, "updated":"<ISO time>"}`

[`runbook.pl`](runbook.pl), next to this file, does the checking and the
writing back, so neither is done by eye. Git Bash ships perl, so it needs no
host toolchain. Work in a scratch directory, not the repository.

### Procedure

1. Load the `artifact-design` skill; it must be loaded before any artifact file
   is written. Leave `artifact-capabilities` alone unless you are changing the
   page's script, which a data update never does.
2. Read the page twice. `Artifact(action: "read", url: <runbook>)` is what lets
   the later publish go through. `Artifact(action: "read", url: <runbook>,
   path: "index.html", out_dir: <scratch dir>)` saves the same page as a local
   file, `page.html` below. Every republish is built from that file, never
   retyped. The page arrives inside the platform's own
   `<!doctype html><head>…` skeleton, which is expected and republishes as-is.
3. **Check the file is clean before editing it:**
   `perl .claude/skills/seed-issues/runbook.pl check page.html`. It exits 0 with
   a one-line summary, or exits 1 and names each problem: a script the page did
   not write, a node of the page's own that appears twice, the viewer's runtime
   baked in, a script closed early, a data block that is not valid JSON or holds
   a literal `<`, a duplicated id, a status for no item. Any of those means the
   page's own save has regressed, or someone edited it by hand: stop, report
   the output, and do not republish that copy.
4. `perl .claude/skills/seed-issues/runbook.pl extract page.html plan.json state.json`
   writes the two blocks out as readable JSON. Edit those two files.
5. Edit `plan.json`:
   - **Look before adding.** Search the plan for the issue number first. A
     spec issue that already has an item, or a package whose PR already has
     one, is updated in place and never added a second time, so a re-run
     changes nothing that is already there. One issue can still sit in two
     packages when each finishes part of it.
   - **One item per issue** for spec issues, in the phase their spec belongs to
     (`01`–`05` Foundations, `06`–`11` Features, `50`–`52` Later), id `m<next>`.
   - **One item per PR** for follow-up issues in the Follow-ups phase, id
     `f<next>`. When several issues are packaged into one PR, that PR is one
     item, and its `spec` field lists every issue number it covers
     (`"#74 #75 #30"`).
   - Fields: `id`, `n` (display number, continuing the sequence), `spec`,
     `title`, `desc` (one sentence, what lands), `model` and `effort` (Opus 5
     for `risk:high` work, else Sonnet 5), `size`, `risk` (true for `risk:high`),
     `steps` (`[["Set the model","/model opus"],["Then paste","/pickup\n\n…"]]`)
     and `after` (what to check by hand once it lands).
   - **Ids are permanent.** Never delete an item or reuse an id: the `state`
     block keys on them, and a removed id orphans its status.
6. Edit `state.json` from GitHub, which is authoritative. The first rule that
   holds wins:
   - every issue the item covers is closed → `"done"`
   - an open PR exists for the item → `"working"`
   - otherwise leave an existing value as it is: a person may have marked it
     `working` for a reason GitHub cannot see. A new item gets no entry, which
     the page shows as not started.
   - Set `"updated"` to the current time. Without that, a viewer's older copy in
     their browser storage outranks what you publish.
7. `perl .claude/skills/seed-issues/runbook.pl inject page.html plan.json state.json out.html`
   writes the two blocks back into an otherwise byte-identical page. It refuses,
   and writes nothing, when either file is not valid JSON, when an item or a
   status that the page had is gone, when an id is used twice or a status names
   no item, or when `"updated"` is not later than the page's. It escapes every
   `<` in the data as `\u003c`, so no text can close the script tag, and checks
   the result is clean before writing it.
8. `Artifact(action: "publish", url: <runbook>, file_path: out.html)`.
   Omit `favicon`, `capabilities` and `contract`: omitting `capabilities` carries
   the page's `artifact` grant forward, which is what lets a status click save.
   A `conflict` means a viewer saved in between: start again from step 2.
9. The public share link is pinned to a version. Link viewers see an update only
   once the owner moves the pin in the share menu, so say so in the report.

## 5. Report

Print the created issue numbers in order and name the first unblocked one.
That is where `/pickup` will start. Then give the runbook link and what changed
on it: items added, and statuses moved.
