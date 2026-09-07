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
| Consumption hygiene | No `consumption` logs in 14 days — **suppressed while any member is in holiday mode** | "Log something you've used up" | 15 |
| First mile *(new storages)* | Storage has < 10 products | "Map your first shelf" | 40 |

If fewer than three generators fire, offer fewer. Do **not** pad with
filler quests — a quest that exists only to be a quest teaches people to
ignore quests.

### When there are no quests: the "All clear" state

Zero quests means the storage is in good order, and it must **read** that
way. The card shows an all-clear state — "Everything's in order. 12 weeks
running." — with the clean-streak counter, never an empty list, a "no
quests available" notice, or anything that looks like the feature is
broken or that the user has nothing to be proud of.

### Coming back from a quiet stretch

A user who saw "all clear" for six weeks will have stopped looking. When
generation produces quests again after a gap, they must find out, gently
and without a notification (principle 2 in `50-gamification-overview.md`
rules out push and email):

- The dashboard card carries a small "new this week" dot for the first
  seven days after quests reappear following a gap of two weeks or more.
- The same badge mechanism as the review inbox
  (`06-vision-shelf-ingestion.md`) shows a count next to the storage
  switcher — one shared, quiet badge convention rather than a second
  attention-grabbing pattern.
- The badge clears on view, not on completion. Being told is not a task.
- Nothing blocks, interrupts, or reorders the page. If the user ignores
  it, nothing bad happens and the quests simply expire.

### Clean-storage milestones (hidden quests)

Keeping a storage spotless produces no quests, and therefore no quest XP —
which would perversely mean that the best-kept storage earns least. Hidden
milestones fix that.

`clean_since` tracks the date from which **no generator has fired at any
weekly generation** (i.e. every quest condition was already satisfied). It
resets to `NULL` the moment any generator fires.

| Milestone | Consecutive clean days | XP, to **every member** of the storage |
|---|---|---|
| `immaculate_90` | 90 | 200 |
| `immaculate_180` | 180 | 500 |
| `immaculate_365` | 365 | 1200 |
| `immaculate_730` | 730 | 3000 |
| `immaculate_year_N` | every further 365 days | 3000 |

- These are **hidden**: not listed, not previewed, not progress-barred.
  They arrive as a surprise, which is what makes them feel like a reward
  rather than another chore with a deadline.
- The XP is awarded **to every member of the storage**, including members
  who contributed little that period. This is the one deliberate exception
  to "reward the contributor": a spotless storage is a household-level
  achievement, and the point is to make maintaining it feel collectively
  worthwhile.
- Unlocking one is announced in the dashboard card and a single toast on
  next visit — never a notification.

### Rules

- Quests expire at the end of the week and are replaced. Uncompleted
  quests cost nothing.
- Progress is evaluated server-side from the same ledgers as XP
  (`51-gamification-scoring.md`); a quest completes the moment its
  underlying condition is satisfied, whoever in the storage did the work.
- Quests are **storage-level and co-op**: any member's contribution counts
  toward the same quest.
- **XP attribution: every member who contributed at least one qualifying
  action receives the full quest reward.** Not split, not awarded only to
  whoever happened to finish it, and not given to members who did nothing.
  - Splitting a fixed pot would make helping someone else's quest lower
    your own share — the exact opposite of co-op.
  - Paying only the finisher rewards sniping the last item, and makes
    doing the first four-fifths of the work worthless.
  - Paying every member regardless of participation makes contributing
    optional, and quietly insults whoever did the work.
  - A "qualifying action" is any event the quest's progress counter
    counted. One item is enough — the incentive is to join in, not to
    compete for volume.
  - The reward is granted at completion, to each qualifying contributor,
    once per quest.

```sql
CREATE TABLE quests (
    id           UUID PRIMARY KEY,
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    week_start   DATE NOT NULL,           -- Monday
    generator    TEXT NOT NULL,
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
| `steady_hand_13` | A 13-week (3-month) streak |
| `steady_hand_26` | A 26-week (6-month) streak |
| `steady_hand_39` | A 39-week (9-month) streak |
| `steady_hand_52` | A 52-week (12-month) streak |
| `spring_clean` | Zero uncategorized products, **in a storage with at least 100 categorized products** |

The streak tiers are cumulative milestones on one counter, not five
separate mechanics: a user passing 52 weeks has already collected the four
below it. Holiday weeks (below) neither advance nor break a streak, so a
52-week streak taken with the full holiday budget spans up to 60 calendar
weeks — which is the intended, humane behavior.

`spring_clean` requires a **threshold of 100 categorized products** in
addition to zero uncategorized ones. Without it the achievement is
trivial: a brand-new storage with one product, categorized, would unlock
"clean up the whole catalog". With it, the 101st categorized product in a
fully-sorted storage is what earns it — real work, fairly recognized.

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

### Holiday mode

A user can mark weeks as holiday (`holiday_weeks`,
`51-gamification-scoring.md`). A holiday week **pauses** the streak: it
neither continues nor breaks it. A 9-week streak, three holiday weeks, and
then another active week yields a 10-week streak.

Rules, which exist so a pause stays a pause and not a permanent shield:

- **Granularity is one whole week.** Holiday is marked per Monday-start
  week; there are no partial weeks.
- **Budget: at most 8 weeks in any rolling 52-week window**, in any
  pattern — one long trip, eight scattered weeks, or anything between.
  Attempting to exceed it is rejected with `409 conflict` stating how many
  weeks remain in the window.
- **Only the current week and future weeks may be marked.** Retroactively
  marking a week already lost would turn holiday mode into an undo button
  for broken streaks, which is precisely the abuse the budget is meant to
  prevent. A user who forgot to set it before leaving loses the streak —
  and loses nothing else, because streaks carry no XP.
- **Un-marking a future week refunds the budget**; un-marking the current
  or a past week does not.
- **Activity during a holiday week still earns full XP** and still counts
  toward quests, achievements, and the storage goal. It simply does not
  count toward the streak, and — importantly — **does not cancel the
  holiday**. Logging one item from a hotel room must not silently end the
  pause and re-arm the streak for the following week.
- Holiday is **per user, across all their storages**: the person is away,
  not the household. Other members' streaks are unaffected.
- The only storage-level effect is that the "consumption hygiene" quest
  generator is suppressed while any member is on holiday, so nobody comes
  home to a quest scolding them for an empty fridge log.
- Marking holiday is done in the user's own settings, alongside the
  gamification toggle. It is never suggested, prompted, or advertised —
  it is there for whoever needs it.

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
linking to the dashboard card.

Holding the ring reveals the exact progress toward the next level as a
popover: `Progress to level 7: 12/250 XP` — the raw numbers behind the
ring's arc, since an arc alone never answers "how much more?".

- **Long press** (~500ms) on touch, **hover** on pointer devices, and
  **focus** for keyboard users — the same popover, three ways in, so the
  information is not touch-only.
- It is a popover, not navigation: tapping the ring still goes to the
  dashboard card, and the long press must not fire the link on release.
- Content is rendered from the caller's `progress` response; XP values for
  the current level threshold and the next come from the level formula in
  `51-gamification-scoring.md`, computed server-side and returned
  alongside the level so the frontend never re-implements the curve.

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
