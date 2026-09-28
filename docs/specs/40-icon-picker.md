# 40 — Icon Picker with Alias Search

Depends on: [`02-data-model.md`](02-data-model.md),
[`42-local-icon-library.md`](42-local-icon-library.md) (the `icons` table
this spec searches — **no external network call**, see that spec for why
and where the icon data actually comes from),
[`16-product-maintenance.md`](16-product-maintenance.md) (`icon_name` —
the field this spec turns from free text into a picker).

## Why this spec exists

`icon_name` (`16-product-maintenance.md`) is a plain text input today: a
person has to already know and correctly type an exact identifier like
`noto:cheese-wedge`. There is no visual, searchable way to set
`icon_name` at all, and no way for a search term that doesn't match an
icon's own name to find it (e.g. searching "beer" should find a beer icon
even if its own name is something else).

*(Revised — an earlier draft of this spec searched Iconify's live
`/search` API at request time. Tizian: "I do not want the inventory to
make an external call to iconify to search for icons... I already have
two dependencies, I do not want or need a third one." Every icon this
picker can return now comes from the local `icons` table
(`42-local-icon-library.md`) — no request this spec makes ever leaves the
container network. The `icon_aliases` table below still exists — it was
never the network dependency, only the *search* step was, and the
identifier namespace stays whatever `42` vendors, so an `icon_name`
already saved under the old design stays valid with no backfill.)*

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
  `catalog_products` already is (`02-data-model.md`), and the same one
  `42`'s own `icons` table is: an alias describes a locally-stored icon,
  not household data, so it is shared across every storage on the
  instance.
- **Insert-only, like `catalog_products`.** There is no edit or delete
  endpoint in this spec. An alias that turns out to be wrong is harmless
  clutter, not a correctness bug — search ranks exact local alias hits
  first (below), so a bad alias only ever adds a low-relevance extra
  result.
- One `icon_name` can have many aliases (a beer icon might be found by
  "beer", "bottle", "drink"), and one alias string can point at more than
  one `icon_name` (searching "milk" reasonably returns both a carton and
  a bottle) — no uniqueness constraint beyond the pair itself.
- `icon_name` here is **not** a foreign key to `42`'s `icons.name` —
  deliberately, so an alias recorded against an icon that a later
  `42`-driven re-import happens to rename or drop degrades to "one fewer
  search hit," never a broken reference, a migration hazard, or a reason
  to block re-vendoring.
- The `gin_trgm_ops` index reuses the same trigram extension
  `02-data-model.md` already enables for matching, so alias search can be
  fuzzy (`ILIKE`/similarity), not exact-substring-only.

## Search endpoint

`GET /api/storages/{storage_id}/icon-suggestions?query=…` — storage-scoped
route for consistency with every other endpoint under
`/api/storages/{storage_id}/…` and because the existing auth/session
middleware is storage-scoped, even though `icon_aliases` and `icons`
underneath are both global data, exactly as `image-suggestions` (`07`)
already is for its own global `cached_images`.

1. Query `icon_aliases` for `alias ILIKE`/trigram-similar to `query`,
   ordered by similarity, returning `{icon_name, alias}` pairs (the
   matched alias is shown to the person as *why* this icon matched — "beer
   → 🍺 matched via 'beer'" reads better than an opaque list of names).
2. Query `42`'s `icons` table directly — `name ILIKE`/trigram-similar to
   `query` — **no network call, no external process**, a single indexed
   `SELECT` against data that has lived in this database since the
   library was vendored.
3. Merge: local alias hits first (deduplicated by `icon_name`), then
   direct `icons` name hits not already present from step 1, capped at a
   reasonable limit (`20` — this spec does not inherit any external
   provider's own pagination convention, since it no longer has one).
4. Each result renders the actual icon — `icons.svg_body` served inline
   or via a small per-icon endpoint (`42`) — never just the identifier
   text; a person recognizes a beer icon on sight far faster than they
   recognize its name.
5. **There is no "provider unreachable" case anymore.** Every result
   this endpoint can return already lives in this database; the only
   failure modes are this database being unreachable (already true of
   every endpoint in this system) and a query returning zero rows (an
   empty, not a failed, result).

## Recording a new alias

`POST /api/storages/{storage_id}/icon-suggestions/aliases` — body
`{icon_name, alias}`. Both fields are required, non-empty, and capped at
100 characters (`VARCHAR(100)`, matching the schema above) — `422` on a
missing/empty field or on either exceeding the limit, the same shape
every other length-bounded field in this system already rejects
(`02-data-model.md`'s `VARCHAR(100)` columns are the existing precedent
for this exact limit). On a valid body, inserts with `ON CONFLICT
(icon_name, alias) DO NOTHING`, the same insert-and-ignore shape
`catalog_products` already uses. Any authenticated storage member may
call this — it is additive, low-risk shared data, not an admin action
(`03-auth-and-multi-tenancy.md`'s admin concept stays about user/storage
administration, not content curation).

The picker (below) calls this automatically, from the search term the
person actually typed, whenever they pick a result that did not already
come from a local alias match (i.e. a direct `icons.name` hit): the
search term becomes a new alias for the icon they picked, so the *next*
person searching the same word gets an alias hit ranked first. No
confirmation step, no checkbox to opt out — teaching the alias table is
the point of using the picker at all, and a wrong alias costs nothing per
the insert-only reasoning above.

## The picker UI

New `web/static/js/icon-picker.js`, sibling to `js/image-picker.js`
(`07`, `16`) and following its established shape: a search input, a
result grid of clickable icon buttons (not a dropdown — recognizing an
icon visually is the entire point), loading/empty states. There is no
"provider down" state to design for anymore — see "Search endpoint,"
above. Replaces the free-text `icon_name` `<input>` on `products.html`'s
edit form (`16-product-maintenance.md`, "The product edit surface"):

- Opens showing the product's current icon (if any) highlighted, with the
  search box focused and empty — a person changing an existing icon does
  not have to remember what they typed to find it originally.
- Picking a result calls the existing `PATCH
  /api/storages/{storage_id}/products/{id}` with `icon_name` (`16`)
  exactly as today, plus fires the alias-recording call above when
  applicable.
- "Clear icon" stays a one-click action distinct from search (sets
  `icon_name: null`), same as today's field already allows.
- No matches for a search term is a plain empty state pointing at `42`'s
  "Uploading a custom icon" path, not a dead end — the picker names it
  explicitly ("Nothing found — add a custom icon") rather than leaving
  the person to guess there's another way.

## Acceptance criteria

- `icon_name` is set only by picking a rendered icon from search results
  — the raw text input is removed from `products.html`'s edit form.
- **No request this spec's endpoints make ever reaches a host outside
  this deployment** — verify by running the picker with network egress
  blocked (or a proxy that fails every external request) and confirming
  search, pick, and alias-recording all still work.
- Searching a term with an existing local alias returns that icon first,
  ahead of any direct `icons.name` hits, even if a substring/trigram
  match on the name alone would rank something else first.
- Picking a direct `icons.name` hit (no local alias existed for the
  search term) records a new `icon_aliases` row for exactly the term the
  person typed, silently, in the same interaction — verify by searching
  the same term again and confirming the icon now appears as an alias
  hit.
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
- E2E: `e2e/specs/products.spec.js` covers searching, picking a direct
  name hit, confirming the alias now exists (a second search for the
  same term returns it as an alias hit), and clearing an icon — with no
  network stub needed for any of it, since nothing here calls out.
