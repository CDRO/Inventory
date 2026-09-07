# Inventory — working agreement

Self-hosted, AI-powered household inventory system. **`docs/specs/` is the
implementation contract.** Read the relevant spec before writing code; it is
detailed on purpose, and it is more authoritative than anything inferred from
existing code.

- `docs/specs/00-overview.md` — start here; map of the whole set
- `docs/specs/` numbering: `00`–`11` core, in build order · `12`–`49` reserved
  · `50`–`52` gamification (later phase, nothing in `00`–`11` may depend on it)
- **`docs/explanations/` is not a contract.** It is human-facing narrative.
  Ignore it when implementing.

## Resuming work

Run **`/pickup`**. It reconstructs state from GitHub (open PR, review verdicts,
issue queue) and reports before acting. GitHub is authoritative;
`.claude/worklog.md` is local scratch and may be stale.

Never start implementation work by guessing what is next — the issue queue
knows.

## Shipping a change

**`/ship`** — one issue → one branch → one PR → three adversarial reviews →
auto-merge when all three approve and tests pass. `/seed-issues` bootstraps the
queue from the specs (once).

Every change is reviewed by three subagents that cannot edit code:
`review-go`, `review-tests`, `review-docs`. They post verdicts as PR comments.
The merge gate reads those comments back from GitHub — never from what an agent
reported in-session.

## Hard constraints

- **No host toolchain.** No Go, Node, Python, or Java on the machine. Every
  build, test, lint, and migration runs through `docker compose`
  (`docs/specs/01-architecture-and-deployment.md`).
- **Backend is Go**, static binary in a `scratch` container.
- **Frontend is vanilla JS** — no Node, npm, React, TypeScript, Tailwind, or
  any build step. Libraries only as single vendored files, never from a CDN.
- Tests: `docker compose run --rm app go test ./...`
- Never commit to `main`; never force-push a branch with an open PR.

## Invariants that fail silently

These are the ones where tests pass while the guarantee is broken. Check them
whenever you touch routing, handlers, uploads, or expiry:

- `404`-not-`403` for unknown **and** inaccessible storages, and for the whole
  admin area — identical response either way.
- `is_admin` re-queried from the DB on every admin request; never returned in
  any JSON, never a client-side branch.
- `debug_reason` only when `APP_ENV=dev`, emitted from the single error
  serializer.
- Same-storage validation on every id: path, `parent_id`, move target,
  confirm-body field.
- Every `inventory_batches` write paired, in the same transaction, with an
  `inventory_logs` row.
- EXIF stripped from every upload, with orientation applied to pixels **first**.
- Expiry cascades touch `derived` dates only, never `user` ones.
