# 16 — Product Maintenance: Edit, Merge, Delete

Depends on: [`02-data-model.md`](02-data-model.md) (`products`,
`inventory_batches`, `inventory_logs`, `tombstones`),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(matching service, image promotion),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(shelf-life cascades), [`12-client-api-contract.md`](12-client-api-contract.md)
(tombstones for delta sync), [`39-batch-containers.md`](39-batch-containers.md)
(the container field on the batch list, "The product edit surface"),
[`40-icon-picker.md`](40-icon-picker.md) (`icon_name`'s picker, replacing
the free-text field below).

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
with its batches (location, quantity, expiry, and — per `06`'s "One batch,
one location — and how to split one" — an inline split/move picker calling
`06`'s split and move endpoints directly), and this product's recent
`inventory_logs`. Quantity corrections are a separate surface, the
stocktake sheet (`13`), which this screen links to per batch (`35`);
moving a batch is this screen's own split/move picker (`06`), and editing
a batch's expiry is `08`'s job — neither happens on the stocktake sheet.

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
- `icon_name` — set or clear, via the icon picker
  ([`40-icon-picker.md`](40-icon-picker.md)), not typed free text.

Unknown fields are `422`, not ignored. Every write bumps `updated_at`
(`02-data-model.md`).

*(Extended by [`39-batch-containers.md`](39-batch-containers.md): the
batch list above gains a container field per batch, with its own
rename/clear/destroy affordances — nothing above changes, `39` only adds
to what a batch row shows.)*

## The product list

`products.html`'s list defaults to **filter-only**: a search box, a "+
Add product" button (below), and a "Show all products" button — no
products rendered until the person searches or explicitly asks to see
everything. This replaces today's behavior of rendering every product as
a full-width button on load and only ever narrowing that list, never
gating it — for a household with more than a handful of products, that
default list is exactly what makes the page slow to use, per the
complaint this section exists to fix.

- Typing in the search box queries client-side over the already-loaded
  product set for short lists, but for a storage with the wide layout's
  reason to exist (see `05-frontend-pwa-foundations.md`, "Layout width")
  this is a filtered `GET` request built the same way every other
  filtered-and-reloaded listing on this system already is
  (`33-inventory-overview-table.md`'s filter/sort/reload pattern is the
  template) — as-you-type, debounced, replacing the empty state with
  matching rows.
- "Show all products" loads and renders every product in the storage,
  the same request the old default page load made — a person who
  genuinely wants to browse everything still can, in one click, without
  guessing a search term first.
- Once either the filter has results or "show all" has been used, the
  list renders as a **table**, not a button per product: same row shape
  and CSS as `inventory.html`'s table (`33-inventory-overview-table.md`)
  reuses — thumbnail/icon, name, category, current total stock — each
  row a link into that product's detail view, replacing today's
  `btn btn--block btn--ghost` button list. Reusing `inventory.html`'s
  table markup/CSS classes directly (not a second table implementation)
  is the point: this is "make it look like the inventory page," not "make
  something table-shaped."
- The narrow-viewport stacked-card fallback `33` already defines for its
  own table applies identically here — one table component, one
  responsive behavior, shared rather than reimplemented per page.
- Clearing the search box returns to the filter-only empty state, not
  back to "show all" — "show all" is a person's explicit choice each
  time, not a state the page remembers.
- `products.html` uses the `.shell--wide` layout, per
  `05-frontend-pwa-foundations.md`'s amended default (cross-reference,
  not a restatement — `05`, "Layout width," has the actual rule).

## Creating a product

Until now, creating a product manually was reachable only by way of
shopping-list resolution's `describeManually()` path
(`07-shopping-list-reconciliation.md`) — there was no "+ Add product"
anywhere for someone who wants to add a product without first typing a
shopping-list line. `products.html`'s list (above) gains one: a "+ Add
product" button opening the same manual-entry form shopping-list
resolution already uses (name, category, item type, `min_stock`, and the
optional picture picker from `07` — a picture has never been required,
at either the frontend or `internal/httpapi/shoppinglists.go`'s
validation, and stays optional here for the same reason: "If the provider
is unreachable, the product is created without a picture rather than the
confirm failing," `07`).

`POST /api/storages/{storage_id}/products` — body `{name, category_id?,
item_type?, min_stock?, image?, icon_name?}`. Only `name` is required
(`422` otherwise, the same rule `internal/httpapi/shoppinglists.go`
already enforces for its own manual-entry path). Runs the matching
service's stage 1 (`07`) against `name` before inserting and, on a close
hit, returns the existing product instead of a new `409`-shaped
duplicate — the same "merge instead?" courtesy the rename path below
gives, applied at creation time instead of after the fact. No initial
batch is created — a product can exist with zero stock, and adding its
first batch is the ordinary stocktake/"found stock" path
(`13-stocktake-and-audit.md`) or a vision-ingestion confirm
(`06-vision-shelf-ingestion.md`), not this endpoint's job.

This does not replace shopping-list resolution's own manual-entry
path — that flow still creates a product inline as part of resolving a
line, and still needs to (`07`). It is the same underlying operation
either way; a later refactor may have `07`'s handler call this endpoint
directly rather than duplicating the insert logic, but that is an
implementation detail this spec does not mandate.

## Merging two products

`POST /api/storages/{storage_id}/products/{id}/merge` — body
`{source_product_id}`. The URL names the **survivor**; the body names
the duplicate that disappears into it. Both must belong to this storage
(`404` otherwise); merging a product into itself is `422`.

One transaction:

1. **Re-point history and references** from source to survivor:
   `inventory_batches.product_id`, `inventory_logs.product_id`,
   `shopping_list_items.matched_product_id`, and
   `product_barcodes.product_id` (`20-barcode-recall.md`) — a barcode
   already on the survivor wins on conflict; the source's duplicate row
   is dropped.
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
- `POST /api/storages/{storage_id}/products` requires only `name`;
  omitting `image` and `icon_name` succeeds and creates no batch. Omitting
  or blanking `name` itself is `422`, the same rule
  `internal/httpapi/shoppinglists.go`'s manual-entry path already
  enforces.
- Creating a product whose `name` closely matches an existing one
  (matching service stage 1) returns the existing product rather than a
  duplicate.
- `products.html` renders no products on load — only after a search
  query or "Show all products" — and the rendered list is the shared
  table component `inventory.html` also uses, never the old
  button-per-product list.
