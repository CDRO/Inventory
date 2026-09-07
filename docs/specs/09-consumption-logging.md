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
  knows its direction without re-asking.
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

### Rejecting and correcting

Vision models hallucinate items and misread labels, so disagreeing with a
proposal must be as easy as accepting it. Every row therefore offers
**three** actions, not two — provided by the shared review component
(`05-frontend-pwa-foundations.md`) and identical in `06`, `07`, and here:

**1. Accept** — with any edits to product, quantity, location, or expiry.

**2. Correct manually** — *"the AI got this wrong, here is what it
actually is."* This is the right action for a misidentification, and
usually a better one than rejecting: the photo and the count were fine,
only the identification failed, so throwing the whole row away discards
work that was correct.

- Pick an existing product by autocomplete (the only option here in `09`,
  since consumption never creates products), or — in the flows that can
  create products (`06`, `07`, `10`) — type a name and create one.
- A typed name still runs matching stages 1 and 2 (local products, then
  the anonymous catalog) because those are free and local, but **stage 3
  never re-invokes Gemini for that row**.
- The product image can come from the **photo the user already took**:
  offer the item's own crop (from `bounding_box`) or the whole
  single-product image as a one-tap choice, alongside the usual
  suggestions and a custom upload.

#### Optional: background removal on a user photo

A shelf crop has a cluttered background, which makes it a poor product
thumbnail next to clean provider images. Offering to strip that background
is worthwhile — with two constraints that shape how it is specified:

- **It needs a different model.** The configured `GEMINI_MODEL`
  (`01-architecture-and-deployment.md`) is a vision *analysis* model: it
  reads images and returns text, and cannot return an edited image.
  Background removal requires a Gemini **image-generation/editing** model,
  configured separately as `GEMINI_IMAGE_MODEL`. If that variable is
  unset, or the model is unavailable, **the feature simply does not
  appear** — no error, no degraded placeholder.
- **An image model regenerates rather than masks.** It can subtly alter
  the product itself, including inventing plausible-looking label text.
  For an image whose only job is helping a human recognize a jar on a
  shelf that is acceptable, but it must never happen invisibly.

Therefore:

- The offer appears **after** the user picks their own photo, as a
  suggestion — never automatic, never applied to provider images, never
  blocking the flow.
- The result is shown **side by side with the original**, and the user
  chooses which to keep. The original remains selected until they actively
  pick the processed version.
- If the call fails, times out, or returns something unusable, the
  original is kept silently. This is a cosmetic nicety; it must never cost
  someone their photo or their place in the flow.
- The kept image is stored locally like any other user photo, and — like
  any user photo — is never published to the catalog
  (`02-data-model.md`).
- Model availability is handled by the same resilience mechanism as the
  analysis model (`01-architecture-and-deployment.md`): a deprecated
  `GEMINI_IMAGE_MODEL` disables the offer and surfaces in the admin
  banner, rather than failing a review.

**Manual correction is terminal — the AI is never retried automatically.**
Once a person has said what an item is, the system does not second-guess
them, re-analyze the row, or overwrite their label on a later pass.
Re-analysis exists only as an explicit action on the whole job
("Analyze again"), never as an automatic retry. This matters most for
exactly the case where vision keeps failing: niche, regional, homemade, or
unlabeled products, where the model will not get it right on the second
attempt either, and where the user's own photo *is* the best possible
product image.

**3. Reject** — *"this is not here, write nothing."* Rejected rows stay
visible but struck through and greyed, so the reviewer can see what was
proposed and undo a mis-tap rather than having items silently vanish.
Nothing about a rejected row is written to inventory.

### How a rejected row leaves the review cycle

Nothing can get stuck, because **the unit of review is the job, not the
row.** A proposal exists only inside its job's `payload`
(`04-backend-api-conventions.md`); there are no per-item review records to
strand. When the job is confirmed it becomes `consumed` and disappears
from the inbox — whether every row was accepted, every row was rejected,
or anything in between.

To make that airtight rather than merely implied, the confirm payload
**states a decision for every row**, and rejections are explicit rather
than silent omissions:

```json
{
  "items": [
    { "row_id": "0", "decision": "accept", "product_id": "018f…", "decrements": [ … ] },
    { "row_id": "1", "decision": "reject" }
  ]
}
```

- The server validates that the set of `row_id`s **exactly matches** the
  proposal it issued — no missing rows, no unknown ones. A missing row is
  `422 validation_failed`, not an implicit rejection. A silently dropped
  row would otherwise be indistinguishable from a client bug, and would be
  the one way an item could quietly vanish without a decision.
- `decision: "reject"` writes nothing: no batch, no `inventory_logs` row,
  no product.
- **A job is never partially confirmed.** There is no state in which some
  rows are done and others still await review. Either the job is confirmed
  (all rows decided, job `consumed`) or it is untouched and still waiting.
  A reviewer who does not want to decide yet simply leaves the page —
  nothing is written and the job stays in the inbox exactly as it was.
- The only two exits from the inbox are therefore **confirm** and
  **discard**, and both are explicit user actions.

Beyond the row level:

- **Whole proposal — "Discard".** Calls
  `DELETE /api/storages/{storage_id}/jobs/{job_id}`
  (`04-backend-api-conventions.md`), dropping the proposal and its photo.
  It asks for confirmation once, because the photo goes with it.
- **Rejecting every row and confirming is equivalent to discarding**,
  except that discard also deletes the photo: the confirm writes nothing
  and marks the job `consumed`, so it leaves the inbox either way.
- An "unrecognized" item must be corrected manually or rejected;
  confirming with an unresolved row is a `422 validation_failed`, never a
  silently skipped line.
- Rejection is not scored, and carries no penalty
  (`51-gamification-scoring.md`); manual correction is scored **more**
  than acceptance, because it is the act that keeps the data truthful.
- **A user-supplied photo never leaves the storage.** A manually chosen
  product name may be inserted into the global catalog, but a photo taken
  inside someone's home must not be: see the image rule in
  `02-data-model.md`.

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
- A confirm whose `row_id` set does not exactly match the issued proposal
  is rejected with `422`; no row can be dropped silently.
- Confirming leaves the job `consumed` and out of the inbox regardless of
  how many rows were rejected — no item and no job can remain stuck in
  review.
- Every stored image is free of EXIF and other embedded metadata, and
  phone photos taken in portrait still display upright
  (`04-backend-api-conventions.md`).
- With `GEMINI_IMAGE_MODEL` unset, no background-removal control is
  rendered anywhere and every other flow behaves identically.
