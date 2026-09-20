# 24 — Barcode Hot Cache

Depends on: [`20-barcode-recall.md`](20-barcode-recall.md) (`catalog_barcodes`,
the recall lookup endpoint this spec accelerates but never replaces),
[`02-data-model.md`](02-data-model.md) (`catalog_products` privacy
invariants), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(service worker, `js/api.js` conventions).

## Why this spec exists

Spec 20 already makes a repeat scan free of any AI or third-party call —
but "free" still costs one network round-trip to
`GET /api/storages/{storage_id}/barcodes/{code}` before the app can show
anything. For the handful of products that are scanned constantly across
a whole self-hosted instance (the household's own milk, the usual bread,
whatever gets bought every week), that round-trip is the only thing left
between a scan and an answer. This spec removes it for exactly that
common case: the client keeps a small, self-refreshing local cache of the
**500 most-scanned barcodes across this instance**, so those resolve with
**zero network calls at all**, while every other code still falls
straight through to spec 20's existing endpoint — no new fallback to
build, because that endpoint already is one.

**This is a perceived-latency accelerant only, never a second source of
truth.** Every write still goes through spec 20's real endpoints; nothing
here changes what gets confirmed or committed.

## Scan counting — instance-wide, catalog-scoped, anonymous

Only barcodes that already have a `catalog_barcodes` row participate.
This is deliberate, not a shortcut: `catalog_barcodes` is already the
one table in this system designed to be visible instance-wide with no
storage reference (`20-barcode-recall.md`), so counting scans against it
adds no new cross-storage visibility — a barcode with only a private
`product_barcodes` association (no catalog entry) simply never
contributes to or appears in the hot cache, exactly as it never appears
in any other cross-storage-visible response today.

```sql
ALTER TABLE catalog_barcodes
    ADD COLUMN scan_count BIGINT NOT NULL DEFAULT 0;
```

- Incremented on every **successful** `GET .../barcodes/{code}` lookup
  (`20-barcode-recall.md`) whose code has a `catalog_barcodes` row —
  whether the lookup itself resolved as a local hit or a catalog hit.
  A miss increments nothing.
- The increment (`UPDATE catalog_barcodes SET scan_count = scan_count +
  1 WHERE barcode = $1`) is a single atomic statement — no read-modify-
  write race — run **asynchronously after the response is written**,
  exactly like the suggestion-cache's `last_accessed_at` update in
  `07-shopping-list-reconciliation.md`: counting a scan must never add
  latency to the lookup a person is waiting on, and a failed count
  update must never fail the lookup itself.
- **No response anywhere ever includes `scan_count`.** The hot-cache
  endpoint below exposes rank through array order only. This is more
  conservative than strictly required — a raw count would not by itself
  reveal which storage did the scanning — but it keeps every
  `catalog_barcodes`-derived response to the same "display fields only,
  no counts, no ordering that implies more than product popularity"
  shape `02-data-model.md` already establishes for the catalog, rather
  than making this one endpoint the exception.

## The hot-cache endpoint

`GET /api/barcodes/hot` — session required (`RequireSession`), **not**
storage-scoped: the data behind it carries no storage reference at any
point, exactly like a `catalog_barcodes`-derived recall-lookup response,
so there is nothing to scope it to. Any authenticated user, in any
storage, gets the same list.

```json
{
  "items": [
    { "barcode": "4006381333931", "display_name": "Barilla Penne 500g",
      "category_path": "Food > Pasta", "item_type": "long_shelf_life",
      "image_url": "/api/catalog-images/9c1f…", "icon_name": null,
      "default_shelf_life_days": 730 }
  ]
}
```

- Up to 500 rows, ordered by `scan_count DESC`, ties broken by
  `catalog_id` for a stable order across calls. Fields are exactly the
  display fields spec 20's catalog-hit response already exposes — no
  new field shape to learn.
- `image_url` points at the same catalog-image serving mechanism as
  every other catalog-derived picture (`07-shopping-list-reconciliation.md`):
  fetched once into this server's own cache, never a third-party URL
  handed to the browser.
- Computed from live data at request time; at household/instance scale
  (`00-overview.md`) a `LIMIT 500` ordered scan of `catalog_barcodes`
  needs no materialized rollup. Revisit only if real deployments show
  otherwise.

## Client-side cache

A new IndexedDB store, `hotBarcodes` (`js/barcodes.js`, alongside the
other shared helpers in `05-frontend-pwa-foundations.md`'s `js/`
layout):

- **Refreshed wholesale**, not incrementally: on app load, if the stored
  copy is missing or older than 24 hours, fetch `GET /api/barcodes/hot`
  once and replace the store entirely. 500 rows of display-field data is
  small (well under 200KB) — diffing it is not worth the complexity a
  delta-sync mechanism (`12-client-api-contract.md`) would add for a
  dataset this size and this tolerant of staleness.
- A fetch failure (offline, server error) leaves the existing cached
  copy in place, however old, and is not surfaced as an error anywhere
  — this is a nicety, and its complete absence must degrade to exactly
  spec 20's existing behavior, never to a broken scan flow.
- Never written to on a scan. The cache is populated only from
  `GET /api/barcodes/hot`; a locally-decoded barcode is looked up
  against it, never inserted into it — inserting client-observed scans
  would make the "cache" a second, uncoordinated copy of server-side
  counting logic for no benefit, since the server already counts every
  lookup that reaches it.

### How a scan uses it

1. The barcode is decoded (client-side `BarcodeDetector`, or the
   server-side decode fallback — both per `20-barcode-recall.md`).
2. The client checks the local `hotBarcodes` store for the code.
   - **Hit:** render the catalog card **immediately** (no loading
     state) as an optimistic preview, using the cached display fields.
   - **Miss, or cache empty/stale:** render the ordinary loading state
     exactly as spec 20 already defines.
3. **Either way**, the client calls spec 20's
   `GET /api/storages/{storage_id}/barcodes/{code}` — this call is
   never skipped, hit or miss. Its answer is authoritative and may
   differ from the optimistic preview (most commonly: this storage
   already has a **local** product for the code, which spec 20's lookup
   order always prefers over a catalog hit) — when it does, the
   authoritative answer replaces the preview before anything becomes
   confirmable. The optimistic preview only removes the spinner; it
   never supplies the data an add/log sheet is built from.
4. A confirm, association, or quantity write proceeds exactly as spec
   20 defines — this spec changes nothing about that path.

This keeps every invariant `20-barcode-recall.md` already established
completely intact: nothing is written from a scan event, the server
lookup remains the one source of truth for what a storage already
knows, and a hot-cache hit changes only how fast the *recognition*
feels, never what gets recorded.

## What this deliberately does not do

- **No offline recall.** The one network call in step 3 above is never
  skipped, so this feature adds no offline-editing capability — the
  non-goal in `00-overview.md` stands exactly as it does for every other
  spec.
- **No per-storage cache of this storage's own barcodes.** Only the
  instance-wide top 500 is cached client-side. A storage's own less-
  popular products still cost the one round-trip spec 20 already
  requires. A fuller per-storage cache is a plausible follow-up, tracked
  as [`25-per-storage-barcode-cache.md`](../spikes/25-per-storage-barcode-cache.md)
  in `docs/spikes/` rather than built here, pending evidence that the
  instance-wide cache alone does not already feel instant enough in
  practice.

## Acceptance criteria

- A lookup whose barcode has a `catalog_barcodes` row increments its
  `scan_count` by exactly one, asynchronously, without adding measurable
  latency to the lookup response; a lookup whose barcode has no
  `catalog_barcodes` row (local-only association, or a miss) increments
  nothing.
- `GET /api/barcodes/hot` returns at most 500 items, ordered by scan
  count descending, containing only display fields — no `barcode`-to-
  storage linkage, no counts, no ids or timestamps beyond what spec 20's
  catalog-hit shape already exposes.
- The response is identical regardless of which storage the requesting
  user is acting in, or how many storages they belong to.
- A client with an empty or stale `hotBarcodes` store behaves exactly as
  if this spec did not exist: every scan still resolves correctly via
  spec 20's endpoint, just without the instant preview.
- The authoritative lookup (`20-barcode-recall.md`) is called for every
  scan, hit or miss against the local cache, with no code path that
  skips it — verified by a test that seeds a hot-cache hit whose
  authoritative answer differs (a local product exists) and asserts the
  local answer wins.
- No test or code path ever serializes `scan_count` into an HTTP
  response.
