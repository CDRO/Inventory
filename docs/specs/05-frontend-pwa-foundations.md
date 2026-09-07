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
├── ingest.html                # shelf photo ingestion (06)
├── shopping-list.html         # list reconciliation (07)
├── consume.html               # consumption logging (09)
├── dashboard.html             # reorder + analytics (10, 11)
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
│   ├── tree.js                # shared tree view (locations + categories)
│   ├── dom.js                 # small helpers: el(), render templates, escape
│   └── pages/                 # one module per HTML page, imported by that page
├── vendor/                    # single-file vendored libraries (see 11)
├── icons/                     # PWA icons (192, 512)
├── manifest.json
└── sw.js                      # hand-written service worker
```

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
- Exactly one storage → use it, and hide the switcher entirely.
- More than one → render the header switcher listing the user's storages;
  selecting one navigates to the same page with the new `?storage=` value,
  so deep links stay meaningful. Persist the last selection in
  `localStorage` and use it to resolve a page opened without `?storage=`.

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
  the uploaded photo itself, without any further AI call.
- Rejected rows remain rendered, struck through, and reversible; they are
  omitted from the confirm payload rather than removed from the DOM.
- A single "Confirm" action posts the edited set to the feature's confirm
  endpoint, and a "Discard" action drops the whole job. Nothing is written
  before one of them.

## Shared tree view (`js/tree.js`)

Used for both `locations` and `categories` (identical shape per
`02-data-model.md`): expand/collapse, inline add-child, rename, a
"move to…" picker, and required drag-and-drop re-parenting via native
HTML5 drag events (`06-vision-shelf-ingestion.md`).

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

## PWA

- `manifest.json` — name, icons (192px and 512px), `display: "standalone"`,
  theme color, `start_url: "/index.html"`.
- `sw.js` — hand-written service worker that caches the app shell (HTML,
  CSS, JS, icons) with a cache-first strategy and a versioned cache name
  bumped on release. It must **not** cache `/api/*` responses: there is no
  offline data editing (`00-overview.md` non-goals), and stale inventory
  data would be actively misleading.
- Camera capture uses
  `<input type="file" accept="image/*" capture="environment">` so mobile
  browsers open the rear camera directly while still allowing a gallery
  pick.

## Testing

The frontend itself has no test toolchain and no dependency step — the
unit-testable logic lives in Go and is covered there
(`04-backend-api-conventions.md`). Browser behavior is covered by
**end-to-end tests, which are a required deployment gate**.

### The rule

| Context | E2E required? |
|---|---|
| Editing files in the dev loop (`docker-compose.dev.yml`) | **No.** Never runs automatically; edit-and-refresh stays instant. |
| `docker compose build` / the image build | **No.** E2E needs a live stack with a database, which a build stage does not have. |
| **Deploying** (promoting an image to the NAS / cutting a release) | **Yes — must pass first.** A failing E2E run blocks the deployment. |

Running them is explicit and on-demand in dev (`docker compose -f
docker-compose.e2e.yml run --rm e2e`), so the suite can be used while
developing without being imposed on every save.

### How they run

- A separate `docker-compose.e2e.yml` brings up the full stack (`app`,
  `db`, `traefik`) against a **disposable database**, seeds a known
  fixture (an admin, two users, one storage with a small inventory), and
  runs the browser suite against it.
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
