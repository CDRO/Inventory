# 32 — Discard the Whole Inbox

Depends on: [`04-backend-api-conventions.md`](04-backend-api-conventions.md)
(jobs table and lifecycle), [`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md)
(the review inbox, "Discarding is explicit"),
[`12-client-api-contract.md`](12-client-api-contract.md) (additive changes).

Amends: `06`, "Discarding is explicit" — it gains a pointer to this spec.
Per-job discard is unchanged.

## Why this spec exists

The inbox holds every job in a storage that has not been confirmed:
proposals waiting for review, photos still being analysed, and failures.
Nothing leaves it on its own (`06`: "Nothing is auto-deleted while
unreviewed"), which is right for one forgotten photo and wrong for forty.
After a batch upload the model got wrong, or a week of test photos, the
only way out today is to press "Discard" and confirm a dialog once per job,
twenty jobs per page.

This spec adds one action that empties the inbox in one confirmed step.

## What "the whole inbox" means

The inbox is every job of the storage whose `status` is `pending`, `done` or
`failed` — exactly what `GET /api/storages/{storage_id}/jobs` lists. A
`consumed` job has already been applied to inventory. It is not in the
inbox, and discarding it would mean nothing, so this spec never touches one:
the retention sweep in `06` still owns consumed jobs.

A `pending` job is included. Discarding a job while its model call is still
running is already allowed for one job (the runner finds the row gone and
drops the result, `internal/httpapi/jobs.go`), and this spec gives the bulk
action the same rule.

## The race the endpoint has to close

Rights are flat (`03-auth-and-multi-tenancy.md`). Another member can upload a
photo in the seconds between the inbox being rendered and "Discard all"
being pressed. That photo was never on the screen, so the person discarding
did not decide anything about it, and it must survive.

The client therefore says **up to which job** it means, using a value the
server gave it: the `created_at` of the newest job on the first page of the
inbox, which is also the first job on the page (the list is newest first).
A server timestamp is used deliberately rather than the device's clock:
a phone that runs two minutes slow would otherwise discard photos uploaded
after the list was drawn.

## Endpoint

`DELETE /api/storages/{storage_id}/jobs?up_to=<RFC3339 timestamp>`

- `up_to` is **required**. If it is missing or does not parse, the answer is
  `422 validation_failed` and nothing is deleted. A bare
  `DELETE …/jobs` that wiped the storage's whole inbox would be too easy to
  send by accident, for example from a script with an empty variable.
- The endpoint deletes every job of the URL's storage with
  `status IN ('pending','done','failed')` and `created_at <= up_to`, in one
  statement. Internally, that statement's `RETURNING id, image_filename`
  hands the handler what it needs for file clean-up. None of it reaches the
  HTTP response, which carries only the count (below).
- Once the rows are gone, the handler removes each job's photo and its cutouts, the
  same way `DELETE /jobs/{id}` does (`internal/httpapi/jobs.go`). That clean-up
  is best effort, exactly as it is for one job. The rows are already gone, so
  a file that fails to delete becomes an orphan on disk and is logged. It is
  not an error the user sees.
- The response is `200 {"discarded": <n>}`. It is `200` even when `n` is 0,
  because an empty inbox is a valid state and not a missing resource.
- Standard storage scoping applies (`03`). A non-member gets `404`, the
  same answer as for a storage that does not exist.

This is an additive route (`12`: additive changes need no version bump).
The `DELETE /jobs/{id}` route is unchanged.

## Frontend (`inbox.html`)

- A **"Discard all"** button appears in the inbox heading row, next to
  "Scan photos", whenever the inbox is not empty. It is a secondary (ghost)
  action. Discarding is irreversible, and the inbox's main job is still
  reviewing photos.
- On first load, the page remembers the `created_at` of the first job it
  receives. Loading more pages with "Show older" does not change that value.
- Pressing the button opens a confirmation. The wording makes three things
  clear: it includes pages not yet loaded, it includes photos still being
  analysed, and nothing from them has reached the inventory. For example:
  *"Discard every photo in the inbox, including older ones not shown and
  ones still being analysed? Nothing from them has been added to your
  inventory."* The confirmation is the same `window.confirm` pattern the
  per-job discard already uses.
- If the user confirms, the page sends `DELETE …/jobs?up_to=…` and then
  **re-fetches the first page**, rather than clearing the list locally. If
  a job arrived after the list was drawn, it is still there after the
  re-fetch, and it is shown. The success notice reads *"Discarded N
  photos."*, using the server's count.
- The inbox badge (`js/inbox-badge.js`) needs no work of its own. It already
  reflects the server's state whenever it is next fetched.

## Acceptance criteria

- `DELETE …/jobs?up_to=T` deletes every `pending`, `done` and `failed` job
  of that storage created at or before `T`. It leaves `consumed` jobs, jobs
  created after `T`, and every other storage's jobs untouched.
- A missing or unparseable `up_to` answers `422` and deletes nothing.
- Each discarded job's photo and cutouts are removed from disk. If removing
  a file fails, that is logged and the response is still `200`.
- A non-member of the storage gets `404`, identical to the answer for an
  unknown storage id.
- The response's `discarded` count equals the number of rows deleted.
- Go tests cover the status filter, the `up_to` boundary (a job exactly at
  `T` is deleted, one later is not), storage scoping, `422` for a missing or
  unparseable `up_to`, and a failing file removal that still answers `200`.
- E2E fixture: `e2e/fixtures/seed.sql` gains a **dedicated storage and a
  dedicated user for this journey only**, for example "E2E Inbox" and
  `e2e-inbox`. The storage holds one `done`, one `failed`, one `pending` and
  one `consumed` job with fixed `created_at` values. The journey deletes
  jobs, so it must not share a storage with `ingestion.spec.js` or
  `consumption.spec.js`, which depend on their own seeded jobs and run in
  parallel.
- E2E: as `e2e-inbox`, open the inbox, press "Discard all" and confirm.
  The inbox then shows its empty state, and the consumed job's batch still
  exists.
- E2E, client bound: with Playwright's route interception, capture the
  `DELETE` request and assert that its `up_to` equals the `created_at` of
  the first job in the list response. The test also sets the page's clock
  (`page.clock`) to an hour before the seeded jobs. If the client used the
  device clock instead of the server's timestamp, the bound would be
  different and the assertion would fail.
