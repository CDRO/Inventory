# 52 — Gamification: Quests, Achievements & UI Integration

Later phase — see the phase marker in [`50-gamification-overview.md`](50-gamification-overview.md).

Depends on: [`51-gamification-scoring.md`](51-gamification-scoring.md),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md).

## Weekly quests

Quests are the mechanism that points effort at what the data actually
needs. They are **generated from the storage's real state**, not drawn
from a random pool — a quest to photograph the basement shelf is only
offered if that shelf is genuinely stale.

### Generation

Three quests per storage per week, generated Monday 00:00 in the server's
local timezone, chosen by evaluating candidate generators against live
data and picking the three with the highest "need" score:

| Generator | Fires when | Example rendered text | XP |
|---|---|---|---|
| Stale location | A location has no `inventory_logs` activity in 90+ days | "Re-scan *Basement → Right Shelf* — untouched since June" | 40 |
| Missing expiry | ≥ 5 perishable batches have `expiration_date IS NULL` | "Add expiry dates to 5 fresh items" | 25 |
| Uncategorized | ≥ 5 products have `category_id IS NULL` | "Sort 5 products into categories" | 20 |
| Imageless | ≥ 5 products have neither image nor icon | "Give 5 products a picture" | 20 |
| Untracked reorder | ≥ 5 products have `min_stock = 0` | "Set minimum stock for 5 staples" | 20 |
| Expiring soon | Items are in the `critical` urgency band (`08`) | "Use up or discard 3 items expiring this week" | 30 |
| Consumption hygiene | No `consumption` logs in 14 days | "Log something you've used up" | 15 |
| First mile *(new storages)* | Storage has < 10 products | "Map your first shelf" | 40 |

If fewer than three generators fire, offer fewer. Do **not** pad with
filler quests — a quest that exists only to be a quest teaches people to
ignore quests.

### Rules

- Quests expire at the end of the week and are replaced. Uncompleted
  quests cost nothing.
- Progress is evaluated server-side from the same ledgers as XP
  (`51-gamification-scoring.md`); a quest completes the moment its
  underlying condition is satisfied, whoever in the storage did the work.
- Quests are **storage-level and co-op**: any member's contribution counts
  toward the same quest. The XP is awarded to whoever did the qualifying
  work.

```sql
CREATE TABLE quests (
    id           UUID PRIMARY KEY,
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    week_start   DATE NOT NULL,           -- Monday
    generator    VARCHAR(40) NOT NULL,
    params       JSONB NOT NULL,          -- target ids/counts resolved at generation time
    target_count INT NOT NULL,
    xp_reward    INT NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE (storage_id, week_start, generator)
);
```

Rendered quest text is built in the frontend from `generator` + `params`,
so wording can change without a migration.

## Achievements

One-off milestones, unlocked once per user per storage
(`achievements_unlocked` in `51-gamification-scoring.md`). They mark
genuine firsts and thresholds, and never expire.

| Key | Unlocked by |
|---|---|
| `first_shelf` | Confirm your first shelf-photo ingestion |
| `cartographer` | Map a location tree 3 levels deep |
| `curator` | Correct 25 AI proposals |
| `librarian` | Storage health score reaches 80% |
| `archivist` | Storage health score reaches 95% |
| `zero_waste_week` | A full week with no item passing its expiry date unconsumed |
| `well_stocked` | No product below `min_stock` for 7 consecutive days |
| `deep_freeze` | 100 distinct products in the storage |
| `steady_hand` | A 4-week streak |
| `spring_clean` | Resolve every uncategorized product in one week |

Achievement definitions live in Go as a table-driven list in
`internal/gamification`, each with a predicate evaluated after the events
that could plausibly satisfy it — never on a timer polling everything.

## Streaks

A week counts toward the streak if the user recorded **any** scored
contribution in it (`51-gamification-scoring.md`). Weeks run Monday to
Sunday. Missing a week resets `streak_weeks` to 0 but never removes XP,
levels, or achievements. There is no daily streak, and no notification
warning that a streak is at risk — that is the nagging pattern principle 2
of `50-gamification-overview.md` rules out.

## Storage goal (the co-op frame)

The default shared objective: `storage_gamification_settings.weekly_goal_items`
contributions this week from all members combined, shown as one bar on the
dashboard with a plain caption ("14 of 20 this week"). It is a household
bar, not a ranking. The optional individual leaderboard
(`leaderboard_enabled`, default **off**) adds a simple per-member XP list
in the same card when a storage turns it on.

## UI integration

The layer is additive and quiet. It appears in exactly four places:

**1. Post-action toast.** After confirming an ingestion, a consumption, or
a shopping-list resolution, a small non-blocking toast appears in the
corner: `+12 XP · 4 items mapped`, with a quest line if one advanced
(`Quest: Re-scan Basement — 3/5`). It auto-dismisses, never steals focus,
and never delays navigation.

**2. Dashboard card.** One card on `dashboard.html`, below the reorder and
analytics widgets from `10`/`11`: level and XP ring, the storage health
bar, the weekly goal bar, this week's quests with progress, and the
current streak. Recently unlocked achievements appear here for a week.

**3. Header ring.** A small level ring next to the storage switcher,
linking to the dashboard card. Nothing else.

**4. Settings.** A single toggle in the user's own settings for
`gamification_enabled`, and the storage-level toggles
(`leaderboard_enabled`, `weekly_goal_items`) alongside them.

### Frontend implementation notes

Per `05-frontend-pwa-foundations.md`: vanilla JS ES modules, no framework.
Add `js/gamification.js` (fetches progress, renders the card and toast)
and `css/gamification.css` using the existing tokens — no new color system.
Rings and bars are hand-written inline SVG/CSS; **do not** add a charting
or animation library for them. All rendering uses `textContent` for any
value that originated in user or AI input.

When `GET /api/storages/{id}/progress` returns `204` (gamification off for
this user), `js/gamification.js` renders nothing at all and the dashboard
reflows without a gap — no disabled placeholder, no "turn this on!"
upsell.

## Acceptance criteria

- With `gamification_enabled = FALSE`, no gamification request is made, no
  element is rendered, and every inventory feature behaves identically.
- No gamification endpoint accepts XP, level, streak, or achievement state
  as input.
- Adding 24 units of one product scores the same as adding 1 unit of it.
- Deleting and re-adding the same product does not increase XP after
  recomputation.
- No quest, achievement, leaderboard, or comparison references data
  outside the current storage (`03-auth-and-multi-tenancy.md`).
- A user who never opens the dashboard still experiences the full
  inventory system with no missing functionality.
