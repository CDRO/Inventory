# 41 — Mixed-Batch Photo Classification: Spotting a Shopping List Among Shelf Photos

Depends on: [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(the Gemini prompt/response contract and shared processing this spec
extends), [`07-shopping-list-reconciliation.md`](07-shopping-list-reconciliation.md)
(the photo-shopping-list ingestion path and matching service this spec
finishes and reuses), [`09-consumption-logging.md`](09-consumption-logging.md)
("Capture mode — decided before the photo, never after," the design this
spec deliberately does not override), [`36-photo-source-picker.md`](36-photo-source-picker.md),
[`37-in-page-camera.md`](37-in-page-camera.md) (photo capture, unaffected —
this spec is entirely server-side and review-screen behavior).

## Why this spec exists

`07` already specifies a photo-source shopping list end to end — schema,
route, "Gemini OCRs/extracts line items into a plain string array" — but
the handler has never been built: it answers every `source: "photo"`
request with `501 not_implemented`. That is the first gap this spec
closes, by giving `07`'s already-written contract an implementation
path.

The second gap is the one actually reported: someone shooting a run of
shelf or product photos (`06`, `09`) may have a shopping list mixed into
that same run — a handwritten note photographed alongside the shelves
because it was easier than switching modes mid-task. `09`'s sticky
capture mode exists specifically so the person declares intent once and
is never asked again per photo; naively re-asking "is this a shelf or a
list?" on every upload would undo exactly the friction `09` was built to
remove. The fix is not a new question per photo — it is the vision call
that already looks at the photo saying, when it's genuinely wrong, "this
doesn't look like what you told me," and leaving the *decision* to the
person, once, only when there is a real mismatch to decide about.

## Classification rides along on the existing analysis call — no second AI call

Every upload through `06`'s `shelf-photos`/`product-photos` endpoints and
`09`'s `consume/photos` endpoint already sends the image to Gemini once
and gets back structured JSON (`06`, "Gemini prompt/response contract").
This spec adds two optional top-level fields to that response schema,
for all three existing modes (`ModeShelf`, `ModeProduct`,
`ModeConsumption` in `internal/vision/gemini.go`):

```json
{
  "items": [ /* unchanged, per 06/09 */ ],
  "looks_like_shopping_list": false,
  "shopping_list_lines": []
}
```

- The prompt for all three modes gains an instruction: if the image is
  primarily a handwritten or printed list of items (not physical shelved
  or held products), set `looks_like_shopping_list: true` and populate
  `shopping_list_lines` with the model's best-effort line-by-line
  transcription — the exact same extraction `07`'s own photo path needs,
  requested in the same call rather than a second one. `items` is
  typically empty in that case, but the schema does not forbid both being
  populated (a photo showing shelved items and a sticky note both, an
  ambiguous edge case — see "Review UI," below).
- No new Gemini mode, no extra round-trip, no added latency or cost on
  the common case (an ordinary shelf photo simply gets
  `looks_like_shopping_list: false` back, exactly as free as today).
- This reuses `Analyze()`'s existing structured-output mechanism (`06`) —
  a schema field addition, not a new integration.

## Finishing `07`'s photo shopping-list path

`POST /api/storages/{storage_id}/shopping-lists` with `source: "photo"`
(`07`) is implemented per `07`'s own already-written contract: multipart
upload, background job, Gemini extracts `shopping_list_lines` using the
same prompt instruction as above (a photo uploaded *directly* as a
shopping list skips the shelf/product/consumption framing entirely and
asks Gemini to read it as a list from the start), each line then run
through the shared matching service (`07`) exactly as a text-source list
already is. This endpoint gains one more optional body field for the
mixed-batch case below: `from_job_id`.

- `from_job_id` (optional, alternative to uploading a photo): the id of
  an existing `shelf_ingestion`/`product_photo`/`consumption` job whose
  stored analysis already has `looks_like_shopping_list: true` and a
  non-empty `shopping_list_lines`. When present, no new upload and no new
  Gemini call happen — the endpoint creates the `shopping_lists` row
  directly from that job's already-extracted lines, runs the matching
  service over them exactly as any other photo-source list, and marks
  the origin job `discarded` (`06`'s existing job-status vocabulary —
  the same state a rejected proposal already reaches). `404` if the job
  does not belong to this storage, is not in a state carrying
  `looks_like_shopping_list: true`, or has already been discarded/
  resolved/reclassified once (reclassifying is not idempotent, the same
  reasoning `39`'s `destroy` endpoint uses for "already gone").
- This is the mechanism the review-screen banner below calls — a photo
  the person shot as a shelf photo, correctly spotted as a list, becomes
  a real shopping list without ever being re-uploaded or re-analyzed.

## Review UI: a distinct banner, never a silent reroute

A job whose stored analysis carries `looks_like_shopping_list: true`
keeps its original kind and proceeds to its normal review screen
(`06`/`09`) exactly as any other job would — **nothing is silently
rerouted**. That review screen additionally shows a banner, ahead of the
item list: "This looks like a shopping list, not a
[shelf photo/product photo/consumption photo] — Process as shopping
list / Keep as [original kind]."

- **Process as shopping list** calls `07`'s ingestion endpoint with
  `from_job_id` set to this job. On success, navigate to the newly
  created shopping list's resolution screen (`07`), the same place a
  person lands after any other list ingestion.
- **Keep as [original kind]** dismisses the banner and proceeds through
  the job's normal review UI unchanged — if `items` came back empty (the
  common case for a genuine list misdetected as e.g. a shelf), that is
  simply an empty proposal, the same as a blurry or empty-shelf photo
  produces today; nothing about this spec changes what an empty item
  list looks like in review.
- If `items` is **also** non-empty (the ambiguous both-populated case),
  the banner still shows, and dismissing it (or ignoring it and reviewing
  items normally) leaves the item list fully intact underneath — the
  banner is an offer, never a gate blocking the rest of the screen.
- The banner's copy names the mode the photo was actually taken under
  ("not a shelf photo," "not a product photo," "not a consumption
  photo") so a person mid-run of twenty photos immediately knows which
  one in the batch this is, without hunting.

## What this deliberately does not do

- **The sticky capture mode is unchanged.** `09`'s "decided before the
  photo, never after" stands exactly as written — a person still picks
  Stocking up / Using up / Shelf scan once, and every photo in the run
  still goes to that mode's endpoint. This spec never asks the person to
  choose a mode per photo, before or during capture; classification is
  purely something the review screen can *surface*, after the fact, for
  the person to act on if and when they choose to.
- **No reverse check.** A photo uploaded directly as a shopping list
  (`07`'s own endpoint, source `"photo"`) is not re-classified as
  "maybe actually a shelf" — the person explicitly chose that endpoint
  by explicitly choosing that mode, so there is no ambiguity to surface
  in that direction. Out of scope for this spec.
- **No new job kind.** A misclassified-as-shelf photo that gets
  reprocessed as a shopping list does not need `store.JobShoppingListPhoto`
  (already reserved, `internal/store/jobs.go`) — that kind is for photos
  uploaded directly as lists through `07`'s own endpoint. A reclassified
  job is discarded outright once its lines have been transferred, per
  "Finishing `07`'s photo shopping-list path," above; it is not
  converted in place.

## Acceptance criteria

- A shelf, product, or consumption photo whose Gemini response carries
  `looks_like_shopping_list: false` (the overwhelming majority) behaves
  identically to today — no visible change, no added latency.
- A photo detected as a likely shopping list shows the banner on its
  normal review screen, names the mode it was captured under, and never
  auto-navigates away from that screen on its own.
- "Process as shopping list" creates a real `shopping_lists` row (via
  `from_job_id`) from the already-extracted lines, resolves it through
  the same matching service every other shopping list uses (`07`), costs
  no second Gemini call, and marks the origin job `discarded`.
- "Keep as [original kind]" proceeds through that job's ordinary review
  flow, unmodified by this spec.
- Calling `from_job_id` against a job that isn't flagged
  `looks_like_shopping_list`, belongs to another storage, or has already
  been reclassified/discarded/resolved is `404`.
- `POST /api/storages/{storage_id}/shopping-lists` with `source: "photo"`
  and a real multipart upload (no `from_job_id`) works end to end per
  `07`'s own already-written contract — this is the fix for the
  standing `501`.
- E2E: a new `e2e/specs/mixed-classification.spec.js` (or an addition to
  `e2e/specs/ingestion.spec.js`) stubs a shelf-photo upload whose analysis
  response carries `looks_like_shopping_list: true` and non-empty
  `shopping_list_lines`, asserts the banner appears on the review screen,
  and covers both banner actions end to end — including that "keep as
  shelf" leaves the (empty) item review intact and "process as shopping
  list" lands on a real, resolvable shopping list.
