# 51 — Gamification: Scoring, Data Model & Anti-Gaming

Later phase — see the phase marker in [`50-gamification-overview.md`](50-gamification-overview.md).

Depends on: [`02-data-model.md`](02-data-model.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md).

## Core design: score from the ledger, don't build a second one

`inventory_logs` (`02-data-model.md`) already records every inventory
change with `created_by`, `reason`, `change_qty`, and `timestamp`. That is
an append-only, tamper-resistant, server-written activity ledger — which
is exactly what an XP system needs. **Derive XP from it rather than
emitting a parallel stream of "gamification events" that can drift out of
sync with reality.**

Consequences:

- XP is **recomputable from scratch** at any time. If a rule changes, or
  data is found to be bogus, recompute — no migration of accumulated
  point totals.
- The client never submits XP, never computes XP, and never tells the
  server what it earned. It only reads.
- If a batch is deleted or a log entry is found to be part of a
  correction, the recomputation naturally reflects that.

A small amount of activity is **not** in `inventory_logs` (correcting an
AI proposal, filling in a missing category, mapping a location). For those,
append a `contribution_events` row in the same transaction as the change
itself — same discipline: server-written, append-only, never
client-supplied.

## Tables

All primary keys are UUIDv7, and enum-like columns are `TEXT` with a
`CHECK` rather than `VARCHAR(n)` — see the column-type conventions in
`02-data-model.md`. (No: `VARCHAR(40)` bought nothing here. PostgreSQL
stores both identically, the `CHECK` is the constraint that actually
matters, and the length cap would only need widening the first time a
longer `kind` is added.)

```sql
-- Server-written record of scoreable work that inventory_logs does not capture.
CREATE TABLE contribution_events (
    id         UUID PRIMARY KEY,
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL
               CHECK (kind IN ('ai_correction', 'metadata_filled', 'location_mapped',
                               'category_created', 'expiry_confirmed', 'ambiguity_resolved')),
    ref_id     UUID,          -- the product/location/batch the work applied to
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_contribution_events_storage_user
    ON contribution_events(storage_id, user_id, created_at);

-- Cached rollup. Always reconstructible from the two ledgers above; never authoritative.
CREATE TABLE user_progress (
    storage_id      UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    xp              INT NOT NULL DEFAULT 0,
    level           INT NOT NULL DEFAULT 1,
    streak_weeks    INT NOT NULL DEFAULT 0,
    last_active_week DATE,          -- Monday of the last week with any scored activity
    recomputed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id)
);

CREATE TABLE achievements_unlocked (
    storage_id     UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    achievement_key TEXT NOT NULL,            -- see 52
    unlocked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, user_id, achievement_key)
);

-- Per-user opt-out (principle 4 in 50).
CREATE TABLE user_preferences (
    user_id             UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    gamification_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Weeks a user has marked as holiday; streaks pause rather than break (see 52).
CREATE TABLE holiday_weeks (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    week_start DATE NOT NULL,          -- Monday
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, week_start)
);

-- Per-storage toggle for the optional individual leaderboard (principle 3 in 50).
CREATE TABLE storage_gamification_settings (
    storage_id        UUID PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
    leaderboard_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    weekly_goal_items  INT NOT NULL DEFAULT 20,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`user_progress` is a cache for cheap reads. It is refreshed on write (in
the same transaction that appended the underlying event) and can be fully
rebuilt by `/inventory recompute-progress` — a maintenance subcommand of
the same binary (`01-architecture-and-deployment.md`).

## XP rules

Values live as named constants in `internal/gamification`, not scattered
literals.

| Action | Source | XP |
|---|---|---|
| Item confirmed from a shelf-photo proposal | `inventory_logs.reason = 'vision_ingestion'` | 3 per distinct product (**not** per unit) |
| Item added via shopping-list resolution | `reason = 'purchase'` | 3 per distinct product |
| Consumption logged | `reason = 'consumption'` | **3 per distinct product** — equal to adding, deliberately |
| AI proposal corrected before confirming | `contribution_events.ai_correction` | 5 |
| Ambiguous shopping-list line resolved | `ambiguity_resolved` | 4 |
| Missing metadata filled (category, image, min_stock) | `metadata_filled` | 2 |
| Expiry date confirmed or corrected on a batch | `expiry_confirmed` | 2 |
| New location mapped | `location_mapped` | 5 |
| Weekly quest completed | see `52` | 15–40 |

Two deliberate choices:

- **Per distinct product, never per unit.** Adding 24 identical cans is
  one contribution, not 24. Paying per unit would reward inflating
  quantities.
- **Correcting the AI is worth more than accepting it** (5 vs 3). The
  correction is the scarce, valuable act: it is what keeps the data
  truthful, and it is the least fun part of the job.

### Levels

`level = floor(sqrt(xp / 50)) + 1` — fast early levels for a new user's
first session, slowing steadily. No level cap, no prestige, no decay.

## Inventory health score

A storage-level percentage, shown as a progress bar. It makes invisible
data quality visible, and gives quests something concrete to target.

Computed as the mean of five sub-scores, each 0–100% across the storage's
products:

1. Products with a `category_id` set.
2. Products with an image or icon.
3. Products with `min_stock` set (> 0) — i.e. actually tracked for reorder.
4. Perishable/long-shelf-life batches carrying an `expiration_date`.
5. Products touched (any `inventory_logs` row) within the last 180 days —
   the staleness check.

Health is a **storage** metric, never a per-user one: nobody should be
blamed for the household's backlog.

## Anti-gaming rules

The failure mode to defend against is not a malicious attacker — it is a
well-meaning person nudged into making the data worse for points. Every
rule below exists to remove that nudge.

- **Server-side only.** XP is computed on the server from server-written
  ledgers. There is no client-supplied XP, and no endpoint that accepts
  points, levels, or achievements as input.
- **Create-delete cycles earn nothing.** XP for adding a product is
  awarded once per `(user, product)` — recomputation deduplicates on the
  product id, so deleting and re-adding the same product does not pay
  twice. Deleting a product removes its contribution on the next
  recompute.
- **No XP cap, ever.** There is deliberately no daily or per-action
  ceiling on base XP. The single most valuable thing a user can do is the
  first-time marathon — spending a Saturday indexing a cellar nobody has
  touched in years — and a cap would punish exactly that, or worse, teach
  someone to spread real work across days to farm the limit. Diminishing
  returns is a retention mechanic, not an incentive; it has no place here.
  Caps exist only inside weekly quests, which are naturally bounded by
  their own target counts (`52-gamification-quests-and-ui.md`).
- **Quantity is not scored.** See "per distinct product" above.
- **Consumption pays as much as addition**, so there is no incentive to
  hoard entries or to avoid logging things being used up.
- **Same product, same session, one contribution — the 2-hour coalescing
  window.** Repeatedly adding or removing units of the same product does
  not multiply XP: events for one `(user, product, kind)` are grouped into
  a contribution window, and any two events less than **2 hours** apart
  fall into the same window, which scores once. Taking three yoghurts out
  one at a time over a morning is one act of logging, and is paid as one.
  A genuinely separate act — the same product again the next evening —
  opens a new window and scores again, correctly.
- **Bulk-ingesting one photo repeatedly earns once.** Confirming a job is
  scored per job (`jobs.status = 'consumed'`), and a job can only be
  consumed once (`04-backend-api-conventions.md`).
- **Recompute, don't punish.** Nothing is penalized, and no XP is
  confiscated as a sanction; miscounts are fixed by recomputing from the
  ledgers rather than by a penalty mechanic. What "fixing the data" means
  differs by ledger, and the distinction matters:
  - `inventory_logs` is **append-only and never deleted**
    (`02-data-model.md`). A wrong quantity is corrected by a new
    compensating row with `reason = 'audit'`, and the recompute simply
    sees the corrected net history.
  - `contribution_events` **may be deleted** — by an admin, and only for
    rows recorded in error (a double-counted correction, events from a
    bug). It is a scoring ledger, not an audit trail, so removing a
    bogus row is the right fix, and the next recompute reflects it. This
    is the one deletable ledger in the system, stated explicitly so it is
    not confused with the inventory audit trail.

## Nightly reconciliation

Progress is recomputed **once a day** (03:00, server-local), rebuilding
`user_progress` from `inventory_logs` and `contribution_events` with the
coalescing rules above applied over the completed day.

Intra-day XP shown after an action is provisional and optimistic — it
assumes each contribution is new. The nightly pass consolidates windows
that the live path counted separately, so a total may settle *slightly
lower* the next morning. That is the only direction it moves, and it is
never framed as a loss: the UI shows a level and a total, not an
audit trail of adjustments. Anything the reconciliation cannot justify
from the ledgers simply does not exist in the new total.

Running it nightly rather than continuously also keeps the write path
cheap: the live update is a single increment, and correctness is the
batch job's problem.

## API

Storage-scoped routes are behind `RequireStorageMember`
(`04-backend-api-conventions.md`); the `/api/me/*` routes need only a
session:

| Route | Returns |
|---|---|
| `GET /api/storages/{storage_id}/progress` | Caller's XP, level, streak **in this storage**, plus the storage health score |
| `GET /api/storages/{storage_id}/progress/leaderboard` | Per-member XP — **`404` unless `leaderboard_enabled`** for that storage |
| `GET`/`PUT /api/storages/{storage_id}/gamification/settings` | Storage-level toggles (any member may change them; rights are flat per `03`) |
| `GET /api/me/progress` | The caller's **overall** progress across every storage they belong to |
| `GET`/`PUT /api/me/preferences` | The caller's own `gamification_enabled` flag and holiday weeks |

### Per-storage score vs. overall score

The dashboard, quests, leaderboard, and health bar are always about **the
currently selected storage** — mixing households into one number would
make the storage goal meaningless.

But a person is one person: someone who keeps a flat, a cellar, and a
holiday house should see what they have done in total. `GET /api/me/progress`
returns that, aggregated over the caller's own memberships only:

```json
{
  "total_xp": 4820,
  "overall_level": 10,
  "longest_streak_weeks": 14,
  "per_storage": [
    { "storage_id": "018f...", "name": "Home", "xp": 3900, "level": 9, "streak_weeks": 14 },
    { "storage_id": "018f...", "name": "Cellar", "xp": 920, "level": 5, "streak_weeks": 2 }
  ]
}
```

- `total_xp` is the sum over the caller's storages; `overall_level` is
  derived from it with the same formula, so it is not the sum of the
  per-storage levels.
- This does **not** breach the non-enumeration rules in
  `03-auth-and-multi-tenancy.md`: it lists only storages the caller is
  already a member of and could already see in `GET /api/auth/me`. It
  never reveals a storage they lack access to, and never compares them to
  anyone outside a storage.
- It is surfaced in the user's own profile page, not on any storage
  dashboard, so the two numbers can never be mistaken for each other.

When the caller has `gamification_enabled = FALSE`, all progress endpoints
return `204 No Content` and the frontend renders nothing — no empty
placeholder cards.
