# 20 — Barcode Recall

Depends on: [`02-data-model.md`](02-data-model.md) (`products`,
`catalog_products` and its privacy invariants),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(single-product photo path this feature falls back to, and one of the
product-creation points the capture-time offer below attaches to),
[`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(the New Item flow is another product-creation point),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md),
[`09-consumption-logging.md`](09-consumption-logging.md) (capture modes,
batch decrement semantics),
[`10-reorder-and-shopping-export.md`](10-reorder-and-shopping-export.md)
(the reorder add-item flow is the third product-creation point).
[`14-account-self-service.md`](14-account-self-service.md)'s
`settings.html` Account section is where the capture-time offer is turned
back on once dismissed — no contract dependency on that spec's endpoint,
this spec defines its own.

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

### `users` columns for the capture-time offer (below)

```sql
ALTER TABLE users
    ADD COLUMN barcode_prompt_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN barcode_prompt_seen_at TIMESTAMPTZ;   -- NULL = never shown to this user
```

Per-user, not per-storage: whether this person wants to be offered a
barcode scan at first capture is a personal preference about their own
workflow, same category as the gamification opt-out
(`50-gamification-overview.md`) — and, like that one, turning it off must
never disable or degrade any inventory feature, only the offer itself.
`barcode_prompt_seen_at` is what distinguishes this user's first-ever
occurrence of the offer (playful, explanatory copy) from every later one
(terse, same three actions) — see "Offering a barcode at first capture"
below.

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
  list with add-by-scan and add-by-typing. Always available, unaffected
  by the preference below — this is a deliberate action, not an offer.
- **The capture-time offer**, below — the primary path, because it
  catches a new product at the one moment its barcode is easiest to
  reach: while the item is still in the user's hand.

## Offering a barcode at first capture

A product created with no barcode already resolvable for it is the
common case the second-scan-is-free promise depends on — nobody benefits
from that promise until a first barcode exists to recall. So the moment
a **new product** is created — confirming a vision-ingestion review row
(`06-vision-shelf-ingestion.md`), accepting a shopping-list New Item
(`07-shopping-list-reconciliation.md`), or adding a product from the
reorder dashboard (`10-reorder-and-shopping-export.md`) — and that
product still has **no** `product_barcodes` row, the confirmation screen
offers to scan one right there, before moving on.

**Three explicit choices, every time it appears, never a fourth silent
one:**

1. **Do it now** — opens the scan sheet inline (client-side
   `BarcodeDetector`, same as "Decoding in the PWA" below); a successful
   scan associates the code via `POST …/products/{id}/barcodes` above,
   in the same interaction.
2. **Not this time** — dismisses the offer for this product only.
   Nothing is recorded; the same product will simply have no barcode
   until someone adds one some other way. This is the ordinary case for
   an item whose packaging is already gone or whose barcode is worn —
   declining costs nothing and is not tracked as a decision.
3. **Turn this off** — sets `users.barcode_prompt_enabled = FALSE`. The
   offer stops appearing at every future capture, for this user, across
   every storage they belong to, until they turn it back on.

`PATCH /api/auth/barcode-prompt` — session required, body
`{enabled: bool}`. A user-scoped route beside `/api/auth/password`
(`14-account-self-service.md`) and `/api/auth/devices`
(`03-auth-and-multi-tenancy.md`) — not storage-scoped, since the
preference is the user's, not any one storage's. `settings.html`'s
Account section (`14-account-self-service.md`) shows the current state
and lets a user who turned the offer off turn it back on — the "im
Profil wieder aktivieren" path. This is the only way to disable or
re-enable it; there is no per-product or per-storage override.

**Tone, once — not a gamification mechanic.** The very first time this
offer is shown to a user (`barcode_prompt_seen_at IS NULL`), its copy is
a short, inviting explainer of *why* ("scan it once now, and every
future can of this stays one tap"), and `barcode_prompt_seen_at` is set
to `now()` in the same request that records the choice. Every later
occurrence for that user is the same three actions with plain, terse
copy — no repeated onboarding tone, no streak, no points, no interaction
with `50-gamification-overview.md`'s scoring at all. This offer is
**never scored** by the gamification layer, accepted, deferred, or
declined alike: the layer already excludes activity that a naive design
could inflate for reward (`50`'s own principle), and a barcode scan is
exactly that shape of action.

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

### Server-side decode fallback (no `BarcodeDetector`)

For a browser without `BarcodeDetector` support, manual entry above is
always available — but typing a 13-digit EAN correctly on a phone
keyboard is exactly the friction this whole spec exists to remove. A
second fallback closes that gap: photograph the barcode, let the server
decode it. *(Amended by [`36-photo-source-picker.md`](36-photo-source-picker.md):
the sheet's photo input is that spec's picker in single-photo mode — a
"Photos" and a "Camera" control — so a barcode photographed earlier can be
used too. Everything below about what happens to the photo is unchanged.)*

`POST /api/storages/{storage_id}/barcodes/decode` — multipart upload of
one image. The server decodes it with a pure-Go barcode-reading library
(no cgo, so the static-binary/scratch-container build in
`01-architecture-and-deployment.md` is unaffected) and answers
`200 {"barcode": "..."}` on a successful read, or `422
validation_failed` when nothing decodable is found — never a silent
empty result. The decoded value then flows through the same association
or lookup path as a client-decoded scan; this route only replaces *how
the code is obtained*, nothing downstream of it.

**The photograph is never written to disk, and never enters any of the
image storage areas in `04-backend-api-conventions.md`'s table.** It is
held in memory only for the duration of the decode call and discarded
the moment the response is written — no EXIF stripping step, because
there is no persisted file for EXIF to survive on. This is a stricter
rule than every other upload in the system, which is the point: a photo
whose only purpose is reading a printed number off a box must not become
a second, accidental photo of whatever else was in frame.

This route is a plain, bounded, synchronous image-processing call, not a
background job (`04-backend-api-conventions.md`'s job machinery exists
for slow *external* calls; decoding a barcode locally is neither slow
nor external) and not an AI call (no Gemini involved — this is
deterministic pixel decoding, not identification, so it costs nothing
and answers immediately).

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
- A new product created via `06`, `07`, or `10` with no barcode yet
  resolvable offers the capture-time scan; a new product created with
  one already resolvable (e.g. the shopping-list line matched via a
  catalog barcode hint) does not.
- The offer always presents exactly the three actions above, never
  fewer; "not this time" writes nothing and reappears on the next
  qualifying product; "turn this off" sets
  `users.barcode_prompt_enabled = FALSE` and the offer never appears
  again for that user, in any storage, until they re-enable it via
  `PATCH /api/auth/barcode-prompt`.
- `barcode_prompt_seen_at` is set exactly once, on the first time the
  offer is shown to a user, and never again — the playful first-time
  copy is never shown twice to the same user.
- Accepting, declining, or disabling the offer writes no gamification
  contribution of any kind.
- `POST …/barcodes/decode` never writes the uploaded image to any
  storage area — a test asserts no new file appears anywhere under
  `/data/uploads/` or `/data/cache/` after a call, success or failure
  alike — and a photo containing no readable barcode is `422`, never a
  guessed or empty-string result.
