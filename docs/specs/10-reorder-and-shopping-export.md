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

## Shopping list export

`GET /api/storages/{storage_id}/dashboard/reorder/export?format=csv|pdf`
generates a downloadable file listing all `low_stock` + `out_of_stock`
products (name, current stock, min stock, suggested reorder quantity =
`min_stock - current_stock`, clamped to at least 1).

- **CSV:** plain comma-separated, one row per product, via Go's stdlib
  `encoding/csv` — no dependency needed.
- **PDF:** a simple tabular layout via a pure-Go PDF library
  (`github.com/go-pdf/fpdf`) so the `CGO_ENABLED=0` static build in
  `01-architecture-and-deployment.md` keeps working. No client-side PDF
  generation — the frontend has no build step or dependency mechanism to
  support one.
- Both formats return with `Content-Disposition: attachment` and an
  appropriate filename (e.g. `reorder-list-2026-09-04.csv`).

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
