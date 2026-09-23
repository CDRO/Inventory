# 33 — Inventory Overview Table

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_batches`,
`products`, `locations`, `categories`),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (pagination,
error envelope), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(file layout, styling, service-worker allowlist),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md)
("Sorting & filtering by urgency"),
[`13-stocktake-and-audit.md`](13-stocktake-and-audit.md) (`POST
/inventory-batches`, the stocktake sheet),
[`34-navigation-and-start-page.md`](34-navigation-and-start-page.md) (the
header navigation that links here).

Amends: `05`, "File layout" (a new page) and "Styling" (a wide layout
modifier). Also amends `08`, "Sorting & filtering by urgency": the
"inventory list view" it names is this page.

## Why this spec exists

No screen in the app shows **what is in the house**. `products.html` lists
product *types*: "Barilla Penne 500g" appears once, whether there are
twelve packets in three rooms or none at all. The location tree shows
where things *could* go. The stocktake sheet shows one shelf at a time. The
dashboard shows only what is running low. To answer "what do we have, and
where?", a user today opens every product one by one.

`08` has required an "inventory list view" with urgency sorting and
filtering since the core specs were written. No spec ever defined the page
or the endpoint behind it, so it was never built. This spec defines both.

## Endpoint

`GET /api/storages/{storage_id}/inventory-batches`: every batch in the
storage, one row per batch. This is the natural read partner of `13`'s
`POST` on the same collection.

```json
{
  "items": [
    {
      "id": "018f…",
      "product_id": "018f…",
      "product_name": "Barilla Penne 500g",
      "image_url": null,
      "category_id": "018f…",
      "category_name": "Pasta",
      "location_id": "018f…",
      "location_path": ["Basement", "Right Shelf", "Layer 2"],
      "quantity": 3,
      "expiration_date": "2027-01-10",
      "expiration_source": "derived",
      "created_at": "2026-09-20T10:14:03Z"
    }
  ],
  "next_cursor": null
}
```

- **The names are resolved on the server.** `product_name`, `image_url`,
  `category_name` and `location_path` (root to leaf) come back in the
  response. The alternative was sending ids and having the page join them
  against `/products`, `/categories` and `/locations`. That costs three more
  requests and repeats a tree walk in JavaScript that the database already
  does well. The stocktake sheet already resolves `product_name` on the
  server (`13`), and this endpoint follows it.
- **Pagination follows `04`**: cursor-based, `?limit=` defaulting to 50
  with a maximum of 200. It uses the shared id-only cursor
  (`internal/httpapi/pagination.go`), exactly like every other list
  endpoint. Rows are ordered by batch `id` (UUIDv7, so roughly by creation
  time), and the cursor is the id of the last row on the page. There is no
  composite sort key to carry across pages. Display order is the client's
  job anyway (see below).
- **Filters**, all optional and combinable:
  - `?location_id=`: batches at this location **or any descendant**. "What
    is in the basement" means every shelf in the basement. The stocktake
    sheet is the one place where only the node itself counts, because it
    mirrors one physical shelf (`13`).
  - `?category_id=`: batches whose product is in this category or any
    descendant.
  - `?q=`: a case-insensitive substring match on the product name, using
    the existing `idx_products_name_trgm` index.
- Every id in a filter is validated against the URL's storage. A foreign id
  or an unknown one answers `404`, and the two answers are identical (`03`).
- Batches with `quantity = 0` never exist (`13`), so the endpoint needs no
  zero filter.
- **Sorting by urgency is done by the client, not the server**, as `08`
  already requires. The same applies to sorting by name or location. The
  server's id order exists only so that the cursor is stable.

## Page (`inventory.html`)

A new page, `web/static/inventory.html`, with its module
`js/pages/inventory.js`.

### Loading

"The whole inventory" means the whole inventory. The page follows
`next_cursor` with `limit=200` until it is `null`, and only then renders.
A household's inventory is in the low thousands of batches (`00`, "Scale
expectations"), which is at most a handful of requests. Sorting and
filtering then happen in memory, without another round trip. While pages
are still arriving, the page shows a loading state that counts up ("Loading…
400 items so far").

If a later page fails, the page shows the error **and** says that the table
is incomplete. It never renders a partial table as though it were the whole
inventory.

### The table

One row per batch, using a real `<table>` with a `<thead>`:

| Column | Content |
|---|---|
| Product | Thumbnail (when `image_url` is set) and name. The name links to that product on `products.html`. |
| Category | `category_name`, or "—" |
| Location | `location_path` joined with ` › ` |
| Qty | `quantity`, right-aligned |
| Expires | The date, plus an urgency chip coloured from the `--urgency-*` tokens (`08`). A "you set this" marker when `expiration_source = 'user'`, the same wording as `products.html`. |
| | Row action: **"Count this shelf"**, which links to `stocktake.html?location={location_id}` (`35-stocktake-entry-points.md`) |

- **Sortable columns**: product, location, quantity and expiry. Clicking a
  header toggles ascending and descending, and `aria-sort` shows the current
  state. The default is **expiry ascending with nulls last**, the same
  default `08` sets for the dashboard's expiring-soon widget. Whatever is
  about to go off is at the top.
- **Filters**, above the table:
  - A text box that filters on product name as the user types. This runs
    against the loaded rows, not through `?q=`.
  - A location select, filled from `GET …/locations`. Choosing a node shows
    that node **and its descendants**.
  - A category select, which works the same way.
  - Urgency toggles: *Expired*, *Critical*, *Soon*, *OK*, *No expiry*. By
    default all of them are on.

  Filters are applied on the client to the full loaded set. The API
  filters exist so that native clients (`12`) can ask for less, not so
  that this page can.
- **Summary line** above the table: *"238 items in 91 batches"*, calculated
  from the rows the current filters show. "Items" is the sum of quantities
  and "batches" is the row count. The wording follows the analytics total
  on the dashboard (`11`).
- **Group by product**: a toggle that folds the rows into one row per
  product, with the total quantity, the number of locations, and the
  earliest expiry. Expanding a folded row shows its batches. The toggle is
  off by default. It is state in the page only, because it controls how the
  page looks, not a preference.
- **Empty states**: one for a storage with no stock at all, which suggests
  scanning a shelf, and a different one for "no rows match these filters",
  which offers a "Clear filters" action. The two are never the same
  message.

The page's filter and sort state is written to the query string with
`history.replaceState`, for example `inventory.html?storage=…&sort=expiry&loc=…`.
A filtered view therefore survives a reload and can be shared as a link,
which is the rule `05` sets for every page.

### Width on large screens

`.shell` is capped at `40rem` (`css/base.css`). That width suits forms and
review rows and is too narrow for six columns. This spec adds one layout
modifier to `base.css`:

```css
.shell--wide { max-width: 80rem; }
```

`inventory.html` puts it on **both** its `<header>` and its `<main>`, so the
header stays aligned with the table beneath it. On a wide screen the table
is therefore up to 1280 px wide, well past the 800 px the page needs to be
readable.

Below the existing `36rem` breakpoint (`components.css`) the table
changes layout rather than scrolling sideways. Each row becomes a stacked
card: the product name as the heading, location and quantity on one line,
and the expiry chip beneath. The column headers are visually hidden, and
sorting moves into a `<select>`. This uses CSS only, with no second
template.

No other page is widened by this spec. Where a page would benefit from
`.shell--wide`, that is a later change.

### Service worker

`/inventory.html` is added to `sw.js`'s `CACHEABLE_EXACT` allowlist and to
the shell precache list (`05`, "The cache-first path is an allowlist").
The data from `/api/…/inventory-batches` is never cached, just like every
other `/api/*` response.

## Out of scope

- Editing on this page. Quantities are corrected through the stocktake
  sheet (`13`), and products through `products.html` (`16`). The table only
  reads data and links to those screens. A table where quantities can be
  edited inline would be a second correction path next to `13`'s, and `13`
  deliberately allows only one.
- Exporting the table. `15-backup-restore-and-export.md` already provides a
  per-storage export.

## Acceptance criteria

- `GET …/inventory-batches` returns every batch of the storage, and none
  from any other storage, with `product_name`, `category_name` and
  `location_path` resolved. It paginates per `04`.
- `?location_id=` and `?category_id=` include descendants. A foreign or
  unknown id answers `404`, and the two answers are identical.
- Go tests cover descendant filtering, the storage scoping of every filter
  id, and a cursor walk across two pages that neither duplicates nor drops a
  row.
- `inventory.html` loads every page before it renders. If a later page
  fails, the page says the table is incomplete.
- The default sort is expiry ascending, nulls last. Every sortable column
  sets `aria-sort`.
- The location filter includes descendants. Filter and sort state
  round-trips through the query string across a reload.
- At a viewport of 1440 px the table is wider than 800 px. At 375 px the
  page has no horizontal scroll.
- `/inventory.html` is in `sw.js`'s allowlist. No `/api/*` response is
  cached.
- E2E fixture: `seed.sql` gains a **dedicated read-only storage and user**,
  for example "E2E Inventory" and `e2e-inventory`. No other E2E spec may
  write to it. It holds:
  - a nested location, `Cellar › Shelf A`, so that a reversed path and
    descendant filtering both show;
  - one expired batch, one batch with no expiry, and one batch expiring in
    more than 14 days, with dates computed relative to `now()` in the seed so
    that they do not go stale;
  - two batches of one product at different locations, for grouping.
- E2E on that storage:
  - Every seeded batch is listed. The location column reads `Cellar › Shelf
    A`, root first.
  - The default order puts the expired batch first and the batch with no
    expiry last.
  - Filtering by `Cellar` shows the `Shelf A` batch. The summary line then
    counts only the rows it shows.
  - Grouping by product folds the two batches into one row with the summed
    quantity.
  - A filter that matches nothing shows the "no rows match" empty state
    with "Clear filters", which is a different message from the empty
    storage one. The empty-storage state is checked on a storage with no
    batches.
  - With route interception failing the second page request (`limit` set
    low enough to force two pages), the page says the table is incomplete.
  - "Count this shelf" on a row opens that location's stocktake sheet.
