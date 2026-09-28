# 42 — Local Icon Library

Depends on: [`02-data-model.md`](02-data-model.md),
[`01-architecture-and-deployment.md`](01-architecture-and-deployment.md)
(no host toolchain — everything below runs through Docker or the compiled
binary; the documented first-clone command sequence this spec's import
step fits into).

This spec is self-contained — it defines the `icons` table and how it's
populated, and depends on nothing that in turn depends on it.
[`40-icon-picker.md`](40-icon-picker.md) is this spec's only consumer
(its search endpoint reads the `icons` table this spec creates); `40`'s
own "Depends on" line points here, and this spec does not point back —
a one-directional reference, not a circular one, even though `40`'s file
number is lower and this spec was added second. A reader who follows
`00-overview.md`'s filename order reaches `40` first and, per `40`'s own
dependency line, is told to read this spec's data model before relying
on `40`'s search endpoint.

## Why this spec exists

`40`'s picker needs real icon data — SVGs plus searchable names — served
without a live call to any external API. Tizian, reviewing `40`: *"I do
not want the inventory to make an external call to iconify to search for
icons... I already have two dependencies, I do not want or need a third
one. Extend this to also integrate everything regarding external icons
and make sure icons are provided internally."* This spec is where that
data actually comes from: a vendored, offline icon set imported once at
build time, plus a documented path for adding more later. Nothing in this
spec makes a network call at request time, at build time, or at any other
time an operator did not explicitly choose to run an import.

## Schema

```sql
CREATE TABLE icons (
    id         UUID PRIMARY KEY,
    name       VARCHAR(100) NOT NULL,
    svg_body   TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'vendored'
               CHECK (source IN ('vendored', 'uploaded')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_icons_name ON icons(name);
CREATE INDEX idx_icons_name_trgm ON icons USING gin (name gin_trgm_ops);
```

- **Global, not per-storage** — the same deliberate exception
  `catalog_products` and `40`'s `icon_aliases` already are
  (`02-data-model.md`): an icon is not household data.
- `name` **is** the value `products.icon_name` stores — no separate id
  indirection, so `16-product-maintenance.md`'s `icon_name` contract
  ("set or clear") is completely unchanged by this spec existing.
  Namespaced (`noto:cheese-wedge`, `custom:whatever-a-person-typed`) so
  two sources can never collide on a bare word.
- `source` distinguishes a vendored icon (below) from one a person
  uploaded (further below) — purely informational, not used by `40`'s
  search, which treats both identically.
- **Licensing is recorded once, here, not per row.** The base vendored
  set (below) is a single collection under a single license; repeating
  that license string on 3,145 rows would be noise, not a safeguard. An
  **uploaded** icon carries no license claim at all — see "Uploading a
  custom icon."

## Vendoring the base set

The base library is Google's **Noto Emoji** set, licensed
**Apache License 2.0** (`https://github.com/googlefonts/noto-emoji`,
`LICENSE`), distributed by the Iconify project in a single machine-readable
file (the `@iconify-json/noto` package's `icons.json`, "IconifyJSON"
format: one JSON document, every icon's SVG path data plus its own name).
This is the same collection `16`/`40`'s earlier drafts already assumed
(`noto:cheese-wedge`) and, as an emoji set, already covers food, drink,
containers, and household objects well — the actual domain this picker is
for.

- **The JSON file is vendored into this repository** at
  `internal/iconlib/data/noto.json` (checked in, not fetched at build or
  run time — the one-time download that produces it is a maintainer
  action, documented in that directory's own `README.md`, not a step any
  Docker build or `docker compose` invocation performs). This is the
  same "single vendored file, never a CDN" rule
  `05-frontend-pwa-foundations.md` already holds frontend libraries to,
  applied to data instead of code.
- **Import is a Go subcommand, not a SQL migration.** `inventory icons
  import` (alongside the existing `inventory migrate` subcommand,
  `01-architecture-and-deployment.md`) reads the embedded JSON
  (`//go:embed`, the same mechanism `web/static` already uses), parses
  every icon into one `icons` row (`name = "noto:" + the icon's own
  key`, `svg_body` reconstructed from the collection's shared SVG
  attributes plus the icon's own path data per the IconifyJSON format),
  and inserts with `ON CONFLICT (name) DO NOTHING` — safe to run more
  than once. **Run as part of `inventory migrate up`, after the schema
  migration that creates `icons` has applied** — not during `docker
  compose run --rm setup`, which only writes `.env` and runs *before*
  `migrate up` in the documented sequence
  (`01-architecture-and-deployment.md`, the four-command first-clone
  sequence), so the table does not exist yet at that point. This is the
  same shape `migrate up` already has for the initial admin — "also
  creates the initial admin" is already one of `migrate up`'s
  side-effects beyond pure schema migration, and this is a second one —
  so a fresh install has a searchable library after the same command a
  fresh install already had to run, with no separate manual step and no
  change to the documented command sequence. All 3,145 icons import —
  this spec does not hand-curate a
  subset; a smaller, hand-picked set is real editorial work with no
  clear stopping point, while "import everything, let search and the
  alias table (`40`) surface what's actually useful" costs only inert
  storage for the icons nobody ever searches for (household scale,
  `00-overview.md` — a few thousand extra text rows is nothing to
  optimize away).

## Adding more icons later

Two paths, both offline, neither turning into a running dependency:

- **Vendor another collection.** Any Iconify-format JSON file under a
  permissive license (MIT, Apache-2.0, CC0 — check before adding) can be
  dropped beside `noto.json` and imported the same way, namespaced by its
  own prefix. This spec does not commit to a second collection now —
  Noto's breadth is the reason it was chosen first — but the mechanism
  does not need to change to add one.
- **Upload a custom icon**, below, for the individual "nothing in 3,145
  emoji fits this specific product" case, which does not need a whole
  new collection to solve.

## Uploading a custom icon

`POST /api/storages/{storage_id}/icons` — multipart SVG upload (or a
pasted SVG string) plus a `name`. For the rare case the vendored set has
nothing close enough.

- The uploaded content must parse as well-formed SVG — `422` on anything
  else, the same validation-failure code every other malformed-input
  case in this system already uses (this is a format check, not a redraw
  or a simplification pass); stored as-is, `source = 'uploaded'`.
- `name` is freely chosen by the uploader (validated non-empty, ≤ 100
  chars, unique — `422` on a collision, including with a vendored name,
  so nobody can accidentally shadow `noto:cheese-wedge`) and immediately
  searchable through `40`'s picker exactly like a vendored icon.
- **Security: an uploaded SVG is untrusted content from a person using
  the system**, not a paid provider's response — the distinction this
  project's own trusted-provider posture already draws. It is never
  injected as inline DOM markup (which could execute embedded script in
  some renderers); it is always served through an `<img>` element (a data
  URI or a dedicated `GET /api/storages/{storage_id}/icons/{id}/svg`
  response with `Content-Type: image/svg+xml` and no inline-script
  execution context), the same rendering discipline that already applies
  to every other user-suppliable image in this system.
- Any authenticated storage member may upload — the same low-risk,
  additive-shared-data reasoning `40`'s alias endpoint already uses, not
  an admin action.

## What this deliberately does not do

- **No AI-generated icon art.** Gemini is already a dependency
  (`01-architecture-and-deployment.md`), so using it to *generate* a
  novel icon when nothing fits was considered — and set aside. Icon-style
  SVG generation is a much less reliable task for a vision/text model
  than the analysis work Gemini already does in this system, the failure
  mode (a broken or ugly SVG silently saved as a product's icon) is
  worse than "no icon," and "upload your own" already gives a complete,
  low-risk answer to the same need. Revisit only if the upload path
  turns out not to be enough in practice.
- **No live external search, ever, for this feature.** Not Iconify, not
  any other icon API — that is the entire premise this spec exists
  under.

## Acceptance criteria

- A fresh install following `01-architecture-and-deployment.md`'s
  documented four-command sequence (`setup`, `build`, `migrate up`,
  `up -d`) ends with a populated, searchable `icons` table after
  `migrate up` — the same command that already creates the initial
  admin — with no separate manual import step and no network access at
  any point in that sequence.
- `inventory icons import` run a second time changes nothing (`ON
  CONFLICT (name) DO NOTHING`) and does not error.
- Every `icons.name` value the base import produces is prefixed `noto:`
  and matches the vendored JSON's own icon keys exactly — spot-check a
  handful against `internal/iconlib/data/noto.json` directly.
- `POST /api/storages/{storage_id}/icons` rejects malformed SVG, rejects
  a name collision (vendored or uploaded) with `422`, and a successfully
  uploaded icon is immediately findable through `40`'s search endpoint.
- An uploaded SVG is never rendered as inline DOM markup anywhere in the
  frontend — grep the icon-rendering code path for confirmation as part
  of this package's own review, not just at picker-UI review time.
- Neither this spec's icon-import path nor `40`'s search/picker endpoints
  ever reach `api.iconify.design` or any other icon-serving host — the
  E2E network-blocked check `40` already specifies is the regression
  guard. **This does not extend to `07-shopping-list-reconciliation.md`'s
  picture-suggestion flow**, which keeps its own, different, already-
  shipped call to the same host (see `07`'s own flagged note) — a
  system-wide "never calls Iconify" claim would be false and is not what
  this spec asserts.
