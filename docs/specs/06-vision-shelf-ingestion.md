# 06 — Vision Shelf Ingestion

Implements PRD Feature 1 (spatial hierarchy UI) and Feature 2 (shelf photo
ingestion). Depends on: [`02-data-model.md`](02-data-model.md) (`locations`,
`products`, `inventory_batches`, `inventory_logs`),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (upload +
background job pattern), [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(`ProposalReviewList`, `useJobPolling`).

## Goal

User photographs a shelf/cupboard (up to 50MP). The system segments the
photo into individual items, infers quantities, and proposes where in the
storage's location tree each item lives. The user reviews and edits the
proposal before anything is committed to inventory.

## Location hierarchy management (Feature 1)

Before or during ingestion, a user must be able to browse and edit the
`locations` tree for the active storage:

- `GET /api/storages/{storage_id}/locations` — returns the full tree
  (flat list with `parent_id`; frontend assembles it, or backend returns
  nested — pick nested JSON for simpler frontend code:
  `{id, name, description, children: [...]}`).
- `POST /api/storages/{storage_id}/locations` — body `{name, description,
  parent_id}` (`parent_id` nullable for a root location).
- `PATCH /api/storages/{storage_id}/locations/{id}` — rename/move
  (changing `parent_id`); reject a move that would create a cycle.
- `DELETE /api/storages/{storage_id}/locations/{id}` — reject (`409`) if
  the location or any descendant has `inventory_batches` referencing it;
  the user must move/clear inventory first.
- Frontend: `locations.html` using the shared tree component
  (`js/tree.js`, `05-frontend-pwa-foundations.md`) — expand/collapse,
  inline add-child, rename, and a "move to…" picker. The same component
  serves the category tree; drag-and-drop is optional and not required.

An item can exist in multiple physical locations simultaneously (e.g. jars
in `Fridge` and in `Cellar`) — this falls out naturally from
`inventory_batches` having one row per (product, location) rather than one
row per product; no special-casing needed.

## Upload flow

1. `POST /api/storages/{storage_id}/ingest/shelf-photos` — multipart
   upload of one image (up to 50MP, subject to the size limits in
   `04-backend-api-conventions.md`), optional `location_id` hint (if the
   user is scoped into a specific shelf already, e.g. via the location
   tree UI's "scan this shelf" action). Returns `202 {job_id}`.
2. Backend background job sends the image to Gemini with the prompt
   contract below, parses the structured JSON response, and resolves each
   proposed location path against the existing `locations` tree (creating
   new tree nodes for path segments that don't yet exist, e.g. a new
   "Layer 3" under an existing shelf — created as `proposed`, not yet
   attached to any batch, so review can discard them).
3. Job status transitions to `done` with the parsed proposal, or `failed`
   with an error message, per `04-backend-api-conventions.md`.
4. Frontend polls via `useJobPolling`, then renders the proposal in a
   `ProposalReviewList`.

## Gemini prompt/response contract

**Request:** the image, plus a text prompt instructing the model to act as
a shelf-inventory vision system and requiring **strict JSON** output
matching the schema below (use Gemini's structured-output / JSON-mode
capability rather than parsing free text, so the response cannot drift
from the contract).

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
- If the configured Gemini model is unavailable, the upload endpoint
  returns `503 model_unavailable` per
  `01-architecture-and-deployment.md`; the UI surfaces it as a
  configuration problem for the admin, not as a failed photo.

## Review UI

For each detected item, the reviewer sees: the cropped region of the
photo (rendered from `bounding_box`), the matched/candidate product name
(editable, with an autocomplete against existing products), an editable
quantity, and an editable location path (a picker rooted at the existing
tree, defaulting to the AI's proposal). Each item can be individually
removed from the batch before confirming (e.g. a false-positive
detection).

`POST /api/storages/{storage_id}/ingest/shelf-photos/{job_id}/confirm` —
body: the edited list of `{product_id | new_product: {name, category_id,
item_type}, quantity, location_id}`. Backend, in one transaction per
confirm call:

1. Creates any `new_product` entries, and upserts each into
   `catalog_products` (`02-data-model.md`) so other storages can reuse the
   description later.
2. Creates any newly-referenced `locations` nodes that were only
   `proposed` until now.
3. Creates one `inventory_batches` row per item (`expiration_date` defaulted
   per `08-expiration-and-classification.md`).
4. Writes one `inventory_logs` row per item with `reason='vision_ingestion'`.
5. Marks the job `consumed` (`04-backend-api-conventions.md`) so the same
   proposal cannot be committed twice.

Nothing is written to `inventory_batches`/`inventory_logs` before this
confirm call — the AI proposal alone never mutates inventory.

## Acceptance criteria

- Uploading a shelf photo never blocks the UI thread; a loading state is
  shown until the job resolves.
- A malformed/unparseable Gemini response results in `job.status =
  'failed'` with a retryable error, not a silent empty proposal.
- Confirming a proposal with zero items is a no-op (nothing created) but
  still marks the job as consumed so it isn't re-reviewed.
- Re-uploading a photo of a shelf that already has inventory adds
  additional batches rather than overwriting existing ones (the user
  reconciles duplicates manually in review, e.g. by editing quantity
  instead of creating a duplicate row — this is a UI convenience, not an
  automatic merge).
