# 10 — Reorder & Minimum Stock Management

Implements PRD Feature 6. Depends on: [`02-data-model.md`](02-data-model.md)
(`products.min_stock`), [`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(dashboard urgency conventions it shares screen space with).

## Zero-quantity tracking

A product's current total stock is derived, not stored:
`SUM(inventory_batches.quantity) WHERE product_id = ...` (0 when it has no
batches at all — a product row can validly exist with zero batches, e.g.
after full consumption per `09-consumption-logging.md`). Expose this via
`GET /api/storages/{storage_id}/products/{id}` including a computed
`current_stock` field, and on the list endpoint
`GET /api/storages/{storage_id}/products` (each item includes
`current_stock`) to avoid N+1 calls from the frontend.

## Minimum threshold

`products.min_stock` (already in `02-data-model.md`), editable via the
product edit screen referenced in `08-expiration-and-classification.md`.
A product is **low stock** when `current_stock < min_stock` (strictly
less — equal to the threshold is not yet "low"), and **out of stock**
when `current_stock = 0`. Out-of-stock is a subset of low-stock whenever
`min_stock > 0`, but is called out separately in the UI since it's the
more urgent state.

## Dashboard highlights

`GET /api/storages/{storage_id}/dashboard/reorder` returns:

```json
{
  "out_of_stock": [ { "product_id": "018f...uuid", "name": "...", "min_stock": 2 } ],
  "low_stock": [ { "product_id": "018f...uuid", "name": "...", "current_stock": 1, "min_stock": 3 } ]
}
```

Rendered on `dashboard.html` (`05-frontend-pwa-foundations.md`) alongside
the expiring-soon widget from `08-expiration-and-classification.md`, as
two clearly separated sections reusing the urgency tokens from
`css/tokens.css` (out-of-stock uses `--urgency-expired`, low-stock uses
`--urgency-soon`).

## Adding a missing product from the reorder list

The reorder list is built from what the inventory knows about, which
misses the most ordinary case there is: **you are out of butter precisely
because you never had any to inventory.** So the list has an "Add item"
input where a product can be created by name, directly from the dashboard.

Typing a name and confirming runs the **same pipeline as
`07-shopping-list-reconciliation.md`** — reuse `internal/matching` and the
image-suggestion flow, do not build a second, thinner version of it:

1. **Stage 1 — local products.** If the name matches an existing product
   in this storage, do **not** create a duplicate. Offer instead to put it
   on the list by setting its `min_stock` (default 1 if unset) — the usual
   reason an existing product is missing from the reorder list is that its
   threshold is `0`, i.e. it was never tracked for reorder.
2. **Stage 2 — anonymous catalog (plus variants).** A hit pre-fills name,
   category path, item type, image, and shelf life for one-click
   acceptance, with **no Gemini and no SerpAPI call**.
3. **Stage 3 — image suggestions by name.** On a miss, the same
   `GET /api/storages/{storage_id}/image-suggestions?query={name}`
   endpoint returns the 1 icon + 2 product photos, cached and served from
   our own origin. The query here is the **typed product name** rather
   than an OCR'd shopping-list line — the only difference between this
   path and `07`'s, which is why nothing else needs to change.

The product is then created with:

- **no `inventory_batches` rows at all** — stock is `0` because there
  genuinely is none. It must not create a zero-quantity batch: batches
  represent physical things in a place, and there is no butter in any
  location (`02-data-model.md`).
- `min_stock` defaulting to `1`, so the product appears immediately in
  `out_of_stock` and in the export. A product created here with
  `min_stock = 0` would vanish from the very list it was created on.
- No `inventory_logs` row: nothing was purchased or consumed, so there is
  no quantity change to record.

When the item is later actually bought, it is stocked through the normal
paths (`06`, `07`, or the "Stocking up" capture mode in `09`), which
create the first real batch.

## Shopping list export

The export lists all `low_stock` + `out_of_stock` products (name, current
stock, min stock, suggested reorder quantity = `min_stock -
current_stock`, clamped to at least 1).

- **CSV — server-side.**
  `GET /api/storages/{storage_id}/dashboard/reorder/export?format=csv`,
  via Go's stdlib `encoding/csv`. Returned with
  `Content-Disposition: attachment` and a dated filename
  (`reorder-list-2026-09-04.csv`).
- **PDF — client-side**, generated in the browser from the same JSON the
  dashboard already fetched, using a **vendored single-file UMD build**
  (jsPDF plus its autotable plugin) committed under `web/static/vendor/`
  per the vendoring rule in `11-reporting-and-analytics.md` — pinned
  version in the filename, origin recorded in `vendor/README.md`, and no
  CDN at runtime. "No build step" does not mean "no libraries": a
  pre-built UMD file loaded with a plain `<script>` tag needs no
  toolchain, which is exactly why vendoring works here.
  - Pagination is the library's job — autotable repeats the header row and
    breaks pages on its own, so a long list is not a reason to move this
    to the server.
  - Doing it client-side keeps the PDF identical to what the user is
    looking at (same filtering and sort order, no second implementation to
    drift), and removes a Go PDF dependency from the static build.
  - The generated file is offered via a blob download with the same dated
    filename convention.
- **A server-side PDF endpoint is deliberately not built.** Add one only
  if a non-browser consumer appears (scheduled email, automation); until
  then it would be a second renderer of the same document, with all the
  divergence that implies.

This exported list is a **read-only export of the current low/out-of-stock
state** — it is not the same `shopping_lists` entity from
`07-shopping-list-reconciliation.md` and does not create one; it exists so
the user can take a physical/text list to a store. (A future "start a
shopping list from this export" convenience could bridge the two, but is
out of scope here — do not build it unless asked.)

## Acceptance criteria

- `current_stock` is always computed from live `inventory_batches` data,
  never a denormalized counter that could drift.
- A product with `min_stock = 0` never appears in `low_stock` or
  `out_of_stock` regardless of its actual stock — a zero threshold means
  "not tracked for reorder."
- The reorder dashboard and export reflect the same underlying query (no
  separate, possibly-inconsistent logic between the on-screen list and
  the exported file).
- Adding a product from the reorder list creates a `products` row with
  **zero batches** and `min_stock >= 1`, no `inventory_logs` row, and it
  appears in `out_of_stock` immediately.
- Adding a name that already exists in the storage adjusts that product's
  `min_stock` instead of creating a second product with the same name.
- The add-item flow issues no external API call when the catalog already
  describes the product.
