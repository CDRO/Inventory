# 16 — Product Maintenance: Edit, Merge, Delete

Depends on: [`02-data-model.md`](02-data-model.md) (`products`,
`inventory_batches`, `inventory_logs`, `tombstones`),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(matching service, image promotion),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(shelf-life cascades), [`12-client-api-contract.md`](12-client-api-contract.md)
(tombstones for delta sync).

## Why this spec exists

Vision ingestion and shopping-list reconciliation *will* create
near-duplicates — "Barilla Penne" one week, "Penne Barilla 500g" the
next — and `06-vision-shelf-ingestion.md` explicitly leaves duplicate
reconciliation to a human. But once two `products` rows exist, no spec
gives the human a tool: there is no merge, no complete edit surface, and
delete semantics are implied only by FK clauses. Meanwhile `08` refers
to "a product edit screen" that no spec fully defines, and notes that
changing `products.default_shelf_life_days` "has no endpoint yet." This
spec is that screen and those endpoints.

## The product edit surface

`products.html` (already in the file layout of
`05-frontend-pwa-foundations.md`) is the product list plus a detail/edit
view per product. The detail view shows: name, image (with the change
paths from `07` — suggestion picker, custom upload), category, item
type, `min_stock`, the storage-local shelf-life override, current stock
with its batches (location, quantity, expiry — linking to the batch
editing defined in `06`/`08`/`13`), and this product's recent
`inventory_logs`.

`PATCH /api/storages/{storage_id}/products/{id}` accepts, individually
or together:

- `name` — free text. Before saving a rename, the frontend runs the
  matching service's stage 1 against the new name and, on a close hit,
  shows the existing product with a "merge instead?" affordance. This is
  a courtesy, not a server rule: the server never blocks a rename over
  similarity, because two genuinely different products can share close
  names.
- `category_id` — must belong to this storage (`404` otherwise).
  Re-filing recomputes the batches' `derived` dates per `08`, in the
  same transaction, silently (as `08` already specifies).
- `item_type` — one of the three values from `02-data-model.md`.
- `min_stock` — integer ≥ 0 (`10-reorder-and-shopping-export.md`
  semantics).
- `default_shelf_life_days` — a whole number of days `0`–`36500`, or
  `null` to fall back down the resolution chain. This is the endpoint
  `08` notes as missing. Like the category rule change it mirrors, it
  runs the derived-date cascade for this product's batches inline and
  returns `{default_shelf_life_days, recomputed_batches}` — it is a
  change made *in order to* move dates, so the count is reported.
- `icon_name` — set or clear.

Unknown fields are `422`, not ignored. Every write bumps `updated_at`
(`02-data-model.md`).

## Merging two products

`POST /api/storages/{storage_id}/products/{id}/merge` — body
`{source_product_id}`. The URL names the **survivor**; the body names
the duplicate that disappears into it. Both must belong to this storage
(`404` otherwise); merging a product into itself is `422`.

One transaction:

1. **Re-point history and references** from source to survivor:
   `inventory_batches.product_id`, `inventory_logs.product_id`,
   `shopping_list_items.matched_product_id`, and — when
   `20-barcode-recall.md` is implemented — `product_barcodes.product_id`
   (a barcode already on the survivor wins on conflict; the source's
   duplicate row is dropped).
2. **The survivor's fields win, unchanged.** Name, image, category,
   item type, `min_stock`, shelf-life override — nothing is copied from
   the source. A merge is "these were the same thing all along, and
   *this* is its description"; field-mixing rules would turn a one-click
   cleanup into a form.
3. **Recompute the moved batches' `derived` expiry dates** under the
   survivor's rules (`08`): `derived` means "follows the current rules,"
   and the rules just changed for those batches. `user` dates are
   untouched, as always.
4. **Delete the source row** and write a `tombstones` entry
   (`entity_type = 'product'`) so delta-sync clients drop it
   (`12-client-api-contract.md`). Its permanent image file is removed
   unless another product references the same file
   (`07-shopping-list-reconciliation.md` deletion rule).
5. Bump the survivor's `updated_at`.

A merge writes **no `inventory_logs` rows**: no quantity changed. The
survivor's history is simply the union of both histories, each row
keeping its own timestamp and reason — which is exactly what reporting
(`11`) should see, since the two names always were one product.

The catalog is **not touched**: `catalog_products` rows are insert-only
and describe what was once claimed, not the current state of any
storage (`02-data-model.md`). The survivor keeps its own `catalog_id`;
the source's is discarded with it.

## Deleting a product

`DELETE /api/storages/{storage_id}/products/{id}`:

- Allowed even with stock on hand — the schema already says so
  (`inventory_batches.product_id ON DELETE CASCADE`), and "this was
  never a thing we should track" legitimately includes things currently
  on a shelf. The frontend must confirm, stating the current stock and
  that history goes with it.
- Deletion cascades batches and logs (per the FKs in `02-data-model.md`),
  writes a `tombstones` row, and removes the permanent image file unless
  shared. It writes no `inventory_logs` rows — the ledger for this
  product is being erased, not appended to. Erasure is the semantic
  difference from consuming-to-zero (`09`), which keeps the product and
  its history.
- The catalog row it may have seeded is unaffected, as everywhere.

## Acceptance criteria

- `PATCH` with `default_shelf_life_days` recomputes exactly the
  `derived` batches of that product, returns the count, and never
  touches `user` dates; `null` resumes the chain from `08`.
- A merge preserves the survivor's total stock as the sum of both
  products' prior stocks, preserves every `inventory_logs` row of both
  (now under one `product_id`), and creates no new log rows.
- After a merge, the source id yields `404` on every route, appears in
  the delta-sync `deleted` array, and its image file is gone unless
  shared; the survivor's fields are byte-identical to before the merge
  except `updated_at`.
- Merged-in `derived` batches carry dates recomputed under the
  survivor's rules; merged-in `user` dates are unchanged.
- Merge source and target must both be in the URL's storage; any
  foreign or unknown id is `404`, self-merge is `422`.
- Deleting a product removes its batches and logs, writes a tombstone,
  and leaves `catalog_products` untouched.
- Unknown fields in `PATCH` are rejected with `422`.
