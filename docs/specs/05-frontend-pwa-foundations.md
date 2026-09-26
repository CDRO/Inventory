# 05 — Frontend & PWA Foundations

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md).

## Hard constraint: no toolchain, no framework, no build step

The frontend is **plain HTML, hand-written CSS, and vanilla JavaScript ES
modules**, served to the browser byte-for-byte as it exists in the
repository. Explicitly forbidden: Node.js, npm/yarn/pnpm, `package.json`,
React, Next.js, Vue, Svelte, TypeScript, JSX, Tailwind, Sass/Less,
webpack/vite/esbuild/rollup, and any other step that transforms source
before it reaches the browser.

Everything therefore relies on what browsers ship natively: ES modules
(`<script type="module">`), `fetch`, `<template>`, custom elements where
useful, CSS custom properties, CSS grid/flexbox, and the service-worker
API. Any library that is genuinely needed must be a **single vendored
file** committed under `web/static/vendor/` — no package manager, and no
runtime CDN reference (the app must work on a LAN-only NAS with no
internet egress; see `11-reporting-and-analytics.md`).

This file covers the **user-facing app only**. The admin UI is not part of
it: it is server-rendered Go templates, and the JS app contains no admin
code at all — see `03-auth-and-multi-tenancy.md`.

## File layout

```
web/static/
├── index.html                 # login / storage entry point
├── storages.html              # storage picker (only if >1 membership)
├── locations.html             # location tree (06)
├── categories.html            # category tree (02, 08)
├── products.html              # product list + detail/edit
├── ingest.html                # camera entry point, sticky mode selector (06, 09)
├── shopping-list.html         # list reconciliation (07)
├── review.html                # ingestion proposal review/confirm (06)
├── consume-review.html        # consumption proposal review/confirm (09)
├── dashboard.html             # reorder + analytics (10, 11)
├── inventory.html             # every batch, sortable table (33)
├── css/
│   ├── tokens.css             # design tokens as CSS custom properties
│   ├── base.css               # reset, typography, layout primitives
│   └── components.css         # buttons, cards, tree, review list, tables
├── js/
│   ├── api.js                 # fetch wrapper: base path, errors, JSON
│   ├── session.js             # current user + storage resolution
│   ├── storage-switcher.js    # header control
│   ├── jobs.js                # background-job polling helper
│   ├── review.js              # shared proposal-review component
│   ├── image-picker.js        # shared picture-suggestion picker (07, 16)
│   ├── tree.js                # shared tree view (locations + categories)
│   ├── dom.js                 # small helpers: el(), render templates, escape
│   ├── i18n.js                # loads the active catalog, translates data-i18n (19)
│   ├── nav.js                 # shared navigation bar + logout (34)
│   ├── photo-picker.js        # library-or-camera photo picker + selection list (36)
│   ├── camera.js              # in-page viewfinder; falls back to the picker (37)
│   └── pages/                 # one module per HTML page, imported by that page
├── i18n/
│   ├── en.json                # English catalog (19)
│   └── de.json                # German catalog (19)
├── vendor/                    # single-file vendored libraries (see 11)
├── icons/                     # PWA icons (192, 512)
├── manifest.json
└── sw.js                      # hand-written service worker
```

- *Amended by [`19-localization.md`](19-localization.md):* every page
  imports `js/i18n.js` before rendering anything of its own, the same way
  every page already imports `js/register-sw.js` first. `js/i18n.js`
  fetches the active catalog from `i18n/en.json` or `i18n/de.json` and
  translates every `data-i18n` element already on the page. Nothing else
  above changes.

## Routing: plain multi-page, no client-side router

Each screen is a real HTML file with its own `<script type="module">`
entry from `js/pages/`. There is no SPA router, no history manipulation,
and no client-side route table — navigation is ordinary links, which the
browser and the service worker already handle well.

The active storage is carried in the query string
(`products.html?storage=3`), read once at page load by `session.js`, and
appended to every API call. It is never held in ambient global state, so a
link is always sufficient to describe where the user is.

## API client (`js/api.js`)

- All requests go to same-origin relative paths (`/api/...`) — Traefik
  serves UI and API from one origin, so there is no base-URL config and no
  CORS.
- `credentials: "same-origin"` so the session cookie is sent; the cookie
  is `HttpOnly`, so JS never reads or manipulates it.
- Central response handling: parse the error envelope from
  `04-backend-api-conventions.md`, throw a typed `ApiError` carrying
  `code`, `message`, and `fields`; on `401`, redirect to `index.html`.
- Never branch on anything resembling admin status — no such field is
  returned (`03-auth-and-multi-tenancy.md`).

## Session & storage resolution (`js/session.js`, `js/storage-switcher.js`)

- On page load, call `GET /api/auth/me` once.
- Zero storages → render the "ask an admin for access" empty state and
  stop. Do not imply that other storages exist.
- *Amended by [`29-first-run-admin-guidance.md`](29-first-run-admin-guidance.md):*
  the zero-storage case now navigates to `GET /no-storages` first and renders
  that empty state only on the way back, when the server has answered
  `?empty=1`. Nothing above is weakened — the copy and layout are unchanged,
  and an admin is sent to the admin view by the **server**, so the page still
  never learns `is_admin` and still renders no navigation to the admin area.
- Exactly one storage → use it, and hide the switcher entirely.
- More than one → render the header switcher listing the user's storages;
  selecting one navigates to the same page with the new `?storage=` value,
  so deep links stay meaningful. Persist the last selection in
  `localStorage` and use it to resolve a page opened without `?storage=`.
- *Amended by [`34-navigation-and-start-page.md`](34-navigation-and-start-page.md):*
  once a storage is resolved, `storages.html` forwards (`location.replace`)
  to that member's chosen start page, the dashboard by default, instead of
  rendering a landing card. Every storage-scoped page renders the shared
  navigation bar from `js/nav.js`. The picker, the header switcher's
  same-page rule and the zero-storage path above are unchanged.

## Shared job polling (`js/jobs.js`)

One helper used by every photo-driven feature (`06`, `07`, `09`) — not
three implementations:

```js
// pollJob(storageId, jobId, { onUpdate }) -> Promise<payload>
// polls GET /api/storages/{id}/jobs/{jobId} every 1.5s until done|failed
```

Callers render a loading state from `onUpdate`, the proposal on resolve,
and a retry affordance on `failed`.

## Shared review component (`js/review.js`)

`06`, `07`, and `09` all follow the same shape: upload → job → AI proposal
→ editable list → explicit confirm → only then does the server write
inventory. Implement that list once:

- Rows are cloned from a `<template>` in the page's HTML, populated via
  small DOM helpers — no string-concatenated HTML for user- or AI-supplied
  values (avoid injection; use `textContent`).
- Each row exposes editable quantity, product, and location/expiry fields
  as appropriate, and the **three row actions** defined in
  `09-consumption-logging.md`: accept (with edits), correct manually, and
  reject. All three behave identically across the features that use this
  component — a user who learns the review screen once has learned all of
  them.
- Manual correction may set a product image from the item's own crop or
  the uploaded photo itself, without any further AI call. Where the
  deployment offers it, that picture's background can then be removed, shown
  beside the original (`09-consumption-logging.md`).
- Rejected rows remain rendered, struck through, and reversible; they are
  omitted from the confirm payload rather than removed from the DOM.
- A single "Confirm" action posts the edited set to the feature's confirm
  endpoint, and a "Discard" action drops the whole job. Nothing is written
  before one of them. "Analyze again" replaces the whole proposal with a new
  analysis of the same photo, after asking, and writes nothing to inventory
  either (`09-consumption-logging.md`).

## Shared tree view (`js/tree.js`)

Used for both `locations` and `categories` (identical shape per
`02-data-model.md`): expand/collapse, inline add-child, rename, a
"move to…" picker, and required drag-and-drop re-parenting via native
HTML5 drag events (`06-vision-shelf-ingestion.md`). A page may pass an
optional `renderDetail` callback to draw something beside each node's name
— the category tree uses it for each node's shelf-life rule
(`08-expiration-and-classification.md`); the location tree does not.

## Styling

Hand-written CSS with custom properties as the single source of design
tokens in `css/tokens.css` — spacing scale, font stack, surface/text
colors, and the expiration urgency colors defined in
`08-expiration-and-classification.md`:

```css
:root {
  --urgency-expired:  #b91c1c;
  --urgency-critical: #ea580c;
  --urgency-soon:     #ca8a04;
  --urgency-ok:       #15803d;
  --urgency-none:     #6b7280;
}
```

No literal color values in component CSS or inline styles; components
reference tokens so urgency looks identical everywhere it appears. Layout
uses CSS grid/flexbox; the app must be usable one-handed on a phone, since
the primary flows start with a camera capture.

Pages use `.shell` (a `40rem` column). A page whose content is a wide table
may use the `.shell--wide` modifier (`80rem`) on both its header and its
main element. The first such page is the inventory table,
[`33-inventory-overview-table.md`](33-inventory-overview-table.md).

## PWA

- `manifest.json` — name, icons (192px and 512px), `display: "standalone"`,
  theme color, `start_url: "/storages.html"`. *(Amended by
  [`34-navigation-and-start-page.md`](34-navigation-and-start-page.md); was
  `/index.html`, which showed a signed-in user the login form on every
  launch. A signed-out user still reaches the login form, through the `401`
  redirect in `api.js`.)*
- `sw.js` — hand-written service worker that caches the app shell (HTML,
  CSS, JS, icons) with a cache-first strategy and a versioned cache name
  bumped on release. It must **not** cache `/api/*` responses: there is no
  offline data editing (`00-overview.md` non-goals), and stale inventory
  data would be actively misleading.
- **The cache-first path is an allowlist, never a denylist.** The `fetch`
  handler decides what to cache from a known set of static shapes: every page
  under `web/static/` by exact path (not only the subset precached at
  install — `sw.js`'s own `CACHEABLE_EXACT` also covers pages the shell
  lazily caches on first visit, such as `dashboard.html` and
  `settings.html`), plus the `/css/`, `/js/`, `/icons/` and `/vendor/`
  prefixes, and `/manifest.json` — not "everything same-origin except a
  list of excluded paths." A denylist needs a human to remember to add every
  new server-rendered route before it ships, and forgetting is silent: the
  route works the first time, then serves a stale response from Cache
  Storage on every request after, indistinguishable from a working app until
  someone notices (`/admin` shipped without an exclusion once, for exactly
  this reason — PR #65). An allowlist has the opposite failure mode: a route
  nobody has thought about yet is safe by default, and caching it is the
  thing someone opts into deliberately.
- The server answers `GET /sw.js` with `Cache-Control: no-cache` — not the
  default `http.FileServer` behaviour of no explicit header — so the worker
  script's own update check never depends on a browser's or an intermediate
  proxy's caching heuristics doing the right thing. It is the one file where
  "ask the server every time, and only reuse the bytes if it says they still
  match" must be guaranteed rather than assumed, because it is the only
  mechanism that can ever tell a client a new version exists.
- A "reset local app data" action must exist somewhere a user can reach it
  (the settings page) and must unregister every service worker registration
  and delete every Cache Storage entry for the origin. It is the manual
  escape hatch for whatever the version-bump cleanup above does not
  anticipate — a real in-app recovery path rather than an instruction to open
  DevTools.
- Photo capture offers **two explicit controls**, one for the photo
  library and one for the camera — see
  [`36-photo-source-picker.md`](36-photo-source-picker.md). *(Amended: this
  bullet used to prescribe a single
  `<input type="file" accept="image/*" capture="environment">` on the
  claim that it "still allows a gallery pick". It does not: on iOS Safari
  `capture` forces the camera and drops `multiple`, and Android Chrome
  forces the camera too (whether it also drops `multiple` is for `36`'s
  package to confirm on hardware) — so a photo already on the phone could
  not be uploaded at all.)* An
  in-page viewfinder that avoids leaving the page for the OS camera is
  [`37-in-page-camera.md`](37-in-page-camera.md); it falls back to `36`.

## Testing

The frontend itself has no test toolchain and no dependency step — the
unit-testable logic lives in Go and is covered there
(`04-backend-api-conventions.md`). Browser behavior is covered by
**end-to-end tests, which are a required deployment gate**.

### The rule

| Context | E2E required? |
|---|---|
| Editing files in the dev loop (`docker-compose.override.yml`) | **No.** Never runs automatically; edit-and-refresh stays instant. |
| `docker compose build` / the image build | **No.** E2E needs a live stack with a database, which a build stage does not have. |
| **Deploying** (promoting an image to the NAS / cutting a release) | **Yes — must pass first.** A failing E2E run blocks the deployment. |

Running them is explicit and on-demand in dev (`docker compose -f
docker-compose.e2e.yml run --rm e2e`), so the suite can be used while
developing without being imposed on every save.

### How they run

- A separate `docker-compose.e2e.yml` brings up the full stack (`app`,
  `db`, `traefik`) against a **disposable database**, seeds a known
  fixture (two admins, three ordinary users, and three storages — one
  shared by two ordinary users with a small inventory, one held by only one
  of them, for journey 2's switcher and journey 8's non-disclosure check,
  and one held by the second admin alone; the bootstrap admin and one
  ordinary user deliberately belong to **no** storage, which is the
  starting position `29-first-run-admin-guidance.md` is about), and runs
  the browser suite against it. A journey that writes state other journeys
  would observe gets its **own** seeded user and storage, because the
  suite runs files in parallel. Specs `32`–`35` each add such a dedicated
  fixture.
- The runner is a **pre-built browser-automation image pulled from a
  registry** (e.g. the official Playwright image), used as a throwaway
  test container. This does not violate the no-toolchain rule in
  `01-architecture-and-deployment.md`: nothing is installed on the host,
  and the application image itself still contains no Node.js — the
  browser runtime exists only inside the test container, and only while
  tests run.
- Specs live in `e2e/` as plain JavaScript. They exercise real user
  journeys through the real UI, not implementation details.
- External services are stubbed: the Gemini, SerpAPI, and Iconify calls
  point at a local fake so runs are deterministic, free, and work offline.

### Required coverage before a deployment is allowed

At minimum, these journeys must pass:

1. Log in, land in a storage; log out.
2. A user with two storages switches between them and sees each storage's
   own data.
3. Create a location and a category; add a product; see it in the list.
4. Upload a shelf photo (stubbed vision), edit the proposal in the review
   UI, confirm, and verify the resulting inventory.
5. Log a consumption with an overridden count and verify the decrement.
6. Reconcile a shopping list across all three match states.
7. A non-admin gets `404` for `/admin` and for `/api/admin/users` — the
   admin area is not reachable or discoverable
   (`03-auth-and-multi-tenancy.md`).
8. A member of storage A gets `404` for a storage B id, with no
   distinction from a nonexistent id.
9. A freshly seeded deployment's bootstrap admin — an admin belonging to no
   storage — ends on the admin view after logging in, without typing a URL
   (`29-first-run-admin-guidance.md`).
10. A non-admin with no storage ends on the empty state and stays on it, and
    an admin who belongs to a storage lands in it and is not redirected —
    the boundary of journey 9 in both directions.
11. The service worker never answers `GET /no-storages` from cache: the
    route's redirect is computed per request, and a remembered one would
    outlive the membership or admin rights it was computed from.
