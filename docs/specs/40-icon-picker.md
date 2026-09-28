# 40 — Icon Picker with Alias Search

Depends on: [`02-data-model.md`](02-data-model.md),
[`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(`Iconify.Candidates`, the existing provider integration this spec reuses),
[`16-product-maintenance.md`](16-product-maintenance.md) (`icon_name` — the
field this spec turns from free text into a picker).

## Why this spec exists

`icon_name` (`16-product-maintenance.md`) is a plain text input today: a
person has to already know and correctly type an exact Iconify identifier
like `noto:cheese-wedge`. The system already talks to Iconify's search API
for the unrelated *picture* suggestion flow (`07`, `Iconify.Candidates`),
but that flow only ever produces a rasterized, cached image — never an
`icon_name` a person could pick. There is no visual, searchable way to set
`icon_name` at all, and no way for a search term that doesn't match
Iconify's own naming to find an icon a household actually wants (e.g.
searching "beer" should find a beer icon even if Iconify itself only
indexes it under a brand-specific name).

## Schema

```sql
CREATE TABLE icon_aliases (
    id         UUID PRIMARY KEY,
    icon_name  VARCHAR(100) NOT NULL,
    alias      VARCHAR(100) NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_icon_aliases_icon_alias
    ON icon_aliases(icon_name, alias);
CREATE INDEX idx_icon_aliases_alias_trgm
    ON icon_aliases USING gin (alias gin_trgm_ops);
```

- **Global, not per-storage** — the same deliberate exception
  `catalog_products` already is (`02-data-model.md`): an icon alias
  describes an Iconify identifier, not household data, so it is shared
  across every storage on the instance. No `storage_id` column, matching
  `catalog_products`'s own reasoning exactly.
- **Insert-only, like `catalog_products`.** There is no edit or delete
  endpoint in this spec. An alias that turns out to be wrong is harmless
  clutter, not a correctness bug — search ranks exact local hits first
  (below), so a bad alias only ever adds a low-relevance extra result, and
  removing bad aliases (if it's ever worth building) is a later, separate
  concern.
- One `icon_name` can have many aliases (a beer icon might be found by
  "beer", "bottle", "drink"), and one alias string can point at more than
  one `icon_name` (searching "milk" reasonably returns both a carton and a
  bottle) — no uniqueness constraint beyond the pair itself.
- The `gin_trgm_ops` index reuses the same trigram extension
  `02-data-model.md` already enables for matching, so alias search can be
  fuzzy (`ILIKE`/similarity), not exact-substring-only.

## Search endpoint

`GET /api/storages/{storage_id}/icon-suggestions?query=…` — storage-scoped
route for consistency with every other endpoint under
`/api/storages/{storage_id}/…` and because the existing auth/session
middleware is storage-scoped, even though `icon_aliases` and Iconify
results underneath are global data, exactly as `image-suggestions` (`07`)
already is.

1. Query `icon_aliases` for `alias ILIKE`/trigram-similar to `query`,
   ordered by similarity, returning `{icon_name, alias}` pairs (the
   matched alias is shown to the person as *why* this icon matched — "beer
   → 🍺 mdi:beer" reads better than an opaque list of slugs).
2. Query Iconify's `/search` endpoint the same way `Iconify.Candidates`
   already does (`07`, `internal/imagesearch/providers.go`), reusing that
   client rather than a second HTTP integration.
3. Merge: local alias hits first (deduplicated by `icon_name`), then
   Iconify hits not already present from step 1, capped at the same
   `limit=10` `Iconify.Candidates` already uses for its own Iconify call —
   this spec does not introduce a new pagination or limit convention.
4. Each result renders as the actual icon (an inline SVG or the same
   rendered-icon URL scheme `07`'s picture picker already uses for
   Iconify previews), not just the identifier text — a person recognizes
   a beer icon on sight far faster than they recognize its slug.
5. If Iconify is unreachable, local alias results still return (never
   fail the whole request over the external half being down) — same
   fail-soft posture `07` already specifies for the picture flow ("If the
   provider is unreachable, the product is created without a picture
   rather than the confirm failing").

## Recording a new alias

`POST /api/storages/{storage_id}/icon-suggestions/aliases` — body
`{icon_name, alias}`. Both fields are required, non-empty, and capped at
100 characters (`VARCHAR(100)`, matching the schema above) — `422` on a
missing/empty field or on either exceeding the limit, the same shape
every other length-bounded field in this system already rejects
(`16-product-maintenance.md`'s `icon_name` cap is the closest precedent).
On a valid body, inserts with `ON CONFLICT (icon_name, alias) DO
NOTHING`, the same insert-and-ignore shape `catalog_products` already
uses. Any authenticated storage member may call this — it is additive,
low-risk shared data, not an admin action (`03-auth-and-multi-tenancy.md`'s
admin concept stays about user/storage administration, not content
curation).

The picker (below) calls this automatically, from the search term the
person actually typed, whenever they pick a result that did not already
come from a local alias match (i.e. an Iconify-only hit): the search term
becomes a new alias for the icon they picked, so the *next* person
searching the same word gets a local hit. No confirmation step, no
checkbox to opt out — teaching the alias table is the point of using the
picker at all, and a wrong alias costs nothing per the insert-only
reasoning above.

## The picker UI

New `web/static/js/icon-picker.js`, sibling to `js/image-picker.js`
(`07`, `16`) and following its established shape: a search input, a
result grid of clickable icon buttons (not a dropdown — recognizing an
icon visually is the entire point), loading/empty/provider-down states.
Replaces the free-text `icon_name` `<input>` on `products.html`'s edit
form (`16-product-maintenance.md`, "The product edit surface"):

- Opens showing the product's current icon (if any) highlighted, with the
  search box focused and empty — a person changing an existing icon does
  not have to remember what they typed to find it originally.
- Picking a result calls the existing `PATCH
  /api/storages/{storage_id}/products/{id}` with `icon_name` (`16`)
  exactly as today, plus fires the alias-recording call above when
  applicable.
- "Clear icon" stays a one-click action distinct from search (sets
  `icon_name: null`), same as today's field already allows.

## Acceptance criteria

- `icon_name` is set only by picking a rendered icon from search results
  — the raw text input is removed from `products.html`'s edit form.
- Searching a term with an existing local alias returns that icon first,
  ahead of any Iconify-only results, even if Iconify's own ranking would
  place something else first.
- Picking an Iconify-only result (no local alias existed for the search
  term) records a new `icon_aliases` row for exactly the term the person
  typed, silently, in the same interaction — verify by searching the same
  term again and confirming the icon now appears as a local hit.
- Iconify being unreachable degrades to local-alias-only results, never a
  failed picker.
- `POST .../icon-suggestions/aliases` is `422` on a missing/empty
  `icon_name` or `alias`, and `422` on either exceeding 100 characters —
  never a silent truncation or a database-level error surfacing to the
  client.
- `icon_aliases` is global: an alias recorded from one storage's picker
  session is immediately findable from a different storage's picker — the
  same non-enumeration rules that protect storage data
  (`03-auth-and-multi-tenancy.md`) do not apply here, because this table
  carries no household data, exactly as `catalog_products` already
  doesn't.
- E2E: `e2e/specs/products.spec.js` covers searching, picking an
  Iconify-only result, confirming the alias now exists (a second search
  for the same term returns a local hit), and clearing an icon.
