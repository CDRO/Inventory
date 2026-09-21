# 27 — Category Quick-Create from Capture Screens

Depends on: [`02-data-model.md`](02-data-model.md) (`categories`),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md) (shared
tree view), [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(manual correction → new product), [`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(New Item flow), [`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(category CRUD endpoints, seed tree, shelf-life rule),
[`26-location-quick-create.md`](26-location-quick-create.md) (the modal
generalized here).

## Why this spec exists

Manual product creation inside a review screen needs a `category_id`: `06`'s
"manual correction" (a new product from a review row,
`new_product.category_id`) and `07`'s "New Item, no catalog hit" stage-3
fill-in (`category_id`) both require an **existing** id — unlike `07`'s
stage-2 catalog-hit path, which resolves a `category_path` and creates any
missing nodes automatically. Whenever the category a user needs isn't in
the tree yet, there is no way to add it without leaving the screen — the
same trap `26` closed for locations, just not yet for categories.

The gap is rarer in practice than the location one: every storage is seeded
with a starter category tree at creation
(`08-expiration-and-classification.md`: `Food`, `Food → Dairy`,
`Food → Produce`, `Food → Meat`, `Food → Canned`, `Household`,
`Collectibles`), so a totally empty tree never happens the way an empty
location tree does. But "the specific category I need isn't in the starter
set" — `Food → Frozen`, say — is an entirely ordinary case, and today it
has the identical no-escape-hatch problem `26` describes for locations.

## Scope

Generalizes `26`'s modal instead of duplicating it: `js/tree.js` already
renders locations and categories identically
(`05-frontend-pwa-foundations.md`), including each category node's
shelf-life-rule detail via its `renderDetail` callback. `js/location-modal.js`
becomes `js/tree-modal.js`, and its exported function becomes:

```js
// openTreeManager(storageId, {kind: "locations" | "categories"}) -> Promise<{ createdIds: string[] }>
// Same contract as 26's openLocationManager, parametrized by which tree
// js/tree.js renders inside the <dialog>. Never navigates the page.
```

`js/review.js`'s existing location-field call site becomes
`openTreeManager(storageId, {kind: "locations"})`. A new call site is added
beside every `category_id` field in the manual-correction / New Item UI,
using `{kind: "categories"}`. A screen with both a location field and a
category field open only ever has one tree active per modal session — the
dialog shows whichever `kind` its trigger was opened with.

No backend change — reuses `08`'s existing `GET`/`POST
/api/storages/{storage_id}/categories`. Creating a category through the
modal goes through that same endpoint, so its existing `category_created`
gamification contribution (`51-gamification-scoring.md`, where that
deployment gate is enabled) and its optional `default_shelf_life_days`
record exactly as they would from `categories.html` — nothing bypasses
them, because nothing new is built to create a category; this spec only
adds a second place to call the same endpoint from.

Refresh, preselect-on-single-create, and single-fetch-per-close behavior
are identical to `26`'s, scoped per `kind` — a `categories`-kind field never
refreshes from a `locations` fetch or vice versa.

## Acceptance criteria

- From `06`'s manual-correction UI and from `07`'s stage-3 "New Item, no
  catalog hit" fill-in, with the category the user needs absent from the
  tree, the user opens the modal, adds it as a child of an existing node
  (e.g. under `Food`), closes the modal, and it is immediately selectable —
  no page reload, no loss of any other field already filled in on this or
  another row (product name, quantity, location, expiry).
- The shelf-life-rule detail shown next to each node inside the modal
  behaves identically to `categories.html` — it is the same `js/tree.js`
  render, not a stripped-down copy.
- Creating a category through the modal uses `POST
  /api/storages/{storage_id}/categories` — no second creation path — and
  its shelf-life default and gamification contribution behave exactly as
  they do from `categories.html`.
- The location field on the same screen, now driven by the same
  generalized module with `{kind: "locations"}`, keeps behaving exactly as
  `26` specifies — this refactor changes the module's shape, not its
  location behavior.
- E2E: extend `e2e/specs/categories.spec.js` (or the relevant flow's own
  spec) to cover adding a category from within the manual-correction / New
  Item screen and completing that row's confirm against it.
