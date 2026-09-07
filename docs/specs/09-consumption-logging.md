# 09 — Quick Consumption Logging

Implements PRD Feature 5. Depends on: [`02-data-model.md`](02-data-model.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (upload +
job pattern), [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(reuses the same Gemini-call + matching-service approach),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(shared review component, job polling).

## Goal

User photographs an item they just consumed/used up (e.g. an empty toilet
paper roll). The system identifies the product and proposes a quantity
decrement; the user can override the count before confirming.

## Capture mode — decided before the photo, never after

Neither a separate view per action nor a per-photo question. Both are
wrong for the actual usage pattern: work comes in **runs** — you unpack a
shopping bag (all additions), or you clear out the fridge (all
consumption). Asking "was this in or out?" after every shot taxes the
frequent case to serve the rare one, and splitting the app into
disconnected views makes the phone camera harder to reach.

Instead there is **one camera entry point with a sticky mode selector**,
shown as a segmented control above the shutter:

| Mode | Photo means | Endpoint | Sign |
|---|---|---|---|
| **Stocking up** | "I am adding this" | `/ingest/product-photos` (`06`) | `+` |
| **Using up** | "I am removing this" | `/consume/photos` | `−` |
| **Shelf scan** | "Index everything visible" | `/ingest/shelf-photos` (`06`) | `+` |

- The mode is chosen **before** capture and **persists** across photos,
  across pages, and across sessions (`localStorage`), defaulting to the
  last one used. A run of twenty consumption photos asks zero questions —
  this is the "inventorizing mode" that suppresses the in/out prompt.
- The current mode is always visible while the camera is open, and is
  stated again on the resulting review screen ("3 items · using up"), so a
  mistakenly-left mode is caught before anything is written — the review
  step (`06-vision-shelf-ingestion.md`) is the safety net, and it exists
  regardless.
- The mode is stored on the job, so a proposal reviewed days later still
  knows its direction without re-asking. @claude: where in the review mode do we find the "reject" option? If none-existant, extend spec accordingly.
- Switching mode on a review screen is allowed but explicit: it discards
  the proposal and re-queues the photo under the other mode, rather than
  silently flipping the sign of a reviewed list.

## Upload flow

1. `POST /api/storages/{storage_id}/consume/photos` — multipart upload of
   one photo. Returns `202 {job_id}`, same background-job pattern as
   `06-vision-shelf-ingestion.md`.
2. Backend sends the image to Gemini with a consumption-focused prompt:
   identify the product and how many units are depicted (e.g. "1 empty
   roll" → `detected_quantity: 1`). Response schema:

```json
{
  "items": [
    { "label": "Toilet Paper Roll", "detected_quantity": 1, "confidence": 0.88 }
  ]
}
```

3. Each `label` is resolved against existing `products` via
   `internal/matching` from `07-shopping-list-reconciliation.md` (reused,
   not reimplemented), **stage 1 only** — consumption logging matches
   existing storage-local products and never creates new ones, so the
   catalog and external stages do not apply. An unmatched item is shown in
   review as "unrecognized" with a manual product picker, never silently
   dropped.

## Review UI (delta proposal)

For each detected item: matched product name (with a manual
override/search picker if the match was wrong or unrecognized), the
AI-detected decrement count (pre-filled, e.g. `-1`), and **an editable
count** — the PRD's explicit example: photo shows 1 roll, user changes the
decrement to `-3` because they're logging a multi-day gap in one shot.
The count is presented and edited as a positive "how many did you use"
number; the sign is applied server-side.

The reviewer must also pick **which existing batch(es)** the decrement
applies to when a product has multiple batches across locations (e.g.
partial consumption from a specific shelf vs. the fridge). Default to the
batch with the nearest `expiration_date` (typically the intended
first-out one) but let the user reassign to a different batch or split
across batches if the total exceeds one batch's quantity.

## Confirm endpoint

`POST /api/storages/{storage_id}/consume/photos/{job_id}/confirm` — body:
edited list of `{product_id, decrements: [{batch_id, quantity}]}`. In one
transaction per confirm call:

1. For each `(batch_id, quantity)`: `quantity` must not exceed the
   batch's current `quantity` — reject with `422` otherwise (the review UI
   should already prevent this client-side, but the backend re-validates).
2. Decrement `inventory_batches.quantity`; if it reaches exactly `0`,
   delete the batch row (batches don't linger at zero — contrast with
   products, which can validly sit at zero total stock, tracked via
   `min_stock`/reorder in `10-reorder-and-shopping-export.md`).
3. Write one `inventory_logs` row per batch touched, `reason='consumption'`,
   `change_qty` negative.
4. Mark the job `consumed` (`04-backend-api-conventions.md`) so the same
   decrement cannot be applied twice.

## Acceptance criteria

- The AI-detected count is always a starting suggestion, never applied
  without passing through the editable review step.
- Confirming a decrement that would take a batch below zero is rejected,
  not clamped silently — surface the error so the user corrects the split.
- A product with zero total remaining stock after consumption is not
  deleted; it remains browsable at "0 in stock", feeding the reorder
  dashboard in `10-reorder-and-shopping-export.md`.
