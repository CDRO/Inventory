# 07 — Shopping List Reconciliation

Implements PRD Feature 3. Depends on: [`02-data-model.md`](02-data-model.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md).

## Goal

User provides a shopping list (typed text or a photo of a handwritten/
printed list). Each line item is matched against existing `products` and
classified into one of three states, each with a distinct resolution UI.

## Additional schema

Primary keys are UUIDv7, per `02-data-model.md`.

```sql
CREATE TABLE shopping_lists (
    id          UUID PRIMARY KEY,
    storage_id  UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    source      TEXT NOT NULL CHECK (source IN ('text', 'photo')),
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shopping_list_items (
    id                 UUID PRIMARY KEY,
    shopping_list_id   UUID NOT NULL REFERENCES shopping_lists(id) ON DELETE CASCADE,
    raw_text           VARCHAR(255) NOT NULL,
    status             TEXT NOT NULL
                       CHECK (status IN ('exact_match', 'new_item', 'ambiguous', 'resolved')),
    matched_product_id UUID REFERENCES products(id) ON DELETE SET NULL,
    resolved_quantity  INT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## Ingestion

- `POST /api/storages/{storage_id}/shopping-lists` — body either
  `{source: "text", raw_text: "milk\neggs\n..."}` (newline-delimited) or a
  multipart photo upload with `source: "photo"`. Photo variant follows the
  same background-job pattern as `06-vision-shelf-ingestion.md`: Gemini
  OCRs/extracts line items into a plain string array, then processing
  continues identically to the text path. Returns `202 {job_id}` for
  photo, or `201` with the created list directly for text (no AI call
  needed to just split lines).
- Each raw line becomes one `shopping_list_items` row via the **matching
  service** (below), which assigns `status` and, for `exact_match`,
  `matched_product_id`.

## Matching service (shared)

One backend package, `internal/matching`, exposing
`MatchProductCandidates(ctx, storageID, text) (MatchResult, error)`. Used
here **and** by `06-vision-shelf-ingestion.md` and
`09-consumption-logging.md` — do not duplicate matching logic across
features.

It runs in three ordered stages, stopping as early as possible, so that
the expensive external calls happen only when nothing already known
applies:

**Stage 1 — local products.** Trigram similarity (`pg_trgm`,
`similarity()` / `%`) between `text` and `products.name` within this
storage, backed by `idx_products_name_trgm` (`02-data-model.md`). No
external service, no embeddings.

- similarity ≥ 0.6, single clear best match → `exact_match`
- best match in [0.35, 0.6), or several close candidates → `ambiguous`
- best match < 0.35, or the storage has no products yet → continue to
  stage 2

**Stage 2 — anonymous catalog.** Trigram lookup against
`catalog_products.normalized_name` (`02-data-model.md`). A hit yields
`new_item` **with pre-filled data** — display name, category path, item
type, image/icon, default shelf life — so the user can accept a known
product in one click with **no Gemini and no SerpAPI call at all**. The
response exposes only those display fields; it must never reveal that the
data came from another storage, or that other storages exist
(`03-auth-and-multi-tenancy.md`).

**Stage 2b — variant siblings.** Alongside a catalog hit, return the
variants linked to it via `catalog_products.base_id`: the hit's siblings
if the hit is itself a variant, or its children if the hit is a base.
This is what makes an abbreviated list line usable — `"thomatoes, c."`
trigram-matches the base `tomatoes`, and the variant set then offers
*cherry tomatoes* / *yellow tomatoes* as one-click choices, which no
amount of string matching on `", c."` could have produced. Cap the
returned variants (e.g. 5, by trigram similarity to the raw line) so the
UI stays a short list rather than a catalog browser.

**Stage 3 — external.** Only if stages 1 and 2 both miss: `new_item` with
no pre-filled data, which triggers the image-suggestion flow below (and,
for photo-sourced input, the Gemini call that produced the text).

Thresholds are a starting default — tune against real behavior, but keep
them as named constants in `internal/matching`, not scattered magic
numbers.

## Resolution UI per state

- **Exact Match:** show the matched product and an auto-filled "+1 unit"
  (or parsed quantity if present in `raw_text`, e.g. "eggs x2") inventory
  proposal. User reviews/edits quantity and location, then confirms —
  same confirm-writes-batches-and-logs pattern as `06`.
- **New Item, catalog hit (stage 2/2b):** show the known product card
  (name, category path, item type, image), any variant siblings as
  alternative cards, an "Add this" action, and an "It's something else"
  escape hatch. Accepting copies the fields into a new storage-local
  `products` row, resolving `category_path` against this storage's
  `categories` tree and creating any missing nodes, then proceeds like an
  exact match for the initial quantity. No external API is called on this
  path. The image is fetched once and stored locally rather than
  hot-linked (`02-data-model.md`).
- **New Item, no catalog hit (stage 3):** trigger the **image suggestion**
  flow (below); once the user picks/uploads an image (or explicitly
  skips), let them fill in `name`, `category_id`, `item_type`,
  `min_stock`, then create the `products` row and **insert** it into
  `catalog_products` (`INSERT ... ON CONFLICT DO NOTHING` — never an
  update, see `02-data-model.md`).
- **Rejecting a suggested name creates a variant link:** if the user was
  shown a catalog card, declined it, and created a differently-named
  product in the same interaction, insert the new catalog row with
  `base_id` pointing at the row they were shown (normalized to a base per
  the one-level rule in `02-data-model.md`). This is the only way the
  variant graph is built — no curation, no AI call.
- **Ambiguous:** present the top candidate products (from the matching
  service, e.g. top 3 by similarity) for manual selection, plus an escape
  hatch to "treat as new item" (routes into the New Item flow) or "enter
  manually" (skip matching entirely, same as New Item without a
  suggested-name prefill).

Each `shopping_list_items` row moves to `status = 'resolved'` once the
user has confirmed an action for it; `resolved_quantity` records what was
actually applied.

## Image suggestion flow (New Item)

`GET /api/storages/{storage_id}/image-suggestions?query={text}` returns
exactly 3 suggestions:

```json
{
  "suggestions": [
    { "type": "icon",  "url": "/api/storages/018f.../images/9c1f…", "source": "iconify" },
    { "type": "photo", "url": "/api/storages/018f.../images/4ab7…", "source": "serpapi" },
    { "type": "photo", "url": "/api/storages/018f.../images/e02d…", "source": "serpapi" }
  ]
}
```

Every `url` is on our own origin and serves bytes already fetched into the
cache below — the browser never contacts Iconify, SerpAPI, or Google.

- **1 icon/vector** via the **Iconify API** (`https://api.iconify.design`,
  no API key required): query a relevant icon set (e.g. search
  `https://api.iconify.design/search?query={text}` and return the first
  reasonable hit's rendered SVG/PNG URL). If no relevant icon is found,
  fall back to a generic "box"/"package" icon rather than omitting the
  suggestion.
- **2 real product photos** via **SerpAPI**'s Google Images engine
  (`https://serpapi.com/search?engine=google_images&q={text}&api_key=...`,
  `SERPAPI_API_KEY` from `01-architecture-and-deployment.md`), taking the
  top 2 image results.
- **The browser never talks to SerpAPI, Google, or Iconify.** The endpoint
  calls both providers server-side (never exposing the SerpAPI key),
  **downloads each candidate image**, stores it in the suggestion cache,
  and returns URLs on our own origin
  (`/api/storages/{storage_id}/images/{hash}`) that the frontend `<img>`
  tags reference. Hot-linking a third-party URL would leak every viewer's
  IP and user-agent to that host, break on a LAN-only NAS, and let the
  remote server change the picture after the fact.
- The user can: pick one of the 3, upload a custom photo instead
  (`POST /api/storages/{storage_id}/products/{id}/image` multipart), or
  re-trigger the search with an edited query (calls the same endpoint
  again with different `query` text — useful when the auto-derived query
  from `raw_text` was poor).
- If SerpAPI or Iconify is unreachable/rate-limited, degrade gracefully:
  return fewer than 3 suggestions rather than failing the whole New Item
  flow; the user can still proceed with a manually uploaded photo or no
  image at all.

### Image cache

Two tiers, because a browsed suggestion and a chosen product image have
very different lifetimes (`04-backend-api-conventions.md`):

**Suggestion cache — `/data/cache/imagesearch/`, capped at 1GB.**

- Every fetched candidate is stored once, keyed by the SHA-256 of its
  source URL; the same query later re-serves the cached bytes instead of
  spending another API call.
- A `cached_images` table tracks `hash`, `byte_size`, `content_type`,
  `source_url`, `fetched_at`, `last_accessed_at`.
- **Enforcement of the 1GB cap:** a cleanup routine runs hourly *and*
  immediately after any write that pushes the total over the cap. It
  evicts least-recently-accessed entries until total size is ≤ 90% of the
  cap (a low-water mark, so eviction isn't re-triggered on every
  subsequent write). Entries referenced by a product (i.e. promoted, see
  below) are never in this tier and so are never evicted. @claude: explicitely state, how recent access is managed (using `touch`, I suppose?)
- Per-image sanity limits before storing: reject anything over 5MB or
  whose sniffed content type is not an image. An evicted image is simply
  re-fetched if it is ever needed again. @claude: Oversized images should not be rejected, since we then might refetch the same image over and over. Instead, I want a compressed version of the image. The same goes for sniffed types that do not match images: if it is a gif, we make a static version and store this one.

**Product images — `/data/uploads/products/`, permanent.**

When the user picks a suggestion for a product, the file is **promoted**:
copied out of the suggestion cache into permanent storage, and
`products.image_url` is set to its stable local path. From that moment it
is outside the cache and outside the eviction budget entirely — choosing
an image must never be undone by a cleanup sweep. The same applies to a
catalog suggestion accepted into a storage (`02-data-model.md`) and to a
user-uploaded custom photo, which goes straight to permanent storage.

Deleting a product deletes its permanent image, provided no other product
references the same file.

## Acceptance criteria

- A shopping list with N lines produces exactly N `shopping_list_items`
  rows, each independently resolvable (resolving one item doesn't block
  or auto-resolve others).
- A line whose product is already described in `catalog_products` never
  triggers a Gemini or SerpAPI request.
- No response derived from `catalog_products` contains storage ids,
  counts, owners, or timestamps (`03-auth-and-multi-tenancy.md`).
- Re-running matching is not automatic once a list exists — matching
  happens once at ingestion; if the user edits `raw_text` they get a
  "re-match" action rather than it happening implicitly.
- No `products` or `inventory_batches` row is created until the user
  explicitly confirms a resolution for that line item.
