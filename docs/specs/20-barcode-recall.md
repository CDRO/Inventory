# 20 — Barcode Recall

Depends on: [`02-data-model.md`](02-data-model.md) (`products`,
`catalog_products` and its privacy invariants),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(single-product photo path this feature falls back to),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md),
[`09-consumption-logging.md`](09-consumption-logging.md) (capture modes,
batch decrement semantics),
[`10-reorder-and-shopping-export.md`](10-reorder-and-shopping-export.md).

## Why this spec exists — and what changed in `00-overview.md`

`00-overview.md` originally ruled out barcode scanning outright, and the
Android spike (`docs/spikes/21-native-android-app.md`) kept barcode
contained pending an explicit decision. **That decision was made by the
project owner on 2026-09-20**, and it is narrower than the thing the
non-goal feared:

- **Still a non-goal:** consulting external UPC/EAN databases to
  identify unknown products. Identification of something the system has
  never seen remains vision-LLM + matching, exactly as before. No new
  external dependency, no barcode-DB API key.
- **Now accepted:** a barcode as a **recall key**. Once a product has
  been identified — by vision, catalog, or hand — its barcode can be
  attached to it, and every later scan of that code resolves the
  product instantly: no Gemini call, no image search, no matching
  ambiguity, no cost. The second scan of anything is free.

This is the cheapest identification path the system can have, it works
in the PWA (no native app required — the spike's own analysis says so),
and it strengthens rather than weakens the vision-first design: vision
does the one expensive first identification, the barcode makes every
repeat free.

## Schema

```sql
CREATE TABLE product_barcodes (
    storage_id UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    barcode    VARCHAR(64) NOT NULL,      -- raw decoded string, e.g. EAN-13 digits
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (storage_id, barcode)
);

CREATE INDEX idx_product_barcodes_product_id ON product_barcodes(product_id);
```

- One barcode maps to **one product per storage** (the PK); one product
  may carry many barcodes (multipack vs. single, relabeled imports).
- `storage_id` is denormalized onto the row to make the per-storage
  uniqueness a database constraint; application code must verify it
  matches the product's storage, like every same-storage rule.
- The stored value is the raw decoded string: 1–64 characters,
  restricted to `[0-9A-Za-z._/-]` (`422` otherwise). No checksum
  validation — the scanner already did that, and a hand-typed code the
  user reads off a label is valid input even when unusual.

### Global recall hints: `catalog_barcodes`

The catalog exists so a product one household described costs the next
household nothing (`02-data-model.md`). A barcode is the strongest
possible catalog key — it is printed on the product and identical in
every kitchen — so the same idea applies:

```sql
CREATE TABLE catalog_barcodes (
    barcode    VARCHAR(64) PRIMARY KEY,
    catalog_id UUID NOT NULL REFERENCES catalog_products(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- **Insert-only**, `ON CONFLICT DO NOTHING`, for exactly the reasons
  `catalog_products` is (`02-data-model.md`): a rewritable global row is
  a channel and a poisoning target. The first mapping wins; a wrong
  first mapping is removed by an admin
  (`DELETE /api/admin/catalog-barcodes/{barcode}`, joining the catalog
  moderation routes in `03-auth-and-multi-tenancy.md`'s table), after
  which the next association may re-insert.
- Carries **no storage reference, no counts, no timestamps in
  responses** — all `catalog_products` privacy invariants apply. A
  lookup hit reveals that a product with this code exists in the world,
  never that another storage exists.
- Populated as a side effect of association (below) when the product
  has a `catalog_id`; never user-editable, never exposed as a list.

## Associating a barcode with a product

`POST /api/storages/{storage_id}/products/{id}/barcodes` — body
`{barcode}`:

- Product must be in the URL's storage (`404` otherwise). A code
  already mapped to another product in this storage is `409 conflict`
  naming nothing about other storages (there is nothing to name — the
  conflict is local by construction).
- In the same transaction, when the product has a `catalog_id`, insert
  `(barcode, catalog_id)` into `catalog_barcodes`
  (`ON CONFLICT DO NOTHING`).

`DELETE /api/storages/{storage_id}/products/{id}/barcodes/{barcode}`
removes a local association; the catalog hint is untouched (it is a
one-shot claim, like a catalog name).

Association points in the UI — wherever a product is already in hand:

- The product edit screen (`16-product-maintenance.md`): a barcodes
  list with add-by-scan and add-by-typing.
- The review screens (`06`, `09`): after a product-photo identification
  is confirmed, offer a one-tap "scan its barcode so next time is
  instant" step. Optional, skippable, never blocking the confirm.

## Recall lookup

`GET /api/storages/{storage_id}/barcodes/{code}`:

1. **Local hit** (`product_barcodes` in this storage) →
   `200 {"product": {…, "current_stock": n}}` — the full product shape
   from `10-reorder-and-shopping-export.md`.
2. **Catalog hit** (`catalog_barcodes`) → `200 {"catalog_suggestion":
   {display_name, category_path, item_type, image_url, icon_name,
   default_shelf_life_days}}` — exactly the stage-2 card shape from
   `07-shopping-list-reconciliation.md`, exposing only display fields.
   Accepting it creates the storage-local product the same way a
   shopping-list catalog card does, then associates the scanned code
   with the new product.
3. **Miss** → `404 not_found`, indistinguishable from any other. The UI
   treats it as "unknown product": offer the single-product photo path
   (`06-vision-shelf-ingestion.md`), and after that identification is
   confirmed, offer to attach the scanned code (its value is kept
   client-side through the flow).

No stage of this lookup calls Gemini, SerpAPI, or any external service.

## Scan-and-log — the quick flow

`ingest.html`'s capture screen (`09-consumption-logging.md`) gains a
**Scan barcode** affordance beside the shutter. Scanning respects the
**current sticky capture mode** — the mode selector's whole point is
that a run of scans asks no questions:

- **Stocking up** → on a local/catalog hit, a one-screen sheet:
  quantity (default 1), location (defaulting to the product's most
  recent batch's location, else a picker), editable expiry defaulted
  per `08-expiration-and-classification.md`. Confirm writes one
  `inventory_batches` row and one `inventory_logs` row with
  `reason = 'purchase'`, in one transaction — the same shape as a
  shopping-list line resolution (`07`).
- **Using up** → a one-screen decrement sheet: editable count (default
  1), applied to the nearest-expiry batch by default with the same
  reassign/split controls as `09`; confirm writes the decrement and its
  `consumption` log rows under `09`'s exact rules (batch at zero is
  deleted, over-decrement is `422`).
- **Shelf scan** mode simply has no barcode affordance — a shelf is not
  a barcode.

These sheets are ordinary synchronous endpoints — the existing batch
create (`13-stocktake-and-audit.md`'s route, with `reason='purchase'`
supplied by this flow's dedicated endpoint below) is *not* reused
blindly; instead:

`POST /api/storages/{storage_id}/barcodes/{code}/log` — body
`{direction: "in" | "out", quantity, location_id?, expiration_date?,
decrements?: [{batch_id, quantity}]}` — resolves the code locally
(`404` on a miss — the quick flow only logs against known products),
validates everything against the storage, and writes the batch/log
rows for the chosen direction as above. One endpoint, so a native
client (`12-client-api-contract.md`) gets the same one-round-trip flow
the PWA has. It accepts `Idempotency-Key` like every write.

**Nothing is written without the confirm tap.** The sheet is the review
step — smaller than a job review because there is nothing probabilistic
to review, but the invariant that no scan/photo mutates inventory by
itself holds here exactly as everywhere (`06`, `09`).

## Decoding in the PWA

- Primary: the browser's native **`BarcodeDetector`** API where
  available (Chromium-based mobile browsers — the PWA's actual
  platform), reading frames from the camera `<video>` stream. Formats:
  `ean_13`, `ean_8`, `upc_a`, `upc_e`, `code_128`.
- Always available: **manual entry** — a numeric field on the scan
  sheet. It is the fallback for browsers without `BarcodeDetector`, and
  it must exist regardless, for damaged labels.
- A vendored single-file decoder library **may** be added later under
  the vendoring rule (`05-frontend-pwa-foundations.md`) if
  `BarcodeDetector` coverage proves too narrow in practice — but it is
  not required for this spec, and no CDN, npm, or multi-file wasm
  bundle is acceptable. Detection support is feature-checked; the
  affordance renders scan-or-type accordingly.

## Acceptance criteria

- Scanning a code previously associated in this storage resolves the
  product with **zero** external API calls, in both capture modes, and
  logging through the sheet writes the same batch/log rows the
  equivalent `07`/`09` confirm would.
- A code known only to the catalog returns the display-field card and
  nothing else — no ids, no timestamps, no storage-derived data; a test
  asserts response-shape parity with the `07` stage-2 card invariants.
- The same physical barcode in two storages resolves independently;
  storage A's association is invisible to storage B (beyond the
  anonymous catalog card both may see).
- An unknown code is `404`, indistinguishable from any other, and the
  UI lands on the single-product photo path with the code carried
  through for post-confirm association.
- Associating a code already mapped in this storage is `409`; after
  deleting the association, re-associating succeeds.
- Admin deletion of a `catalog_barcodes` row never touches any
  storage's `product_barcodes`.
- Merging products (`16-product-maintenance.md`) re-points the source's
  barcodes to the survivor, dropping any that collide with the
  survivor's existing codes.
- No write occurs from a scan without the explicit confirm; the quick
  endpoint honors `Idempotency-Key` replay semantics
  (`12-client-api-contract.md`).
