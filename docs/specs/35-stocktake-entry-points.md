# 35 — Stocktake Entry Points

Depends on: [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(pages, shared tree view), [`13-stocktake-and-audit.md`](13-stocktake-and-audit.md)
(the stocktake sheet, `last_audited_at`),
[`33-inventory-overview-table.md`](33-inventory-overview-table.md) (row
action), [`34-navigation-and-start-page.md`](34-navigation-and-start-page.md)
(navigation bar).

Amends: `13`, "Confirming the sheet", last paragraph. `stocktake.html`
without `?location=` becomes a location chooser instead of an error. The
sheet itself and both endpoints are unchanged. Also amends `16`, "The
product edit surface", which repeated the same stale claim that expiry
edits happen on the stocktake screen — corrected there to match this
spec's "Why this spec exists" section below.

## Why this spec exists

The stocktake sheet works, but reaching it depends on luck. Three problems
add up.

**1. The link on the product page is broken.** The stock card on
`products.html` ends with *"Quantities, locations and expiry dates are
edited on the stocktake screen"*, and it links to `stocktake.html?storage=…`
with **no `?location=`** (`js/pages/products.js`, `renderStockCard`).
`stocktake.js` stops before it loads anything, because without a location
there is no shelf to walk. It shows *"No location given. Pick one from the
locations tree."* That message is not a link and offers nothing to pick
from. From the user's side, stocktake simply does not work. The same card
also links every batch's "in stock" text to the bare `locations.html`, not
to the batch's own location.

The sentence itself is wrong as well. The stocktake sheet corrects
**quantities** only (`13`). Moving a batch is `06`'s split/move, and
editing a batch's expiry is `08`'s batch editor. Neither happens on the
stocktake sheet.

**2. The one working entry point is hidden.** `locations.html` shows a
"Stocktake" button beside every node (`13`, through the tree's
`renderDetail`). It works, but nothing leads a user there who has not
already found it.

**3. Nothing links to stocktake on its own terms.** There is no menu
entry, and no way to start from "which shelf haven't I checked in a
while?" That question is exactly what `last_audited_at` was added to
answer (`13`).

## Changes

### `stocktake.html` without a location: a chooser, not a dead end

When `?location=` is missing, the page stays usable. It resolves the
session as usual, fetches `GET …/locations`, and renders a **location
chooser**:

- It uses the existing tree component (`js/tree.js`) in **read-only mode**:
  no add-child, no rename, no drag. Each node shows the same `renderDetail`
  as `locations.html`, which is "audited 3 weeks ago" or "never audited"
  and a **Count** link to `stocktake.html?location={id}`. If `tree.js` has
  no read-only mode yet, this spec adds one as an option (`{ editable:
  false }`). It does not create a second tree.
- Above the tree is a **"Stalest first"** list: at most five locations that
  have never been audited or were audited longest ago, each with its Count
  link. This answers "where should I count next?" without making the user
  scan the whole tree. Never-audited locations sort before any audited
  one. Among the never-audited ones, tree order decides.
- If a storage has no locations at all, the page shows an empty state that
  links to `locations.html`.

The old message, *"No location given. Pick one from the locations tree."*,
is removed.

### `?location=` pointing at nothing

If a location id is unknown, belongs to another storage, or has been
deleted since the link was made, the sheet fetch answers `404` (`13`).
The page then shows the error **and** renders the chooser below it.
Leaving a broken bookmark is one click, not a trip to another page.

### The product page

On the stock card in `products.html`:

- Each batch row names its location path. The page fetches `GET
  …/locations` once, per product view, and resolves `batch.location_id`
  against it. The response already contains `location_id`
  (`internal/httpapi/batches.go`).
- Each batch row gains a **"Count this shelf"** link to
  `stocktake.html?location={batch.location_id}`.
- The line under the list is corrected to describe what the stocktake
  sheet actually does: *"Counts are corrected shelf by shelf on the
  stocktake sheet."*, where "stocktake sheet" links to the chooser. The
  claim about locations and expiry dates is removed. Those are edited
  where `06`/`28` and `08` put them, not here.
- The "in stock" link to the bare `locations.html` is removed. The
  location path and the Count link replace it.

### Everywhere else

- The navigation bar (`34`) has a **Stocktake** entry that opens the
  chooser.
- The inventory table (`33`) has a **"Count this shelf"** action on every
  row, which goes to that row's location.
- `locations.html` keeps its per-node Stocktake button unchanged.

The sheet's "Back" and "Cancel" links currently always go to
`locations.html`. They now go to **the page the user came from**, when
that page is one of this app's own pages: `document.referrer` is checked
for the same origin, and `locations.html?storage=…` remains the fallback.
Someone who started a stocktake from the inventory table then returns to
the inventory table, not to the tree.

## Out of scope

- Stocktake across a whole subtree ("count the entire basement"). `13`
  defines a stocktake as one physical shelf on purpose, and that stays.
- Any change to the stocktake API, its confirm rules, or the
  `last_audited_at` semantics.

## Acceptance criteria

- `stocktake.html?storage=…` with no location renders the chooser, with
  the audited state and a Count link per node. It never renders the old
  dead-end message.
- The chooser's "Stalest first" list shows never-audited locations first,
  then the oldest `last_audited_at`, with at most five entries.
- A `?location=` for a foreign or deleted location shows the `404` message
  and the chooser. The two cases look identical (`03`).
- On `products.html`, every batch row shows its location path and a working
  "Count this shelf" link that opens the sheet for that location. The stock
  card no longer claims that locations or expiry dates are edited on the
  stocktake sheet.
- The navigation bar's Stocktake entry opens the chooser.
- Back and Cancel on the sheet return to the same-origin page the user came
  from, or to `locations.html` when there is no such page.
- E2E fixture: this journey confirms stocktakes, which writes quantities
  and `last_audited_at`, so it gets a **dedicated storage and user**, for
  example "E2E Stocktake" and `e2e-stocktake`. The storage holds seven
  locations. One has never been audited. The other six have
  `last_audited_at` values spread over the past months, relative to
  `now()`. At least one location holds a batch.
- E2E on that storage:
  - Open a product with stock and press "Count this shelf" on a batch.
    Correct its quantity and confirm. The product page then shows the new
    quantity.
  - Open Stocktake from the navigation bar. The "Stalest first" list shows
    exactly five entries: the never-audited location first, then the four
    oldest audited ones, oldest first.
  - Confirm the first entry unchanged. Back in the chooser, that location
    reads "audited today" and has left the list.
  - Open `stocktake.html?location=` with a random UUID, and again with the
    id of a location in another fixture storage. Both show the same
    not-found message and the chooser below it.
  - Start a stocktake from `inventory.html` and press Cancel: the page
    returns to `inventory.html`. Open a sheet by direct URL, which gives no
    referrer, and press Cancel: the page goes to `locations.html`.
