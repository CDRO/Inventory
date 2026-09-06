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
  as appropriate, plus a per-row include/exclude toggle.
- A single "Confirm" action posts the edited set to the feature's confirm
  endpoint. Nothing is written before that.

## Shared tree view (`js/tree.js`)

Used for both `locations` and `categories` (identical shape per
`02-data-model.md`): expand/collapse, inline add-child, rename, and a
"move to…" picker. Drag-and-drop is optional and not required.

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

There is no frontend test toolchain, by design — the frontend has no build
or dependency step to hang one on. Correctness of the browser layer is
covered by keeping logic thin (the interesting rules live in Go and are
tested there per `04-backend-api-conventions.md`) and, optionally, by
end-to-end browser tests run from a throwaway container. Such tests are
never a prerequisite for building or deploying.
