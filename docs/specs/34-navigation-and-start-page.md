# 34 — Navigation and a Per-Storage Start Page

Depends on: [`02-data-model.md`](02-data-model.md) (`storage_members`),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md) (membership,
non-disclosure, no admin navigation),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md) (routing,
session and storage resolution, the PWA manifest),
[`12-client-api-contract.md`](12-client-api-contract.md) (additive changes),
[`29-first-run-admin-guidance.md`](29-first-run-admin-guidance.md) (the
zero-storage path, which is unchanged).

Amends:
- `05`, "Session & storage resolution": once a storage is resolved,
  `storages.html` forwards to the start page instead of rendering a landing
  card.
- `05`, "PWA": `start_url` changes from `/index.html` to `/storages.html`.

Nothing in `03` or `29` is weakened.

## Why this spec exists

There is no navigation. After login, `storages.html` shows a card of
buttons (Dashboard, Locations, Categories, Products, Shopping list, Scan
photos). Every other page's header has one "Back" link, and it leads to
that card. Moving from the dashboard to the products page means going back
to the card first. Some pages, such as the inbox and the stocktake sheet,
cannot be reached from the card at all. Log out exists only on the card.

Opening the installed app is worse. The manifest's `start_url` is
`/index.html`, the login form. That page deliberately never checks for a
session (`js/pages/index.js`), so a user who is already signed in sees a
login form every time they open the app.

`storages.js`'s own header comment calls the card a stopgap ("until the
dashboard lands, this page also serves as the landing shell"). The
dashboard has since landed.

This spec replaces the card with a navigation bar on every page, and makes
where the app opens a per-storage choice. The default is the dashboard.

## The start page

### What can be chosen

| Value | Page |
|---|---|
| `dashboard` *(default)* | `dashboard.html` |
| `inventory` | `inventory.html` (`33-inventory-overview-table.md`) |
| `products` | `products.html` |
| `locations` | `locations.html` |
| `shopping_list` | `shopping-list.html` |
| `ingest` | `ingest.html`, for households that mostly open the app to photograph a shelf |
| `inbox` | `inbox.html` |

The list is closed. Pages that need a parameter to mean anything, such as
`review.html?job=` or `stocktake.html?location=`, cannot be chosen. Neither
can `settings.html`: it is not a place anyone starts their day.

### Where it is stored: per person, per storage

The start page belongs to **one person in one storage**: "when I open *this
household*, show me *this*". Two members of the same storage can choose
differently, and one person can choose differently for two storages. That
is exactly the key of `storage_members`, so a migration adds one column to
it:

```sql
ALTER TABLE storage_members
  ADD COLUMN start_page TEXT NOT NULL DEFAULT 'dashboard'
  CHECK (start_page IN ('dashboard', 'inventory', 'products', 'locations',
                        'shopping_list', 'ingest', 'inbox'));
```

This deliberately does **not** give storages roles.
`storage_members` still carries no role and no rights (`02`, `03`). The
column is a personal preference that happens to live on the row which
already means "this person, this storage". When an admin removes someone
from a storage, the row and the preference go with it. That is correct,
because there is nothing left to start on.

**Rejected alternative: `localStorage`, per device.** This would need no
backend change, and it would let a phone open on "Scan" while the desktop
opens on "Inventory". It was rejected because the preference would be
lost silently, and in exactly the situations people least want it to be.
The settings page's own "Reset local app data" action (`05`) clears it,
so does a new phone, and so does a browser that clears site data. The
user would then find the app opening on the dashboard again with no
explanation. The per-device idea remains possible later as an *override*
on top of this one, and nothing here closes that door.

### API

- `GET /api/auth/me`: each entry in `storages` gains `start_page`. This is
  an additive field (`12`), and it is the caller's own value for that
  storage. No other member's preference is ever returned.

  ```json
  { "storages": [ { "id": "018f…", "name": "Home", "start_page": "dashboard" } ] }
  ```

- `PATCH /api/storages/{storage_id}/membership`: body `{"start_page": "inventory"}`.
  It updates the **caller's own** `storage_members` row for that storage.
  It answers `200 {"start_page": "inventory"}` on success. A value outside
  the list above answers `422 validation_failed` with a `fields.start_page`
  message. Standard scoping applies: a non-member gets `404`, identical to
  an unknown storage (`03`). The route has no user id in it, so it cannot
  touch anyone else's row.

The resource is called `membership`, not `preferences`, on purpose.
`/api/me/preferences` already exists for the gamification opt-in (`51`),
and that endpoint is per user, not per storage. Using the same word for a
per-storage setting would invite someone to merge the two.

### Choosing it (`settings.html`)

The settings page gains a **"Start page for {storage name}"** select in
its storage-scoped section. That section is the same one where `17`'s
notification settings live, and it is shown only when a storage is
resolved. Changing the select saves at once with the `PATCH` and shows a
short "Saved" status, the same pattern as the display-name field. When a
user belongs to several storages, the select always concerns the storage
currently chosen in the switcher. Its label names that storage, so the
user can see which one they are changing.

## Opening the app

- `manifest.json`'s `start_url` becomes `/storages.html`. For a signed-in
  user, that page resolves the storage and forwards to its start page. For
  a signed-out user, its `GET /api/auth/me` gets a `401`, and `api.js`
  already redirects that to `/index.html`. The login form therefore still
  appears exactly when it is needed, and only then.
- After a successful login, `index.js` still goes to `/storages.html`, as it
  does today. That page is now the single place that decides where a user
  lands.
- `storages.html`, after resolving a storage (`05`'s rules, unchanged),
  **forwards with `location.replace`** to that storage's start page and
  carries `?storage=`. `replace` is used rather than `assign` so that Back
  from the dashboard leaves the app instead of bouncing through the
  forwarder. This is the same reasoning as the zero-storage redirect in
  `29`.
- The storage picker (more than one membership and nothing remembered) and
  the zero-storage path (`29`) are unchanged. `storages.html` renders only
  those two states now. The landing card is removed.
- Choosing a storage in the **picker** also forwards to that storage's
  start page. Switching storage with the **header switcher** still keeps
  the current page (`05`), because a user who is on the inventory table
  and switches household wants the other household's inventory table.

## Navigation bar

A new shared module, `js/nav.js`, renders the navigation on every
storage-scoped page. There is one implementation, not one per page. This
is the same rule `05` applies to the review component and the tree.

```js
// renderNav(container, { storageId, current, startPage })
// current: the page's own key, marked aria-current="page"
```

- **The header's "Inventory" wordmark becomes a link to the start page** of
  the current storage. This is the "home" link. It goes wherever the user
  chose, so it matches what opening the app shows.
- Below the header is a `<nav aria-label="Main">` with, in this order:
  **Dashboard**, **Inventory**, **Products**, **Locations**,
  **Categories**, **Shopping list**, **Stocktake**
  (`35-stocktake-entry-points.md`), **Scan**, and **Inbox** with its
  waiting-count badge. That badge is the existing `js/inbox-badge.js`, now
  inside the bar instead of placed separately on some pages. Every link
  carries `?storage=`.
- **Settings** and **Log out** sit at the end of the bar, visually
  separated from the rest. Log out moves here from `storages.html`, where
  it was the only way to sign out. The logout handler moves into
  `js/nav.js` unchanged.
- **No admin link, ever.** This is `03`'s rule, and `29` already routes an
  admin without a storage to the admin view on the server. `js/nav.js`
  receives nothing that would let it know who is an admin.
- **On a phone** the bar is a single row that scrolls horizontally, with
  the current item scrolled into view on load. It is not a hamburger menu:
  every destination stays one tap away and visible, which suits the
  one-handed use `05` requires. From the `36rem` breakpoint upwards it
  wraps normally.
- Every page's existing header "Back" link that pointed to
  `/storages.html` is removed, because the bar replaces it. Contextual back
  links that point somewhere specific stay. Examples are review and
  consume-review returning to the inbox, stocktake returning to locations,
  and settings returning to the dashboard (which becomes the start page).

Pages without a resolved storage do not render the bar. These are
`index.html`, the storage picker and the zero-storage empty state. There
would be nothing to link to without a storage, and showing links that
lead to empty states would imply storages exist that the user cannot see
(`05`, "Do not imply that other storages exist").

## Acceptance criteria

- The migration adds `storage_members.start_page` with default `dashboard`
  and the `CHECK` list above. Its down migration drops the column.
- `GET /api/auth/me` returns the caller's own `start_page` for each of
  their storages, and nobody else's.
- `PATCH …/membership` updates only the caller's own row. An invalid value
  answers `422`, and a non-member gets `404` identical to an unknown
  storage. Go tests cover all three. Another Go test gives two members of
  one storage different `start_page` values and checks that each member's
  `GET /api/auth/me` returns only their own value.
- An installed app opened by a signed-in user lands on that storage's start
  page without showing the login form. A signed-out user lands on the login
  form.
- With exactly one storage, logging in lands on the dashboard by default,
  and on the chosen page after it has been changed on `settings.html`.
- Back from the start page after forwarding does not return to
  `storages.html`.
- Every storage-scoped page renders the navigation bar with its own entry
  marked `aria-current="page"`. No page's JavaScript contains `/admin`.
- At a width of 375 px the bar scrolls horizontally, and the page body does
  not.
- E2E fixture: `start_page` is durable, so these journeys **must not change
  it for a user that other spec files log in as** (`e2e-alice`, `e2e-bob`).
  `seed.sql` gains dedicated users:
  - `e2e-start`, a member of one storage, who has its start page changed;
  - `e2e-start-multi`, a member of two storages, seeded with different
    `start_page` values (`inventory` in one, `locations` in the other), so
    that the journey reads them and never writes them.
- E2E: as `e2e-start`, log in and land on the dashboard. Change the start
  page to Inventory in settings, log out, log in, and land on
  `inventory.html`. As `e2e-start-multi`, pick each storage in the picker
  and land on that storage's own start page.
- **Existing test to update:** `e2e/specs/storage-switching.spec.js`
  currently asserts that picking a storage stays on `storages.html` and
  shows its name in `#main`. Under this spec the picker forwards to the
  start page, so that assertion changes to "lands on the dashboard with
  `?storage=` set". The rest of that test is unaffected. This includes
  switching storage through the header switcher, which still keeps the
  current page.
