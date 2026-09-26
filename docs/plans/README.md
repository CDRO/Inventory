# Plans

Engineering plans for work on the *harness* — CI, the review loop, the wave
orchestrator, deployment tooling — as opposed to the product.

**Nothing in this folder is an implementation contract.** Like
`docs/explanations/`, a plan is narrative: it explains why a set of issues
exists, what they are meant to achieve together, and what was measured before
and after. The contract for each piece of work is its GitHub issue (acceptance
criteria) and, where the work changes a deployment or operations guarantee,
the numbered spec under `docs/specs/` that the issue names. When a plan and a
spec disagree, the spec wins; when a plan and an issue disagree, the issue
wins.

A plan is updated when its measurements change (baseline → after) and closed
by a final "what happened" section. It is not deleted afterwards: the numbers
are the record.

| Plan | Central issue | Status |
|---|---|---|
| [2026-09 Harness optimization](2026-09-harness-optimization.md) | see the plan's header | proposed |
