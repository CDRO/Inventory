# 28 — Location Quick-Create on the Batch Move/Split Picker

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_batches`,
`locations`), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(`products.html`), [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
("One batch, one location" — split/move endpoints),
[`26-location-quick-create.md`](26-location-quick-create.md) (the modal
reused here).

## Why this spec exists

`26` closed the location-quick-create gap for the three review screens that
render through `js/review.js`. The batch **split**
(`POST /api/storages/{storage_id}/inventory-batches/{id}/split`) and
**move** (`PATCH /api/storages/{storage_id}/inventory-batches/{id}`)
pickers on `products.html` have the identical requirement — an existing
`target_location_id` / `location_id` — but sit outside `js/review.js`
entirely: `products.html` is the product list and detail/edit view
(`05-frontend-pwa-foundations.md`), not a review screen, so `26` does not
cover it even though the underlying problem is the same one. Moving three
jars from the cellar to a kitchen shelf that hasn't been created as a
location yet currently means abandoning the split/move action, creating the
shelf on `locations.html`, and coming back to start the action over.

## Scope

Reuses the modal `26` introduces. If `27-category-quick-create.md` has
already generalized it to `js/tree-modal.js`'s `openTreeManager(storageId,
{kind})`, this calls it with `{kind: "locations"}`; if `27` hasn't landed
yet, this calls `26`'s `js/location-modal.js`'s `openLocationManager`
directly. Either way, **no new modal component is built for this** — this
spec is the third call site of an already-shared piece of UI, not a new
one.

Adds the same persistent `+ New location` trigger `26` defines beside the
target-location field of both the split action and the move action on a
product's batch list on `products.html`.

No backend change — reuses `06`'s existing split/move endpoints and `06`'s
`GET`/`POST /api/storages/{storage_id}/locations` exactly as `26` does.

## Acceptance criteria

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
