# 26 — Location Quick-Create from Capture Screens

Depends on: [`02-data-model.md`](02-data-model.md) (`locations`),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md) (shared
tree view, shared review component, no-build-step constraint),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md) (location
CRUD endpoints, ingestion review UI),
[`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(resolution UI).

## Why this spec exists

Two review-style screens ask the user to attach a **location** to an item
while reviewing/resolving it: the ingestion review row
(`06-vision-shelf-ingestion.md`) and a shopping-list line's resolution
(`07-shopping-list-reconciliation.md`). The ingestion review row renders
through the shared row component, `js/review.js` (`05-frontend-pwa-foundations.md`);
the location field itself, like every field on the row, is set up by the
page module (`web/static/js/pages/review.js`), not by `js/review.js` itself.
The shopping-list resolution UI is its own page module
(`web/static/js/pages/shopping-list.js`) and does not use `js/review.js` at
all. A third screen that does use `js/review.js`, `09-consumption-logging.md`'s
manual correction, has no location field at all — consumption only ever
decrements an existing batch, and every batch already carries a location —
so it never had this gap to begin with; see "Scope" below.

Only the ingestion confirm endpoint (`06`) accepts a location **path** and
silently creates any node on it that doesn't exist yet. The shopping-list
resolve endpoint requires an existing `location_id` — by design, so the
same-storage id validation `06`'s confirm handler does ("creates any
newly-referenced `locations` nodes that were only proposed until now")
doesn't have to be reimplemented, audited, and kept in sync across two
handlers. The consequence is that the resolution screen has **no way at
all** to add a location the user needs but hasn't created yet — not only
when a storage's location tree is completely empty, but any time the
specific node needed (a new shelf, a new box) doesn't exist. Today the only
way out is to abandon the screen, navigate to `locations.html`, create it
there, and start over — discarding every other row's edits and any
in-flight upload job, because these review screens hold their state in
memory only until "Confirm" is pressed
(`05-frontend-pwa-foundations.md`: no client-side router, nothing persisted
before confirm).

This spec adds one small, shared escape hatch to the location field
wherever a page module renders one: a trigger that opens the real location
tree editor **without leaving the page**, so the user can create whatever
they need and land back on the exact screen they were on, every other
field's edits intact, with the new location immediately selectable.

## Scope

Applies to every location field a review/resolution page module renders
that actually exists: the row of `06`'s `review.html` and the resolution UI
of `07`'s `shopping-list.html`. `09`'s `consume-review.html` renders no
location field at all and is out of scope — see below. Implemented once per
page module, so both inherit the same trigger and the same single-GET
refresh rule `05` already applies to the three row actions
(accept/correct/reject).

No backend change. This is a frontend-only addition that composes two
endpoints `06` already defines: `GET`/`POST
/api/storages/{storage_id}/locations`. `06`'s own path-with-autocreate
behavior on ingestion confirm is unchanged and out of scope here.

Out of scope, each picked up by its own follow-up spec so this one ships as
a single reviewable PR:

- The same gap on the batch split/move picker (`06`, "One batch, one
  location"), on `products.html` — see
  [`28-batch-move-quick-create.md`](28-batch-move-quick-create.md).
- The equivalent for category fields, in the same manual-correction / New
  Item screens this spec covers — see
  [`27-category-quick-create.md`](27-category-quick-create.md), which
  generalizes the modal this spec introduces.

Also out of scope, but not picked up anywhere else, because there is nothing
to build: `09`'s `consume-review.html`. Consumption logging only ever
decrements an existing batch, and every batch already carries a location —
there is no point in that flow where a user would create a new one, and its
confirm body (`{row_id, decision, product_id, decrements}`,
`09-consumption-logging.md`) has no field a location id could travel through.
Wiring this spec's trigger into that screen would mean building UI the data
model has nowhere to send.

## The escape hatch

Every location field gets a persistent trigger next to it — shown whether
the tree is empty or not, because the same need reappears later, once other
locations already exist but the specific one the user wants still doesn't.
Label it `+ New location`, styled per `css/components.css` tokens (no
literal color values, per `05-frontend-pwa-foundations.md`).

Clicking it opens `js/tree.js` — the exact component `locations.html`
already uses, with the same interactions (expand/collapse, inline
add-child, rename, drag-and-drop re-parenting, the keyboard "move to…"
equivalent) — inside a native `<dialog>` (`dialog.showModal()`), scoped to
the current page's `storage_id`. This is a second embedding of an
already-shared component, not a new tree UI to build or keep in sync.

A modal was chosen over opening `locations.html` in a new tab because
`js/tree.js` is already built as an embeddable component rather than only
as a page, so embedding it costs nothing extra — while a new tab would need
its own cross-tab sync (`BroadcastChannel` or a `localStorage` `storage`
event, plus a `focus`/`visibilitychange` fallback for when that signal is
missed) just to get a created location back into the originating tab at
all. That's a second, weaker mechanism for a problem the same-document
modal doesn't have in the first place.

New module `js/location-modal.js`:

```js
// openLocationManager(storageId) -> Promise<{ createdIds: string[] }>
// Renders js/tree.js inside a <dialog>, scoped to storageId. Resolves when
// the dialog is dismissed — an explicit "Done", Esc, or backdrop click, all
// equivalent — with the ids of any locations created during that session,
// in creation order. Never navigates the page.
```

Superseded by [`27-category-quick-create.md`](27-category-quick-create.md),
which generalizes this module to categories: the file is renamed
`js/tree-modal.js` and the export becomes `openTreeManager(storageId,
{kind})`, with `{kind: "locations"}` as this spec's own call shape carried
over unchanged. The description above is this spec's own contract as
originally shipped; the current module name and signature are `27`'s.

Each page module calls it from a field's trigger — `openLocationField`
(`web/static/js/location-options.js`) — and on resolve:

- Re-fetches `GET /api/storages/{storage_id}/locations` **once** and
  refreshes every open location field's option list on the page from that
  one response — not once per field — so ten detected items on one
  ingestion job don't fire ten parallel requests when the modal closes.
- If `createdIds` has exactly one entry, preselects it in the field whose
  trigger opened the modal. Any other open fields just get the refreshed
  option list; their own current selection is untouched.
- If `createdIds` is empty (the user only looked, renamed, or reorganized),
  every field gets the refreshed tree with selections untouched — a no-op
  from the user's point of view besides the option list being current.

Opening or closing the modal never reloads the page, never re-fetches the
job/list being reviewed, and never touches any other field's in-progress
value — it is the same document throughout.

## Acceptance criteria

- In a storage with zero locations, from `06` review and `07` resolution
  alike (`09` is out of scope — see "Scope" above), the user opens the
  modal, creates a root location, closes the modal, and that location is
  immediately selectable — no page reload.
- In a storage that already has locations, the same trigger still works to
  add a further one the picker doesn't yet offer — the trigger is not
  conditional on an empty tree.
- Editing a different row's quantity, product, or expiry, then opening and
  closing the location modal (with or without creating something) on
  another row, leaves that first row's edit untouched.
- Creating a location through the modal uses the same `POST
  /api/storages/{storage_id}/locations` `06` already defines — no second
  creation path, no duplicated same-storage validation.
- Cancelling the modal (Esc or backdrop) without creating anything leaves
  every field's selection exactly as it was.
- Opening the modal from any one of several location fields on one screen
  and closing it once triggers one `GET
  /api/storages/{storage_id}/locations` call, not one per field.
- E2E: extend `e2e/specs/locations.spec.js` or the relevant flow's own spec
  (`ingestion.spec.js`, `shopping-list.spec.js`) to cover creating a location
  from within the review/resolution screen and completing that row's confirm
  against it, in a fixture storage seeded with zero locations.
