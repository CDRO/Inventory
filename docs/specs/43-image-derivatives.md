# 43 — Image derivatives: pre-rendered thumbnails and crops

Accepted 2026-10-03, promoted from the spike of the same number. Depends on:
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (upload
handling, storage areas, background jobs),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md) and
[`09-consumption-logging.md`](09-consumption-logging.md) (the review screens
and their crops), [`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(product pictures), [`15-backup-restore-and-export.md`](15-backup-restore-and-export.md)
(what is and is not backed up).

## Why

Every screen that showed a picture showed the **source file**. The review
screens painted each detected item as a CSS `background-image` of the whole
shelf photo — up to 50 MP and 25 MB — scaled and positioned per row, so a
phone decoded a 50 MP JPEG and rasterised it once per row; the inbox fetched
the whole photo once per card; the inventory table pulled 1024 px product
pictures to paint 36 px squares. The owner's report was simply that previews
took far too long to load. (The CSS crop also stretched non-square boxes, its
two axes being scaled independently.)

The fix is the usual one: render every picture once, on the server, at the
sizes screens show it, and serve those. Done in Go with
`golang.org/x/image/draw`, which was already a dependency and already did
this work for product pictures and the suggestion cache. ImageMagick was
measured and rejected: about twice as fast on a step that runs once per
upload inside a job already waiting 5–20 s on the vision model, no faster on
the serving path, and it would have cost the `scratch` image (34 shared
libraries), put a C decoder with a long CVE history on untrusted uploads, and
been the first non-Go runtime dependency. The measurements are in the "How
the numbers were produced" section at the end.

## Owner decisions

Four choices were put to the owner on 2026-10-03 and decided as follows.
They are not open questions.

1. **A row crop is the exact bounding box, framed as `object-fit: cover`
   frames it** — the box, then its largest centred square — not the box
   expanded with shelf context.
2. **The size ladder is 96 / 192 / 384 / 768 px squares plus a 1600 px
   preview.** 768 was added for a future lightbox.
3. **Derivatives live on the `imagecache` volume**, outside the backup,
   re-derivable by definition.
4. **The bare image routes keep serving the original.** Put to an advisor:
   removing them would be a breaking change under `12`'s policy (an existing
   endpoint's 200 becoming 404; a handed-out `image_url` no longer fetchable
   as is), and the bare job route is a native client's only full-resolution
   path. Variants are additive path segments beneath the URL a response hands
   out.

## Variant catalogue

A fixed, server-side allow-list (`images.PhotoVariants`,
`images.RowVariants`). Nothing outside it is ever rendered, and no client
string ever selects a size by number.

| Variant | Geometry | Encoding | Shown at |
|---|---|---|---|
| `thumb-96`, `thumb-192`, `thumb-384`, `thumb-768` | square N × N: the source's largest centred square, scaled down | JPEG q82; PNG when the source is a PNG with transparency | every `<img>` thumbnail — inventory table 36 px, stocktake and review-list rows 56 px, inbox card and review crop 72 px — at 1×, 2× and 3× device pixels, chosen by `srcset` |
| `preview` | longest edge ≤ 1600, aspect kept | JPEG q82 | whole-picture views: a single-product photo's review row, any row without a box, the product detail's picture |
| `rows/{row_id}/thumb-192`, `rows/{row_id}/thumb-384` | the row's `bounding_box` clamped to the photo, then its largest centred square, scaled to N | JPEG q82 | the crop in a review row; the cutout comparison's original for a crop |

Rules that hold for every variant:

- **Never scaled up.** A source smaller than the size yields the source's
  own size: a 1024 px product picture's `preview` is 1024 px (re-encoded at
  q82, which is smaller than the stored q92 file), a 20 px crop's
  `thumb-192` is 20 px.
- **Row crops are the exact box.** The product picture a confirm cuts
  (`new_product.image: "crop"`) is unchanged: still the exact box, still
  `images.ProductImage`.
- **Scaling is box-then-kernel.** A YCbCr source is first area-averaged by
  the largest integer factor that leaves every variant 1.5× oversampled
  (`preShrinkFactor`), then CatmullRom does the fractional remainder.
  Anything else — a PNG, a small JPEG — goes straight to CatmullRom. That is
  what halves the time and memory of a 50 MP reduction; it is the two-stage
  pipeline libvips calls shrink-on-load.
- **One decode per set.** All whole-picture variants come from one decode;
  all of a proposal's row crops come from one decode. A decode is the cost,
  not an encode.
- **No metadata.** Every derivative is encoded from pixels, as every
  re-encode in `internal/images` already is.
- **Decode bounds.** `images.MaxPixels` applies to every decode.
- **No WebP/AVIF.** Go has no encoder for either; at these sizes JPEG is
  small enough to make the question moot.

## Where derivatives live

`/data/cache/derived/{area}/{source-stem}/{variant}.{jpg|png}` on the
**`imagecache` volume**, next to the suggestion cache — not on `uploads`
(`uploads.DerivedDir`, `uploads.Derived`).

- `area` is `ingest` or `products`; `source-stem` is the source file's UUID,
  the server-generated name `uploads.Dir` already validates. A row crop is
  `rows-{row_id}-thumb-192.jpg`. Every path component is matched against its
  one allowed shape before it touches the filesystem; nothing is sanitised.
- They are **re-derivable by definition**, so they belong with the tier
  `15` leaves out of the backup. `15`'s rule for the suggestion cache
  applies verbatim: a missing file is a miss, and the system heals in both
  directions.
- **A derivative never outlives its source.** Enforced in one place:
  `uploads.Dir.Remove` removes the source's whole derived directory with the
  source (`RemoveDerivedWith`). Every existing removal — job discard,
  discard-all, the 30-day retention sweep, product delete and merge, a
  replaced product picture, a failed confirm's cleanup — gets it with no new
  call sites. An hourly orphan sweep (at start-up, then every hour) deletes
  derived directories whose source is gone, covering a crash between the two
  removals and a restored backup whose uploads tree is older than the cache.
- **Bounded without eviction.** Each source has at most five whole-picture
  files plus two per row; that is 3–6 % of a photo's own size and about a
  quarter of a product picture's. No LRU, no cap, nothing to tune.
- An SVG product picture (a promoted icon) has no derivatives: it is served
  as it is at every size.
- Cutouts (`/data/uploads/cutouts/`) are out of scope: ≤ 1024 px PNGs that
  live for one review and are looked at one at a time.

## When they are made: eager, with a lazy safety net

One rule: **every write of a source schedules its derivatives; every read of
a missing derivative makes it** (`internal/derive`). The eager path is the
normal one; the lazy path is what makes a restore, a cache wipe, a crash, or
a deployment onto existing data invisible.

- **Photo jobs** (`shelf_ingestion`, `product_photo`, `consumption_photo`,
  `shopping_list_photo`), inside the job's work:
  1. The whole-picture set starts **concurrently with the vision call**, so
     it costs the job no latency and the inbox card has its thumbnail within
     seconds of the upload.
  2. After the proposal is built, the row set is cut from a **second
     decode** — holding 150 MB of pixels across a 20 s network wait is the
     wrong trade — with the source's previous `rows-*` files removed first,
     so "Analyze again" cannot leave a stale crop beside a new box. A
     photographed shopping list has no boxes and gets only step 1.
  3. Both steps are best effort: a derivation error is logged (`18`) and
     never changes what the vision call decided about the job. A job waits
     for its own derivation goroutine before it is recorded as finished.
- **Product pictures**: `productPictures.save` — the single path by which a
  picture reaches `/data/uploads/products/` — saves through
  `derive.Saving`, which schedules the whole-picture set in the background
  after writing the file.
- **Lazy**: the serving handler, finding a variant missing, derives it on
  the spot *after* the access check, writes it, and serves it. The unit of
  work is the whole set a decode can serve: a request for `thumb-192` makes
  `preview` and all four thumbnails; a request for one row makes every
  row's crops. Keyed per source in a `singleflight` group
  (`golang.org/x/sync`), so twenty first-time requests for one job's crops
  decode the photo once. Row sets are additionally keyed by a digest of the
  boxes, so two analyses of one photo never share a run.
- **Existing data** needs no migration: the lazy path covers it. A one-shot
  warm-up at start-up walks `/data/uploads/products/` and makes missing sets,
  so the first inventory table after the deploy does not trickle in; ingest
  photos are not warmed (most on disk belong to consumed jobs nobody will
  open).
- **Two lanes, one slot each.** All derivation goes through an `eager` lane
  (jobs, warm-up, saves) and a `lazy` lane (requests), each a single slot. A
  request never waits behind background work; at most two derivations run at
  once, so the worst case on top of the baseline is two 50 MP sets, about
  600 MB, and the typical lazy case a 70 ms product picture. A lazy
  derivation runs under its own 30 s context, detached from the request's: a
  client that gives up should not waste the decode it caused.

## Routes

```
GET /api/storages/{storage_id}/jobs/{id}/image/{variant}
        variant ∈ preview | thumb-96 | thumb-192 | thumb-384 | thumb-768
GET /api/storages/{storage_id}/jobs/{id}/image/rows/{row_id}/{variant}
        variant ∈ thumb-192 | thumb-384
GET /api/storages/{storage_id}/product-images/{name}/{variant}
        variant ∈ preview | thumb-96 | thumb-192 | thumb-384 | thumb-768
```

- Same access check as the route beneath each: the job must be in the
  storage; a product in the storage must use the picture. A variant outside
  the list, a `row_id` that is not a row of the job's current proposal or has
  no usable box, a job without a photo, a deployment with no usable cache
  volume (job routes only) — all `404 not_found`, worded like the existing
  misses (`03`). The `row_id` doubles as the box lookup: it is validated
  against `jobs.payload`, never trusted as text.
- **The bare routes keep serving the original** (owner decision 4). The
  export (`15`), third-party clients (`12`) and the server-side crop at
  confirm time are unaffected. The PWA requests only variants; the e2e
  ingestion journey asserts that no request for the original is made.
- An SVG product picture answers every variant with the SVG itself, with
  the same CSP and `nosniff` headers as the bare route. A deployment whose
  cache volume is unusable answers every product-picture variant with the
  stored picture, which is what every size got before this spec.
- Headers as the route beneath: `private, max-age=86400` for product
  variants, `private, max-age=3600` for job variants, `nosniff`,
  `Content-Type` from the derivative's own encoding. A whole-picture variant
  is immutable. A row crop changes when a job is analysed again while its
  URL does not, so the client appends the job's `updated_at` as a `v` query
  parameter, which the server ignores.

## Frontend

- One shared helper, `js/images.js`: `thumbAttrs(url, cssPx)` returns
  `{src, srcset, sizes}` from the ladder; `setThumb(img, url, cssPx)` applies
  them to a templated `<img>`; `variantURL(url, variant)` appends a variant;
  `rowCropURL(imageURL, rowId, size, version)` addresses a crop. All four
  pass a URL without variants — `null`, an `.svg`, a suggestion-cache
  picture — through untouched, so a call site needs no branch. Used by
  `product-table.js` (36 px), `pages/stocktake.js` (56 px), `review.js`'s
  row thumbnail (56 px), `pages/inbox.js` (72 px), and the product detail in
  `pages/products.js`, which shows the `preview` because the whole picture
  is the point there. Thumbnails carry `loading="lazy"` and explicit
  `width`/`height` so a table does not shift as they arrive.
- `pages/review.js` and `pages/consume-review.js` no longer paint
  backgrounds. The `.review-crop` is an `<img>` with `object-fit: cover`,
  `src`/`srcset` from the row's `rows/{row_id}` variants at 192 and 384, the
  `preview` variant for a row without a box, `?v=updated_at` on row URLs.
  The cutout comparison's "original" uses the same source.
- `CACHE_VERSION` in `web/static/sw.js` is bumped and `/js/images.js` is a
  shell asset (`CLAUDE.md`, "Invariants that fail silently").

## Invariants this must not bend (`03`, `04`, `18`)

- No client-supplied string becomes a path component: the variant is matched
  against the list, the `row_id` against the proposal, the source name is
  the server's own UUID. The derived filename is built from those alone, and
  `uploads.Derived` refuses any other shape.
- `404`, never `403`, for a picture the caller may not see, and no wording
  that distinguishes "not yours" from "not there".
- Lazy derivation runs after the access check, never before.
- Logs carry a job id or a UUID filename at most — never pixels, never the
  original name of anything.

## Acceptance criteria

- Opening a shelf proposal transfers no request for the original photo: every
  picture on the review screen is a `rows/…` crop or the `preview`. The same
  for the consumption review.
- The inbox requests one thumbnail per card and never the original.
- The inventory table and the stocktake rows request `thumb-*` variants of
  product pictures, lazily, never the stored file.
- A row crop is the box clamped to the photo, then its largest centred
  square, never stretched and never scaled up; a box that selects nothing
  yields no crop and fails nothing (geometry unit tests in
  `internal/images`).
- A variant is removed with its source through `uploads.Dir.Remove`; an
  orphaned derived directory is gone after one sweep.
- A missing variant is rendered on request and served; concurrent first
  requests for one source decode it once (unit test with a counting loader).
- After "Analyze again", the row crops reflect the new boxes and no
  `rows-*` file of the old proposal remains.
- A derivation failure never fails a job and never changes its status; a
  failed analysis still gets its inbox thumbnail.
- Variant names outside the list, row ids outside the proposal, jobs
  without a photo: `404`. An SVG product picture answers every variant with
  the SVG. Without a cache volume, product pictures are served as stored.
- No upscaling: a `preview` of a source already within 1600 px keeps the
  source's dimensions.
- Deriving one 50 MP photo's whole-picture set peaks below about 300 MB
  (documented from the benchmark below, not a test), and no more than two
  derivations run at once.
- `go test ./...`, the e2e suite and `web/shell_manifest_test.go` pass.

## How the numbers were produced

Measured on a 4-vCPU x86 container (Go 1.26, `golang.org/x/image` v0.46,
ImageMagick 6.9) on a synthetic 8660 × 5773 JPEG of 8.9 MB — the 50 MP
ceiling of `06` — made with `convert -size 8660x5773 plasma:fractal -blur
0x1 -quality 90`, run through a throwaway Go program using only `image/jpeg`
and `golang.org/x/image/draw`, one process per scenario, peak RSS read from
`/proc/self/status`. The owner's DS923+ (Ryzen R1600, two cores) should be
reckoned at 1.5–2× these times.

| Step | Go | Peak RSS | ImageMagick |
|---|---|---|---|
| Decode 50 MP JPEG | 1.2 s | 150 MB | (inside the next rows) |
| Preview, longest edge 1600, CatmullRom straight from 50 MP | 2.9 s total | 455 MB | 0.9 s |
| Same, with a box pre-shrink before CatmullRom | **1.8 s total** | **250 MB** | — |
| Preview + three square thumbnails, one decode | 1.9 s | 270 MB | — |
| 20 row crops at 384 px, each also at 192 px, from the full decode | 2.5 s | 340 MB | — |
| One 384 px thumbnail | (in the row above) | | 0.3 s with DCT shrink-on-load |
| Product picture (≤ 1024 px) → 96/192/384 squares | 70 ms | — | — |

Output sizes at JPEG quality 82: preview ≈ 205 KB, 384 px square ≈ 21 KB,
192 px square ≈ 10 KB, 96 px square ≈ 4 KB. Real phone JPEGs are 4:2:0
rather than this file's 4:4:4, so their decode holds half the pixels in
memory; everything else is the same.
