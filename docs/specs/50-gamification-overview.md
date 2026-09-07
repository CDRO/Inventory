# 50 — Gamification: Overview & Principles

**Phase marker:** specs numbered `50` and above are a **later phase**.
Numbers `12`–`49` are reserved for core work still to be specified.
Nothing in specs `00`–`11` may depend on anything defined here: the
inventory system must be complete, correct, and shippable with the entire
`50` range unimplemented.

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_logs` is the
ledger everything here scores from), [`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md)
(storage scoping applies unchanged).

## The problem this solves

Inventory work is tedious and its payoff is deferred: you photograph a
shelf today so that in three weeks you don't buy a fourth jar of paprika.
The chore is immediate, the benefit is invisible, so the data rots — and a
home inventory with rotten data is worse than none, because it is
confidently wrong.

Gamification here has one job: **make the moment of contributing feel
worth it**, so the data stays fresh. It is not about engagement metrics,
retention, or making people open the app more often.

## Scope

Applies to the three moments where a person does inventory work:

- **Adding** — shelf ingestion (`06`), new products via shopping-list
  reconciliation (`07`).
- **Taking** — consumption logging (`09`).
- **Curating** — correcting an AI proposal, filling in a missing category
  or expiry, mapping a new location.

Explicitly out of scope: reporting (`11`) stays a plain analytics view,
and the reorder dashboard (`10`) stays a plain to-do list. Not everything
should be a game.

## Design principles

**1. Reward the behavior you actually want, not activity.**
The naive design — points per item added — pays people to inflate the
inventory, which destroys exactly the data quality the feature exists to
protect. Scoring must therefore reward *correct and complete* data:
confirmed items, corrected AI proposals, consumption logged (which
*reduces* the inventory and must be worth as much as adding), and gaps
filled. See the anti-gaming rules in `51-gamification-scoring.md`.

**2. Never make the chore louder than the app.**
Rewards appear as a toast after an action the user already chose to take,
and a small card on the dashboard. No modals, no interstitials, no
notifications, no "you haven't scanned in 3 days" nagging. If the
gamification layer ever blocks or delays a real task, it is wrong.

**3. Co-op first, competition optional.**
A storage is a household. Ranking family members against each other by
default is a good way to make a chore into a grievance. The default frame
is a **shared storage-level goal**; an individual leaderboard exists only
if a storage explicitly turns it on.

**4. Fully optional, per user.**
A per-user setting disables the whole layer: no XP, no toasts, no cards,
no quests. Turning it off must never disable or degrade any inventory
feature, and must not affect other members of the same storage.

**5. Gentle streaks.**
Streaks are measured in **weeks, not days**. A household does not do
inventory daily, and a daily streak turns a two-day holiday into a
punishment. Missing a week costs the streak; it never costs progress
already earned.

**6. No cross-storage anything.**
No global leaderboards, no "you're in the top 10% of households", no
comparisons that would require knowing other storages exist — that is
forbidden outright by `03-auth-and-multi-tenancy.md`. All scoring,
ranking, and comparison is strictly within one storage.

**7. Nothing is ever taken away.**
No XP decay, no losing levels, no penalties for inactivity. The layer
adds; it never punishes. The one exception is correcting fraud-shaped
data (`51-gamification-scoring.md`), and even that is a recomputation
rather than a punishment.

## What it consists of

| Element | Purpose | Spec |
|---|---|---|
| XP and levels | Immediate acknowledgment that a contribution mattered | `51-gamification-scoring.md` |
| Inventory health score | Turns invisible data quality into a visible progress bar | `51-gamification-scoring.md` |
| Weekly quests | Directs effort at what the data actually needs right now (stale shelves, missing expiry dates) | `52-gamification-quests-and-ui.md` |
| Achievements | One-off recognition of milestones | `52-gamification-quests-and-ui.md` |
| Weekly streak | Light continuity incentive | `52-gamification-quests-and-ui.md` |
| Storage goal | The shared, co-op framing | `52-gamification-quests-and-ui.md` |

## Non-goals

- No virtual currency, no purchasable items, no loot boxes, no gacha.
- No push notifications or email digests.
- No social sharing outside the storage.
- No avatars/pets/farms to maintain — the game is the inventory, not a
  second thing to keep alive.
- No difficulty tuning that makes the app slower to use (e.g. hiding a
  quick action behind an "unlock").
