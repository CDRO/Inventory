# 28 — Location Quick-Create on the Batch Move/Split Picker

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_batches`,
`locations`), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(`products.html`), [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
("One batch, one location" — split/move endpoints, and the picker built for
them on `products.html`'s batch list — see `#145`),
[`26-location-quick-create.md`](26-location-quick-create.md) (the modal
reused here).

## Why this spec exists

`26` closed the location-quick-create gap for the two review-style screens
that render through `js/review.js`: the ingestion review row and the
shopping-list resolution UI. The batch **split**
(`POST /api/storages/{storage_id}/inventory-batches/{id}/split`) and
**move** (`PATCH /api/storages/{storage_id}/inventory-batches/{id}`) picker
on `products.html`'s batch list (`06`, built for `#145`) has the identical
requirement — an existing `target_location_id` / `location_id` — but sits
outside `js/review.js` entirely: `products.html` is the product list and
detail/edit view (`05-frontend-pwa-foundations.md`), not a review screen, so
`26` does not cover it even though the underlying problem is the same one.
Moving three jars from the cellar to a kitchen shelf that hasn't been
created as a location yet currently means abandoning the split/move action,
creating the shelf on `locations.html`, and coming back to start the action
over.

## Scope

Reuses the modal `26` introduces, generalized to `js/tree-modal.js`'s
`openTreeManager(storageId, {kind})` by `27-category-quick-create.md`
(landed since wave 4): this calls it with `{kind: "locations"}`. **No new
modal component is built for this** — this spec is a further call site of an
already-shared piece of UI, not a new one.

Adds the same persistent `+ New location` trigger `26` defines beside the
target-location field of both the split form and the move form that `#145`
built on a product's batch list on `products.html` — each field is already a
plain `<select>` built through `js/location-options.js`
(`appendLocationOptions`/`refreshLocationOptions`), the same shape `26`'s
trigger already knows how to attach to and refresh via `openLocationField`.

No backend change — reuses `06`'s existing split/move endpoints and `06`'s
`GET`/`POST /api/storages/{storage_id}/locations` exactly as `26` does.

## Acceptance criteria

Trigger (this spec):

- From a product's batch list on `products.html`, splitting or moving a
  batch to a location that doesn't exist yet can be done by opening the
  modal, creating it there, and continuing the split/move action — no
  navigation away from `products.html`, and no loss of whatever quantity
  was already entered for the split.
- The refresh/preselect/single-fetch-per-close contract matches `26`'s
  exactly: the target-location field is refreshed once after the modal
  closes and preselects a single newly created location automatically.
- Cancelling the modal (Esc or backdrop) leaves the in-progress split/move
  form — quantity entered, batch selected — untouched.
- Creating a location from this picker uses the same `POST
  /api/storages/{storage_id}/locations` `06` and `26` already use — no
  third creation path.
- E2E: extend `e2e/specs/locations.spec.js` or a products-focused spec to
  cover creating a location from the split/move picker on `products.html`
  and completing the move/split against it.

Picker itself (`06`, built for `#145`, listed here because this is where the
batch list's acceptance criteria live):

- Splitting a batch by a quantity strictly below its current quantity
  creates a new batch at the chosen target location carrying the source
  batch's `expiration_date` and `expiration_source`, decrements the source
  batch by that quantity, and leaves the product's total quantity
  unchanged.
- Moving a whole batch (`PATCH .../inventory-batches/{id}` with
  `{location_id}`) keeps the same batch id and quantity, changes only its
  location, and leaves the product's total quantity unchanged.
- A split quantity the server rejects — at or above the source batch's
  current quantity — shows the server's error message and leaves the batch
  list unchanged: no new batch, no quantity change. The frontend never
  reimplements this bound client-side; the server is the only thing holding
  a lock on the row and therefore the only thing that knows it.
- Neither form reimplements same-storage validation of `target_location_id`
  or `location_id` — an id from another storage is rejected by the
  endpoint, `404`, exactly as `06` specifies, and the frontend just surfaces
  whatever the server answers.
