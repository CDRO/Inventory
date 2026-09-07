# 06 — Photo Ingestion & Spatial Hierarchy

Implements PRD Feature 1 (spatial hierarchy UI) and Feature 2 (photo
ingestion). Depends on: [`02-data-model.md`](02-data-model.md) (`locations`,
`products`, `inventory_batches`, `inventory_logs`),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (upload +
background job pattern), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(shared review component, job polling).

## Goal

A user photographs either a whole shelf/cupboard (up to 50MP) or a single
product. The system identifies items, infers quantities, and — for shelf
photos — proposes where in the storage's location tree each item lives.
The user reviews and edits the proposal before anything is committed to
inventory. **Uploading and reviewing are separate, independently timed
steps** (see "Deferred review", below).

## Location hierarchy management (Feature 1)

Every route below is storage-scoped and behind `RequireStorageMember`
(`04-backend-api-conventions.md`). **Every location id in a request — the
path id, `parent_id`, any move target — must belong to the storage in the
URL.** A reference to a location in another storage is treated exactly
like a nonexistent one: `404 not_found`, never `403`, and never a message
distinguishing the two (`03-auth-and-multi-tenancy.md`).

- `GET /api/storages/{storage_id}/locations` — returns the location tree
  **of that storage only**, as nested JSON:
  `{id, name, description, children: [...]}`. The query is filtered by
  `storage_id`; no other storage's nodes may appear at any depth, and the
  response must never contain a node whose `storage_id` differs from the
  URL's.
- `POST /api/storages/{storage_id}/locations` — body `{name, description,
  parent_id}` (`parent_id` nullable for a root location). If `parent_id`
  is given it must exist **and belong to this storage**; otherwise
  `404 not_found`. The new node inherits the URL's `storage_id` — that is
  never taken from the request body.
- `PATCH /api/storages/{storage_id}/locations/{id}` — rename, or move by
  changing `parent_id`. Reject with `404` if `{id}` or the new
  `parent_id` is not in this storage; reject with `409 conflict` if the
  move would create a cycle (the new parent is the node itself or one of
  its descendants). **Cross-storage moves are impossible by construction:
  both ends are validated against the URL's storage, so a node can never
  be re-parented into another storage.**
- `DELETE /api/storages/{storage_id}/locations/{id}` — reject with `404`
  if the location is not in this storage. Reject with `409 conflict` if
  the location or any descendant still has `inventory_batches` referencing
  it; the user must move or clear that inventory first.
- Frontend: `locations.html` using the shared tree component
  (`js/tree.js`, `05-frontend-pwa-foundations.md`) — expand/collapse,
  inline add-child, rename, and **drag-and-drop re-parenting (required)**.
  Drag-and-drop uses the native HTML5 drag events (no library, per the
  no-toolchain rule), must show a drop-invalid state when the target would
  create a cycle, and must have a keyboard-accessible equivalent
  ("move to…" picker) — required both for accessibility and because
  drag-and-drop is unreliable on touch. The same component serves the
  category tree.

## One batch, one location — and how to split one

A batch is a quantity of one product at **one** location; that is enforced
by `inventory_batches.location_id` being a single FK
(`02-data-model.md`). A batch therefore can never drift into an
inconsistent "partly here, partly there" state.

What the model needs is the *operation* for the physical case where three
jars sit in the cellar and one is carried to the kitchen: that is a
**split**, not a batch spanning two places.

`POST /api/storages/{storage_id}/inventory-batches/{id}/split` — body
`{quantity, target_location_id}`:

1. Validate that `quantity` is between 1 and the source batch's current
   quantity − 1 (splitting the whole batch is a *move*, below), and that
   `target_location_id` belongs to this storage.
2. Decrement the source batch; create a new batch at the target location
   with the split quantity, **copying `expiration_date` and
   `expiration_source`** — the jars are the same jars, so their expiry
   does not reset.
3. Write two `inventory_logs` rows with `reason = 'move'`: negative on the
   source, positive on the target. Net product stock is unchanged, while
   per-location analytics (`11-reporting-and-analytics.md`) stay correct.

`PATCH /api/storages/{storage_id}/inventory-batches/{id}` additionally
accepts `{location_id}` to move an entire batch, logged the same way.

A product that exists in several places is therefore simply several
batches — the "cucumber jars in `Fridge` *and* `Cellar`" case from the
PRD — and every one of them has an unambiguous location.

## Two upload flows

Not every photo is a shelf. Both flows share the upload → job → deferred
review machinery and differ only in prompt and expected payload.

### A. Shelf photo (many items, spatial)

`POST /api/storages/{storage_id}/ingest/shelf-photos` — multipart upload
of one image (up to 50MP, subject to `04-backend-api-conventions.md`
limits), optional `location_id` hint when the user is already scoped into
a specific shelf (e.g. the tree UI's "scan this shelf" action). Creates a
job of kind `shelf_ingestion` and returns `202 {job_id}`.

### B. Single-product photo (one item, no spatial inference)

`POST /api/storages/{storage_id}/ingest/product-photos` — multipart
upload of one image, optional `location_id`. Creates a job of kind
`product_photo` and returns `202 {job_id}`. This is the "I just bought
this — what is it, and add one" path, and the way to identify an unknown
item without paying for shelf segmentation or wading through its noise.

The prompt asks Gemini to identify **one** product: name, brand/size if
legible, and a suggested category. The response uses the same schema as
below with exactly one item, `bounding_box` omitted, and
`proposed_location_path` empty — the location comes from the
`location_id` hint or from the user at review time.

Capture mode (`09-consumption-logging.md`) decides which of these two
endpoints a photo goes to, so the user picks it once rather than being
asked after every shot.

### Shared processing

1. The image is persisted before the AI call (`04-backend-api-conventions.md`),
   so a failed call never loses the photo.
2. A background job sends it to Gemini, parses the structured response,
   and (for shelf photos) resolves each proposed location path against the
   existing tree, marking path segments that do not yet exist as
   `proposed` — created for real only on confirm.
3. The job moves to `done` with the parsed proposal, or `failed` with a
   retryable error.
4. If the configured Gemini model is unavailable, the upload endpoint
   returns `503 model_unavailable` (`01-architecture-and-deployment.md`);
   the UI presents that as a configuration problem for the admin, not as a
   failed photo.

## Gemini prompt/response contract

**Request:** the image, plus a text prompt instructing the model to act as
an inventory vision system and requiring **strict JSON** output matching
the schema below (use Gemini's structured-output / JSON-mode capability
rather than parsing free text, so the response cannot drift from the
contract).

**Response schema (`GeminiShelfAnalysis`):**

```json
{
  "items": [
    {
      "label": "Barilla Penne 500g",
      "confidence": 0.91,
      "quantity": 3,
      "bounding_box": { "x": 0.12, "y": 0.30, "width": 0.10, "height": 0.22 },
      "proposed_location_path": ["Basement", "Right Shelf", "Layer 2", "Front-Right"]
    }
  ]
}
```

- `bounding_box` coordinates are normalized `[0, 1]` fractions of image
  width/height (resolution-independent, so they remain valid regardless
  of how the frontend downscales the image for display).
- `proposed_location_path` is an ordered array from root to leaf; if the
  model can't infer a full path, it may return a shorter array (down to
  `[]`), and the review UI must let the user complete/correct it manually
  — never silently drop an item just because location inference was weak.
- `label` is resolved through the shared matching service in
  `internal/matching` (defined in `07-shopping-list-reconciliation.md` —
  reuse it, do not reimplement it): first against existing `products` in
  this storage, then against the anonymous `catalog_products` table. A
  confident product match references the existing `product_id`; a catalog
  hit becomes a pre-filled new-product candidate (name, category path,
  item type, image) needing no Gemini or SerpAPI follow-up; anything else
  is a blank new-product candidate with the name pre-filled from `label`.

## Deferred review — upload now, review whenever

Uploading and reviewing are **fully decoupled**. A user can photograph six
shelves in ten minutes on a Saturday and review the proposals days later,
across several sittings, without losing anything.

- **Uploads queue.** The upload screen accepts multiple photos in one go,
  firing one job per photo, and returns immediately. It never waits for
  results and never requires the user to stay on the page.
- **Jobs are durable and have no review deadline.** A `done` job holds its
  parsed proposal in `jobs.payload` (`04-backend-api-conventions.md`)
  until it is confirmed or discarded. The uploaded image is retained for
  as long as its job is unreviewed, so cropped regions still render weeks
  later.
- **A review inbox is the entry point:**
  `GET /api/storages/{storage_id}/jobs?status=done|pending|failed` lists
  unreviewed proposals (kind, thumbnail, item count, age). `inbox.html`
  renders it, and a count badge appears next to the storage switcher when
  anything is waiting. That badge is the only nudge — no notifications, no
  emails.
- **Review happens per job**, at `review.html?job={job_id}`, which is a
  normal deep link: bookmarkable, shareable within the storage, and
  openable on a different device than the one that took the photo.
- **Any member of the storage may review any of its jobs**, not only the
  uploader — rights are flat (`03-auth-and-multi-tenancy.md`), and the
  person with time to sort the pantry is often not the person who
  photographed it.
- **Discarding is explicit:** `DELETE /api/storages/{storage_id}/jobs/{job_id}`
  drops the proposal and its image. Nothing is auto-deleted while
  unreviewed; a retention sweep only removes jobs already `consumed` or
  explicitly discarded (images older than 30 days in that state).
- Live polling (`js/jobs.js`) is used only when the user chooses to wait
  on the upload screen. It is a convenience, never the path by which
  results are obtained.

## Review UI

For each detected item, the reviewer sees: the cropped region of the photo
(rendered from `bounding_box`; for single-product photos, the whole
image), the matched/candidate product name (editable, with autocomplete
against existing products), an editable quantity, an editable expiry
(defaulted per `08-expiration-and-classification.md`), and an editable
location path (a picker rooted at this storage's tree, defaulting to the
AI's proposal or the upload's `location_id` hint). Each row carries the
three actions defined in `09-consumption-logging.md` — accept, correct
manually, reject — so a false-positive detection is dropped and a
misidentified item is renamed by hand rather than re-run through the
model.

Manual correction here may also create a new product, and its image may be
**the item's own crop from this photo** (`bounding_box`), which is often
the only usable picture of a niche or unlabeled product. That image stays
local to the storage and is never published to the catalog
(`02-data-model.md`).

`POST /api/storages/{storage_id}/ingest/{job_id}/confirm` — body: the
edited list of `{product_id | new_product: {name, category_id,
item_type}, quantity, location_id, expiration_date}`. Backend, in one
transaction per confirm call:

1. Creates any `new_product` entries, and **inserts** each into
   `catalog_products` (`INSERT ... ON CONFLICT DO NOTHING` — never an
   update; see `02-data-model.md`) so other storages can reuse the
   description later.
2. Creates any newly-referenced `locations` nodes that were only
   `proposed` until now, all within this storage.
3. Creates one `inventory_batches` row per item, with
   `expiration_source = 'user'` when the reviewer edited the date and
   `'derived'` when they accepted the default
   (`08-expiration-and-classification.md`).
4. Writes one `inventory_logs` row per item with `reason='vision_ingestion'`.
5. Marks the job `consumed` (`04-backend-api-conventions.md`) so the same
   proposal cannot be committed twice.

Nothing is written to `inventory_batches`/`inventory_logs` before this
confirm call — the AI proposal alone never mutates inventory.

## Acceptance criteria

- Uploading a photo never blocks the UI; the user may leave the page
  immediately and the job still completes.
- A proposal uploaded today can be reviewed a week later, with its image
  and crops intact, from a different device and by a different member.
- A malformed/unparseable Gemini response results in `job.status =
  'failed'` with a retryable error, not a silent empty proposal.
- Confirming a proposal with zero items is a no-op (nothing created) but
  still marks the job as consumed so it isn't re-reviewed.
- Any location id from another storage — as a path parameter, a
  `parent_id`, a move target, or a confirm-body field — yields `404`,
  indistinguishable from a nonexistent id.
- Splitting a batch preserves the original expiration date on both halves
  and leaves total product stock unchanged.
- Re-uploading a photo of a shelf that already has inventory adds
  additional batches rather than overwriting existing ones (the user
  reconciles duplicates manually in review, e.g. by editing quantity
  instead of creating a duplicate row — this is a UI convenience, not an
  automatic merge).
