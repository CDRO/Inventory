# 43 — Image derivatives: pre-rendered thumbnails and crops

**Status:** Proposed · **Raised:** 2026-10-03
**Source:** the owner, after using the system for a while: pictures in the
review previews load far too slowly, because they are served at their
original size. Asked for: every picture downscaled to the size it is shown
at, cropped where the screen crops it, made asynchronously once the shelf
photo's analysis has returned each item's box, for every size the UI needs,
in Go or — if it does not bloat the container — ImageMagick. This spike is
the proposition. Written so that it can be promoted to
`docs/specs/43-image-derivatives.md` without rewriting.

## The problem, measured

Three screens show pictures, and all three show the **source file**:

| Screen | What is shown | What is fetched |
|---|---|---|
| Review (`review.html`, `consume-review.html`) | one 72 px square per detected item, cut from the shelf photo | the **whole original photo**, up to 50 MP / 25 MB, as a CSS `background-image` that is scaled and positioned per row (`paintCrop` in `js/pages/review.js`) |
| Inbox (`inbox.html`) | one 72 px square per waiting job | the **whole original photo**, once per card |
| Inventory table, products, stocktake, shopping list | 36–128 px thumbnails | the product picture as stored: ≤ 1024 px, typically 150–350 KB |

The review case is the one that hurts. One request, but the browser then
decodes a 50 MP JPEG (≈ 200 MB of pixels), rasterises it once per row, and
on a phone evicts and re-decodes it while scrolling. The inbox multiplies
that by the number of waiting jobs. The product thumbnails are a smaller
version of the same mistake: a 200-row inventory table pulls 30–70 MB to
paint 36 px squares.

There is also a framing bug in the CSS crop: `background-size` is set to
`100/box.width % × 100/box.height %`, two independent scales, so a tall jar's
box is squeezed into a square. A server-rendered crop fixes that for free.

## Go or ImageMagick

Measured here on a 4-vCPU x86 container (Go 1.26, `golang.org/x/image`
v0.46, ImageMagick 6.9), on a synthetic 8660 × 5773 JPEG of 8.9 MB — the
50 MP ceiling of `06`. The owner's DS923+ (Ryzen R1600, two cores) should be
reckoned at 1.5–2× these times.

| Step | Go | Peak RSS | ImageMagick |
|---|---|---|---|
| Decode 50 MP JPEG | 1.2 s | 150 MB | (inside the next rows) |
| Preview, longest edge 1600, CatmullRom straight from 50 MP | 2.9 s total | 455 MB | 0.9 s |
| Same, with a 4× box pre-shrink before CatmullRom | **1.8 s total** | **250 MB** | — |
| Preview + three square thumbnails (96/192/384), one decode | 1.9 s | 270 MB | — |
| 20 row crops at 384 px, each also emitted at 192 px, from the full decode | 2.5 s | 340 MB | — |
| One 384 px thumbnail | (in the row above) | | 0.3 s with DCT shrink-on-load |
| Product picture (≤ 1024 px) → 96/192/384 squares | 70 ms | — | — |

Output sizes at JPEG quality 82: preview ≈ 205 KB, 384 px square ≈ 21 KB,
192 px square ≈ 10 KB, 96 px square ≈ 4 KB.

So ImageMagick is roughly twice as fast on a step that runs **once per
upload, in a background job that is already waiting 5–20 s on the vision
model**, and makes no difference at all on the serving path, which is a
file read either way. Against that:

- **The container.** `prod` is `FROM scratch` with one static binary
  (`01`, and a hard constraint in `CLAUDE.md`). `convert` links 34 shared
  libraries here (MagickCore, MagickWand, jpeg, png, gomp, lcms2, fontconfig,
  freetype, xml2, z, bz2, lzma, ltdl, …). Shipping it means either leaving
  `scratch` for an Alpine base plus `imagemagick` (≈ +60–80 MB, several times
  the current image) or hand-copying `.so` files into `scratch`, which breaks
  on the next base-image bump. Plus a `policy.xml`, a process-exec path, and
  ImageMagick's own memory/thread limits to tune, because a 50 MP decode in
  ImageMagick wants hundreds of MB too.
- **The attack surface.** Every decode today goes through Go's memory-safe
  `image/jpeg` and `image/png`, on bytes a user uploaded. ImageMagick on
  untrusted input has a long CVE history (ImageTragick and successors); it
  would be the first C decoder in the system and the only non-Go runtime
  dependency.
- **It already exists.** `golang.org/x/image/draw` is a direct dependency
  and already does exactly this work for product pictures
  (`images.fitWithin`) and the suggestion cache (`imagesearch.downscale`).
  The missing piece is a box pre-shrink that halves the time and memory of a
  large reduction, and a place to keep the results.

**Proposition: Go.** The design below keeps all resizing behind one package
with one function per variant, so if a future workload ever justifies an
external binary (libvips would be the right one, not ImageMagick) it is a
contained swap, not a redesign.

## Design

### Variant catalogue

A fixed, server-side allow-list. Nothing outside it is ever rendered, and no
client string ever selects a size by number.

| Variant | Geometry | Encoding | Shown at |
|---|---|---|---|
| `thumb-96`, `thumb-192`, `thumb-384` | square N × N: the source's largest centred square, scaled down (what `object-fit: cover` draws today) | JPEG q82; PNG when the source has transparency | every `<img>` thumbnail — inventory table 36 px, stocktake row 56 px, inbox card and review crop 72 px, shopping-list card 96 px, product detail 128 px — at 1×, 2× and 3× device pixels, chosen by `srcset` |
| `preview` | longest edge ≤ 1600, aspect kept | JPEG q82 | whole-photo views: a single-product photo's review row, the "whole photo" picture choice, the cutout comparison's original, any row without a box |
| `rows/{row_id}/thumb-192`, `rows/{row_id}/thumb-384` | the row's `bounding_box` **expanded to a square around its centre**, shifted to stay inside the photo, then scaled to N | JPEG q82 | the crop in a review row; the cutout comparison's original for a crop |

Rules that hold for every variant:

- **Never scaled up.** A variant that would not shrink the source *is* the
  source: a `preview` of a 1024 px product picture serves the original file.
- **Square around the box, not the box stretched.** The whole item stays
  visible, undistorted, with shelf context filling the short axis. The
  product picture a confirm cuts (`new_product.image: "crop"`) is unchanged:
  still the exact box, still `images.ProductImage`.
- **Scaling is box-then-kernel.** A YCbCr source shrunk by ≥ 2 is first
  area-averaged by the largest integer factor that keeps it above 1.5× the
  target, then CatmullRom does the fractional remainder. Anything else goes
  straight to CatmullRom. That is the row of the table above that halves the
  time and memory; it is the same two-stage pipeline libvips uses.
- **No metadata.** Every derivative is encoded from pixels, as every
  re-encode in `internal/images` already is.
- **Decode bounds.** `images.MaxPixels` applies to every decode, source
  files being ≤ 50 MP by construction notwithstanding.
- **No WebP/AVIF.** Go has no encoder for either; at these sizes JPEG is
  small enough to make the question moot.

### Where derivatives live

`/data/cache/derived/{area}/{source-stem}/{variant}.{jpg|png}` on the
**`imagecache` volume**, next to the suggestion cache — not on `uploads`.

- `area` is `ingest` or `products`; `source-stem` is the source file's UUID
  (the server-generated name `uploads.Dir` already validates). A row crop is
  `rows-{row_id}-thumb-192.jpg`.
- They are **re-derivable by definition**, so they belong with the tier
  `15` deliberately leaves out of the backup. `15`'s rule for the suggestion
  cache applies verbatim: a missing file is a miss, and the system heals in
  both directions.
- **A derivative never outlives its source.** Enforced in one place:
  `uploads.Dir.Remove` is given the derived area for that directory and
  removes the source's whole derived directory with the source. Every
  existing removal — job discard, discard-all, the 30-day retention sweep,
  product delete and merge, a replaced product picture, a failed confirm's
  cleanup — gets it for free, with no new call sites. An hourly orphan
  sweep (startup, then alongside the suggestion-cache sweep) deletes derived
  directories whose source is gone, covering a crash between the two
  removals and a restored backup whose uploads tree is older than the cache.
- **Bounded without eviction.** Each source has at most four photo-level
  files plus two per row; that is 3–6 % of a photo's own size and ≈ 25 % of
  a product picture's. No LRU, no cap, nothing to tune.
- Cutouts (`/data/uploads/cutouts/`) are out of scope: ≤ 1024 px PNGs that
  live for one review and are looked at one at a time.

### When they are made: eager, with a lazy safety net

One rule: **every write of a source schedules its derivatives; every read of
a missing derivative makes it.** The eager path is the normal one; the lazy
path is what makes a restore, a cache wipe, a crash, or a deployment onto
existing data invisible.

- **Photo jobs** (`shelf_ingestion`, `product_photo`, `consumption_photo`,
  `shopping_list_photo`), inside the job's `work`:
  1. The photo-level set (`preview` + three thumbs, one decode) starts
     **concurrently with the vision call**, so it costs the job no latency
     and the inbox card has its thumbnail within seconds of the upload.
  2. After the proposal is built, the row set is cut from a **second
     decode** — holding 150 MB of pixels across a 20 s network wait is the
     wrong trade; a second 1.2 s decode in a background job is not. Any
     previous `rows-*` files of the source are removed first, so an
     "Analyze again" cannot leave a stale crop beside a new box.
  3. Both steps are best effort: a derivation error is logged (`18`) and
     never changes what the vision call decided about the job. The lazy
     path retries on request.
- **Product pictures**: `productPictures.save` — the single path by which a
  picture reaches `/data/uploads/products/` (confirm crop/photo/cutout,
  custom upload, promoted suggestion, promoted catalog picture) — schedules
  the photo-level set in the background after writing the file.
- **Lazy**: the serving handler, finding a variant missing, derives it on
  the spot *after* the access check, writes it, and serves it. The unit of
  work is the whole set a decode can serve, not one file: a request for
  `thumb-192` makes `preview` and all three thumbs; a request for one row
  makes every row's crops. Keyed per source and set in a `singleflight`
  group (`golang.org/x/sync`, already in `go.sum`), so twenty first-time
  requests for one job's crops decode the photo once.
- **Existing data** needs no migration: the lazy path covers it. A one-shot
  warm-up at startup walks `/data/uploads/products/` and makes missing
  photo-level sets through the eager lane, so the first inventory table
  after the deploy does not trickle in; ingest photos are not warmed (most
  on disk belong to consumed jobs nobody will open).
- **Two lanes, one slot each.** All derivation goes through a worker with an
  `eager` lane (jobs, warm-up) and a `lazy` lane (requests), each a single
  goroutine. A request never waits behind background work; at most two
  derivations run at once, so the worst case on top of the baseline is two
  50 MP sets ≈ 600 MB, the typical lazy case a 70 ms product picture. Jobs
  queue on the eager lane; with `DefaultConcurrency = 3` the queue is short.
  A lazy derivation runs under its own 30 s context, detached from the
  request's: a client that gives up should not waste the decode it caused.

### Routes

```
GET /api/storages/{storage_id}/jobs/{id}/image/{variant}
        variant ∈ preview | thumb-96 | thumb-192 | thumb-384
GET /api/storages/{storage_id}/jobs/{id}/image/rows/{row_id}/{variant}
        variant ∈ thumb-192 | thumb-384
GET /api/storages/{storage_id}/product-images/{name}/{variant}
        variant ∈ preview | thumb-96 | thumb-192 | thumb-384
```

- Same access check as the route beneath each: the job must be in the
  storage; a product in the storage must use the picture. A variant outside
  the list, a `row_id` that is not a row of the job's current proposal or
  has no box, a job without a photo — all `404 not_found`, worded exactly
  like the existing misses (`03`). The `row_id` doubles as the box lookup:
  it is validated against `jobs.payload`, never trusted as text.
- The bare routes keep serving the original. The export (`15`), third-party
  clients (`12`) and the server-side crop at confirm time are unaffected.
- An SVG product picture (a promoted icon) answers every variant with the
  SVG itself, with the same CSP and `nosniff` headers as today: a vector
  needs no derivative.
- Headers as the route beneath: `private, max-age=86400` for product
  variants, `private, max-age=3600` for job variants, `nosniff`,
  `Content-Type` from the derivative's own encoding. A photo-level variant
  is immutable. A row crop changes when a job is analysed again while its
  URL does not, so the client appends the job's `updated_at` as a `v`
  query parameter, which the server ignores.

### Frontend

- One shared helper, `js/images.js`: `thumbAttrs(url, cssPx)` returns
  `{src, srcset, sizes}` from the ladder — `src` the smallest size that
  covers 1×, `srcset` all three with `w` descriptors, `sizes` the CSS width;
  `variantURL(url, variant)` appends a variant and passes `null` and `.svg`
  URLs through untouched. Used by `product-table.js` (36 px),
  `pages/stocktake.js` (56 px), `pages/inbox.js` (72 px),
  `pages/shopping-list.js` (96 px) and the product detail in
  `pages/products.js` (128 px). Thumbnails gain `loading="lazy"` and
  explicit `width`/`height` so a table does not shift as they arrive.
- `review.js` and `consume-review.js` stop painting backgrounds. The
  `.review-crop` `<span role="img">` becomes an `<img>` with
  `object-fit: cover`, `src`/`srcset` from the row's `rows/{row_id}` variants,
  the `preview` variant for a row without a box, `?v=updated_at` on row
  URLs. The cutout comparison's "original" uses the same source.
- `CACHE_VERSION` in `web/static/sw.js` is bumped, `/js/images.js` joins
  `SHELL_ASSETS`, and `web/shell-manifest.json` is regenerated — every
  changed file is a shell asset (`CLAUDE.md`, "Invariants that fail
  silently").

### Invariants this must not bend (`03`, `04`, `18`)

- No client-supplied string becomes a path component: the variant is matched
  against the list, the `row_id` against the proposal, the source name is
  the server's own UUID. The derived filename is built from those alone.
- `404`, never `403`, for a picture the caller may not see, and no wording
  that distinguishes "not yours" from "not there".
- Lazy derivation runs after the access check, never before.
- Logs carry a job id or a UUID filename at most — never pixels, never the
  original name of anything.

## Acceptance criteria

- Opening a 20-item shelf proposal transfers under 1 MB of image data and
  issues **no request for the original photo**; the same for the
  consumption review.
- The inbox requests one thumbnail of ≤ 40 KB per card and never the
  original.
- A 200-row inventory table at 2× device pixels transfers ≤ 2 MB of
  thumbnails.
- A row crop is the box expanded to a square around its centre, clamped to
  the photo, never stretched; a box touching an edge yields a square shifted
  inside, a box taller than the photo is wide yields the photo's full
  height. (Geometry unit tests on the quadrant image already used by
  `internal/images`.)
- A variant is removed with its source through `uploads.Dir.Remove`; an
  orphaned derived directory is gone after one sweep.
- A missing variant is rendered on request and served; `n` concurrent first
  requests for one source decode it once (unit test with a counting decoder).
- After "Analyze again", the row crops reflect the new boxes and no
  `rows-*` file of the old proposal remains.
- A derivation failure never fails a job and never changes its status.
- Variant names outside the list, row ids outside the proposal, jobs
  without a photo: `404`. An SVG product picture answers every variant with
  the SVG.
- No upscaling: a `preview` of a source already within 1600 px serves the
  original bytes.
- Deriving one 50 MP photo's full set peaks below ≈ 350 MB (documented from
  the benchmark above, not a test), and no more than two derivations run at
  once.
- `go test ./...`, the e2e suite (with its `**/jobs/{id}/image` route stubs
  widened to `/image/**` and the `product-images/…` regexes extended) and
  `web/shell_manifest_test.go` pass.

## Implementation plan

One branch off `main`, in this order, each step green on its own:

1. `internal/images/derive.go` (+ tests): `boxShrink` for YCbCr by integer
   factor; `Thumb(img, n)`, `Fit(img, n)`, `SquareAroundBox(bounds, box)`;
   `PhotoSet(src) map[Variant][]byte` and `RowSet(src, boxes)`; the variant
   type and its allow-list; the "never upscale" rule.
2. `internal/uploads/derived.go` (+ tests): the layout above, `Write`,
   `Open`, `RemoveSource`, `SweepOrphans(sourceDir)`; `Dir` learns its
   derived area so `Remove` removes both.
3. `internal/derive` (+ tests): the worker with its two lanes and
   `singleflight` groups, `EnsurePhotoSet`, `EnsureRowSet`, `Schedule`,
   `WarmProducts`, `SweepOrphans`.
4. Hooks: `ingest.Service.work`, `consume.Service.work`, the shopping-list
   photo job, `productPictures.save`. Row set removal before rewrite.
5. Routes in `httpapi/jobs.go` and `httpapi/productimages.go`, router
   wiring, handler tests (the 404 matrix, content types, SVG passthrough,
   the lazy path).
6. `cmd/inventory/main.go`: construct the derived area under
   `/data/cache/derived`, hand it to the two `uploads.Dir`s and the worker,
   add the sweep and warm-up to `backgroundLoops`.
7. Frontend: `js/images.js`; the five thumbnail call sites; `review.js`,
   `consume-review.js`, `review.html`, `consume-review.html`
   (`span` → `img`); `components.css` (`.review-crop` as an image);
   `sw.js` version and asset list; `shell-manifest.json`.
8. Docs: promote this file to `docs/specs/43-image-derivatives.md`; add it
   to `00-overview.md`'s table and numbering; add the
   `/data/cache/derived/` row to `04`'s storage-areas table; note in `06`
   and `09` that review crops are served pre-rendered; note in `15` that the
   derived cache is not backed up and self-heals; the `imagecache` volume
   comment in `docker-compose.yml`; `CLAUDE.md`'s numbering line.
9. e2e: widen the image route stubs in `ingestion.spec.js` and
   `background-removal.spec.js`; extend the `product-images` regexes in
   `products.spec.js`; add the "no request for the original" assertion to
   the ingestion review journey.

Roughly 900 lines of Go including tests and 150 lines of JavaScript; one to
two days of agent work. The owner has said this change may ship from a
branch against `main` without the three-reviewer cycle.

## Choices the owner may want to make differently

Defaults are what the design above assumes.

1. **Crop framing** — *square around the box* (default), or the exact box
   with `object-fit: cover` (shows the centre of a tall item, loses its
   ends), or the exact box letterboxed (`contain`; whole item, smaller).
2. **The size ladder** — `96 / 192 / 384` squares and a `1600` preview
   (default). A `768` "large" square could be added later for a lightbox
   without touching anything else.
3. **The volume** — derivatives on `imagecache`, outside the backup
   (default), or on `uploads`, inside it (simpler to reason about on
   restore, 3–6 % larger archives, no self-healing needed).
4. **The original routes** — kept (default) or hidden behind the variants
   once nothing in the PWA uses them.
5. **Not proposed:** capping the stored original below 50 MP. It would make
   every step above cheaper, but `06` promises 50 MP to the vision model and
   the owner's photos are the irreplaceable part of the backup.

## Gate conditions

- The owner picks or confirms the four defaults above.
- Nothing technical gates this: no new dependency, no schema change, no
  migration, no change to any stored URL.

## How the numbers were produced

A 8660 × 5773 JPEG (`convert -size 8660x5773 plasma:fractal -blur 0x1
-quality 90`) run through a throwaway Go program using only
`image/jpeg` and `golang.org/x/image/draw` as vendored in `go.mod`, one
process per scenario, peak RSS read from `/proc/self/status` (`VmHWM`), and
`convert`/`-define jpeg:size=` for the ImageMagick column. Real phone JPEGs
are 4:2:0 rather than this file's 4:4:4, so their decode holds half the
pixels in memory; everything else is the same.
