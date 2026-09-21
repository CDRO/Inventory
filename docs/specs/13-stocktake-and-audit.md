# 13 — Stocktake & Manual Inventory Correction

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_batches`,
`inventory_logs`, `locations`), [`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md) (location tree
UI), [`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(expiry resolution for found stock), [`09-consumption-logging.md`](09-consumption-logging.md)
(the exact-row-set confirm pattern this spec reuses).

## Why this spec exists

`inventory_logs.reason` has allowed `'audit'` since the first migration
(`02-data-model.md`), but no spec has ever defined how an audit row comes to
exist. That is a hole with a predictable consequence: inventory drifts —
a jar breaks, a snack is eaten without a photo, a batch was miscounted at
ingestion — and the system offers no sanctioned way to say "the shelf is
right, the database is wrong." `50-gamification-overview.md` states the
stakes plainly: an inventory with rotten data is worse than none, because
it is confidently wrong.

This spec defines the two correction paths — a **single-batch fix** for the
moment you notice an error, and a **guided stocktake** for walking a whole
location — and makes both honor the ledger invariant: every write to
`inventory_batches.quantity` is paired, in the same transaction, with an
`inventory_logs` row explaining it.

## Single-batch correction

`PATCH /api/storages/{storage_id}/inventory-batches/{id}` (the existing
route from `06-vision-shelf-ingestion.md` /
`08-expiration-and-classification.md`) additionally accepts `{quantity}`:

- `quantity` must be an integer ≥ 0; anything else is `422`.
- The write computes the delta against the batch's current quantity and
  records **one `inventory_logs` row with `reason = 'audit'`** and
  `change_qty` equal to that signed delta, in the same transaction. A
  no-op edit (same quantity) writes nothing — there is no change to
  explain.
- `quantity = 0` deletes the batch row in the same transaction, exactly as
  a consumption reaching zero does (`09-consumption-logging.md`): batches
  do not linger at zero.
- Combining `quantity` with the other patchable fields (`location_id`,
  `expiration_date`, `expiration_source`) in one request is allowed; each
  field keeps its own rules and log semantics (`move` rows for a
  relocation, no log for an expiry edit — dates are not quantities).
  The one exception is `quantity = 0` together with `location_id`, which
  is `422`: "the shelf is empty" and "carry it to the other room"
  contradict each other, the row is deleted either way, and applying them
  in either order gives a different ledger. A caller that means both means
  two requests.

## Manual batch creation — "found stock"

The inverse case: three jars are on the shelf that no flow ever recorded.
Sending the user to photograph them would work, but a person standing in
front of the shelf already knows what they are — the AI adds nothing.

`POST /api/storages/{storage_id}/inventory-batches` — body
`{product_id, location_id, quantity, expiration_date?}`:

- `quantity` ≥ 1; `product_id` and `location_id` must both belong to the
  URL's storage — a foreign id answers `404`, indistinguishable from a
  nonexistent one (`03-auth-and-multi-tenancy.md`).
- `expiration_date` absent → resolved per
  `08-expiration-and-classification.md` and stored with
  `expiration_source = 'derived'`; present (or explicitly `null`) →
  stored with `expiration_source = 'user'`.
- Writes the batch and one `inventory_logs` row with `reason = 'audit'`
  and positive `change_qty`, in one transaction. `'audit'`, not
  `'purchase'`: nothing was bought in this moment — the record is being
  corrected to match reality, and turnover analytics
  (`11-reporting-and-analytics.md`) must not count it as shopping.

## Guided stocktake — walking one location

The two operations above fix errors the user happens to notice. A
stocktake is the systematic version: stand in front of one shelf, compare
it against the database, correct everything in one confirmed pass.

### `locations.last_audited_at`

A migration adds to `locations`:

```sql
last_audited_at TIMESTAMPTZ    -- NULL = never audited
```

Set only by completing a stocktake (below). It is returned in the
location tree response (`06-vision-shelf-ingestion.md`), and
`locations.html` shows it per node via the tree component's
`renderDetail` callback (`05-frontend-pwa-foundations.md`) — "audited 3
weeks ago" / "never audited" — so staleness is visible where the tree is
edited. No notification, no nagging; visibility is the whole feature.
(A later gamification quest may target stale locations — `52-gamification-quests-and-ui.md`
— but nothing here depends on the `50` range.)

### Reading the sheet

`GET /api/storages/{storage_id}/locations/{id}/stocktake` — the batches
**directly at** this location (not descendants — a stocktake mirrors a
physical shelf, and child locations are their own walk):

```json
{
  "location": { "id": "018f…", "name": "Layer 2", "last_audited_at": null },
  "batches": [
    { "id": "018f…", "product_id": "018f…", "product_name": "Barilla Penne 500g",
      "image_url": null, "quantity": 3, "expiration_date": "2027-01-10",
      "expiration_source": "derived" }
  ]
}
```

### Confirming the sheet

`POST /api/storages/{storage_id}/locations/{id}/stocktake` — one
transaction, following the exact-row-set rule from
`09-consumption-logging.md`: the payload **states a decision for every
batch the location currently holds**, plus any found stock.

```json
{
  "batches": [
    { "batch_id": "018f…", "quantity": 2 },
    { "batch_id": "018f…", "quantity": 4 }
  ],
  "found": [
    { "product_id": "018f…", "quantity": 3, "expiration_date": null }
  ]
}
```

- The `batch_id` set must **exactly match** the batches currently at the
  location — a missing or unknown id is `422 validation_failed`, never a
  silent skip. If another member changed the shelf since the sheet was
  fetched, the mismatch surfaces as exactly that error and the user
  re-fetches; last-write-wins guessing is not acceptable for a flow whose
  purpose is correctness.
- Each stated quantity is applied as a single-batch correction (above):
  delta logged with `reason = 'audit'`, zero deletes the batch, an
  unchanged quantity writes nothing.
- Each `found` entry is a manual batch creation (above) at this location.
- `last_audited_at = now()` is set in the same transaction — including
  when nothing changed, because "I checked and it was right" is precisely
  the information the timestamp records.

There is no server-side stocktake session, draft, or partial state: like
a review job, the sheet is either confirmed whole or abandoned by leaving
the page (`09-consumption-logging.md`). The frontend is a plain checklist
on `locations.html` (or a dedicated `stocktake.html?location=…` deep
link), one row per batch with a quantity stepper, an "add item" row using
the product picker, and a single Confirm.

## Not scored

Audit corrections deliberately earn nothing in the gamification layer —
`51-gamification-scoring.md` already excludes `'audit'` rows from scoring.
Paying points per correction would pay people to create errors to fix.
If stocktakes are ever rewarded, it will be as a quest in the `50` range,
scored per completed walk, not per delta.

## Acceptance criteria

- Every quantity change made through this spec produces exactly one
  `inventory_logs` row with `reason = 'audit'` and the correct signed
  delta, in the same transaction as the batch write; an unchanged
  quantity produces no log row.
- Setting a batch's quantity to 0 deletes the batch; the audit log row
  survives with `batch_id` set `NULL` by the FK (`02-data-model.md`).
- Found stock creates a batch with the resolved default expiry
  (`derived`) unless a date (or explicit `null`) was supplied (`user`),
  and never writes a `'purchase'` log row.
- A stocktake confirm whose `batch_id` set does not exactly match the
  location's current batches is rejected with `422` and writes nothing.
- Completing a stocktake sets `last_audited_at` even when no quantity
  changed.
- All ids in every payload are validated against the URL's storage;
  foreign ids yield `404` indistinguishable from nonexistent ones.
- Turnover analytics (`11-reporting-and-analytics.md`) are unaffected by
  audit rows in their purchased/consumed series (audit is neither).
