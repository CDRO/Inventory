# 12 — Client API Contract (third-party native clients)

Depends on: [`02-data-model.md`](02-data-model.md),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md).

## Why this spec exists

Until now the API had exactly one consumer — the PWA in `web/static`, which
ships in the same binary. Both sides changed in one commit, so the API needed
no stability guarantees, no version policy, and no auth mechanism beyond a
browser cookie.

A **native client built in a separate repository, by someone else** breaks all
three of those assumptions. This document is the contract that client codes
against. It is the only spec in the set written for a reader outside this
codebase.

**It does not define new endpoints.** The contract is the existing API surface
defined in `04`–`11`, subject to the rules below. Re-listing those endpoints
here would guarantee the two lists drift apart; when this document and a
feature spec disagree about an endpoint's behavior, the feature spec wins.

## Scope

**Full parity.** Everything the PWA can do, a paired client may do: inventory,
locations, categories, products, ingestion, shopping lists, consumption,
reorder, export, reporting. There is no reduced mobile subset.

Out of scope, deliberately:

- **The admin area.** `/admin/*` and `/api/admin/*` stay browser-only and
  server-rendered (`03-auth-and-multi-tenancy.md`). A paired client is never
  an admin client; requests to those paths get the same `404` as any
  non-admin. The rule is enforced on the **session kind**, not on the
  `Authorization` header: a device session and its owner's browser session
  resolve to the same user, so `is_admin` alone cannot tell them apart, and a
  header is a transport the client picks while the kind was fixed server-side
  when the session was minted. An admin pairing a phone therefore keeps the
  admin area in their browser and does not gain it on the phone.
- **Barcode-database identification.** Recall of locally-associated
  barcodes *is* in scope (see "Barcode recall" at the end); consulting
  external UPC/EAN databases is not, per `00-overview.md`.
- **Telemetry.** A client may collect its own; this server neither receives nor
  stores client analytics.
- **A sync engine.** See "Idempotency" — the server makes a client's retries
  safe. It does not reconcile divergent state, and the non-goal in
  `00-overview.md` stands.

## Pairing and authentication

The client reuses the **existing session mechanism** — no second auth system,
no token table, no JWT. What it adds is a way to *obtain* a session without
typing a password on a phone, and a way to send it without a cookie jar.

### The QR pairing flow

1. In the PWA, signed in, the user opens **Settings → Devices → Pair a device**.
2. The browser calls `POST /api/auth/pairing-codes`. The server creates a
   `pairing_codes` row — CSPRNG code, the issuing `user_id`, `expires_at =
   now() + 2 minutes`, `used_at NULL` — and returns the code.
3. The browser renders a QR containing **both the base URL and the code**:

   ```json
   { "url": "https://nas.tailnet-name.ts.net", "code": "<opaque>", "v": 1 }
   ```

   The URL is not optional. This server is self-hosted behind Tailscale at an
   address the client cannot guess, and asking a user to type a
   `.ts.net` hostname on a phone keyboard is the worst step in the flow.
4. The client scans it and calls `POST /api/auth/pair` at that URL with
   `{code, device_label}` — the label being something the user recognizes
   later, e.g. "Pixel 9".
5. The server validates the code, marks it used, creates a **new `sessions`
   row** for that user, and returns the session id in the response body.

### Rules that make this safe

- **The QR never contains a session token.** It carries a pairing code that is
  single-use and expires in two minutes. A photograph of the screen, a
  shoulder-surf, or a screenshot in a chat is worthless after that window, and
  worthless immediately once redeemed. Putting the session in the QR would make
  a picture of a monitor a permanent credential.
- **Single use, enforced server-side.** Redeeming sets `used_at`; a second
  attempt fails even inside the TTL. Compare codes in constant time.
- **The pairing endpoint is rate-limited** per IP and per user — it is the one
  unauthenticated endpoint that mints a session, so it is the one worth
  guessing at.
- **A device gets its own session row**, not a copy of the browser's. Revoking
  the phone therefore does not sign the user out of their laptop, which is the
  behavior anyone would expect and the reason not to share one session.
- **Device sessions get a longer `expires_at`** than browser sessions — a phone
  that must re-pair every week will simply not be used — but they remain listed
  and individually revocable.

### Sending the session

A paired client sends the session id as `Authorization: Bearer <session-id>`.
The auth middleware in `04-backend-api-conventions.md` accepts the session id
from **either** the cookie or the Bearer header, resolving both through the
same `sessions` lookup. There is one session concept in this system, with two
transports.

`RequireSession` is otherwise unchanged, and every rule in
`03-auth-and-multi-tenancy.md` applies identically to a paired client:
storage scoping, `404`-not-`403` for unknown *and* inaccessible storages, and
no `is_admin` in any response.

### Device management

- `GET /api/auth/devices` — the caller's own sessions: id (a derived
  handle, never the session's bearer token — see
  `03-auth-and-multi-tenancy.md`), label, created, last seen, and which one
  is the current request.
- `DELETE /api/auth/devices/{id}` — revoke one, using the handle from the
  list above, not the session id. Deleting the row revokes immediately
  (`02-data-model.md`).

Users manage only their own devices. This is not an admin surface.

## Idempotency — making an offline queue safe

A mobile client that queues writes while offline **will** retry, and some of
those retries will be for requests the server already processed but whose
response never arrived. Without a server-side guard, the user's fridge gets
debited twice.

- Every **write** request (`POST`, `PATCH`, `DELETE` that mutates) may carry
  an `Idempotency-Key` header holding a client-generated UUID.
- The server records the key with the caller, the storage, the resulting
  status, and the response body, for **7 days**.
- A replay of the same key by the same caller returns the **stored response**
  and executes nothing. Not an error — the same answer, so a client that lost
  the first response converges on the right state by retrying.
- The same key with a *different* request body is a client bug and returns
  `422 validation_failed`. Silently treating it as a replay would hide the
  bug and lose a write.
- Keys are scoped per user and storage; they cannot collide across tenants.
- Omitting the header is allowed — writes stay non-idempotent, which is fine
  for the browser, where the user can see whether the request landed.

**This is not the sync engine that `00-overview.md` rules out.** It makes a
single request safe to repeat. It does not merge divergent state, resolve
conflicting edits, or track client-side revisions. If two devices edit the same
batch, last write wins, exactly as with two browser tabs today.

## Delta sync for cached data

A client that works offline caches the product catalog and shopping list, and
needs to refresh cheaply.

- List endpoints for cacheable entities accept `?updated_since=<RFC3339>` and
  return only rows changed after that instant, using the existing cursor
  pagination from `04-backend-api-conventions.md`.
- This requires `updated_at` on the cacheable entities — `products`,
  `categories`, `locations`, `shopping_lists`, `shopping_list_items` — set on
  every write (`02-data-model.md`).
- **Deletions need tombstones.** A delta built only from `updated_at` can never
  tell a client that a row is gone, so a deleted product would live in the
  cache forever. Deletions of cacheable entities write a `tombstones` row
  (`storage_id`, entity type, id, `deleted_at`), and the delta response carries
  a `deleted` array alongside the changed rows.
- Tombstones are retained **30 days**. A client whose `updated_since` is older
  than the retention window cannot be brought up to date safely, so the server
  responds `409 resync_required` and the client discards its cache and does a
  full fetch. This case is easy to forget and produces a cache that is quietly
  wrong for months; make the server detect it rather than trusting clients to.

### The delta envelope

`?updated_since=` is opt-in and purely additive: a request without it gets the
same response it always got, which is what keeps the PWA — the only consumer
that reads these endpoints today — unaffected. With it, the envelope is:

```json
{
  "items": [ ... ],
  "deleted": ["<id>", "..."],
  "synced_at": "2026-09-21T14:30:00Z",
  "next_cursor": null
}
```

- **`items`** carries the same shape the full list carries, narrowed to what
  changed. A delta is the list filtered, not a second response format.
- **`deleted`** is ids, never rows — the row is gone, and an id is all a client
  needs to drop it. Ids are never reused, so an id here means "this one,
  permanently". Deleting a parent tombstones every node beneath it, so a client
  holding only the parent still learns each descendant is gone.
- **`synced_at`** is the instant the delta was taken, to send back as the next
  `updated_since`. It is not optional decoration: none of these endpoints
  serialize `updated_at` on their items, so without it a client has nothing to
  compute its next cursor from, and using its own clock would shift every sync
  by the skew between the phone and the NAS — in the direction that skips
  changes.

  **`synced_at` is not the wall clock**, and the difference is a data-loss bug
  rather than a nicety. Postgres `now()` is transaction-*start* time, and every
  row here is stamped with it. A write that begins at 10:00:00.000 and commits
  at 10:00:00.500 therefore carries the timestamp 10:00:00.000, while being
  invisible to a delta running at 10:00:00.200 — so handing that delta's client
  `10:00:00.200` as its cursor would skip a row stamped `10:00:00.000` on every
  future request, and that deletion would never reach the client at all. The
  server instead takes the start of the oldest transaction still in flight: no
  row it cannot yet see can carry a timestamp earlier than that. `updated_since`
  is compared **inclusively** (`>=`) for the boundary this creates, since a row
  belonging to the transaction that set the watermark carries exactly that
  timestamp.

  Together these make delta sync **at-least-once**: a row may arrive in two
  consecutive deltas, which a client overwrites with the same value, and never
  in none.
- **`next_cursor`** is present for the same reason it is on every collection.
  These three endpoints return their collections whole, so it is always null
  today; when one gains pagination, the delta rides on the same cursor.

**Tree endpoints answer a delta flat, not nested.** `categories` and
`locations` return a nested tree for an ordinary list. A delta contains only
the nodes that changed, so a changed child whose parent did not change has no
parent in the payload and would deserialize as a root — the client would file
it at the top of its tree and never learn otherwise. In a delta each node
therefore carries `parent_id` and no `children`, and the client rebuilds the
tree itself from a flat cache keyed by id. For the same reason every nullable
field is serialized even when null: in a delta an absent key reads as
"unchanged", so a cleared description or a shelf-life rule reset to *inherit*
has to be present-and-null to reach the cache at all.

**The resync boundary is computed from the retention window**, not from the
oldest tombstone still in the table. The two are not equivalent: once the sweep
has removed the only tombstone a storage ever had, "oldest surviving tombstone"
is undefined and a year-old cursor would be answered as if nothing had ever been
deleted. Retention is also the less conservative rule — a storage whose oldest
surviving tombstone is 25 days old can still answer a 28-day-old cursor
correctly. The sweep and the boundary read the same constant so they cannot
drift apart.

**Not yet delta-capable: shopping lists.** `shopping_lists` and
`shopping_list_items` are listed above as cacheable and have tombstone entity
types reserved, but there is no shopping-list *list* endpoint to hang
`?updated_since=` on — a list is created and then fetched by id — and no
deletion path writes those tombstones. When a list endpoint for them exists it
takes the same parameter and the same envelope.

## What the server must never trust from a client

The client is written by someone else, in a codebase this project does not
review. Everything it sends is untrusted input.

- **`product_id`** — verified to belong to the caller's storage before use,
  every time. A client-supplied id from another storage is a `404`, as always.
- **The client's identification result** is a *proposal*, not a fact. A client
  that recognized an item on-device sends `raw_label` text; the server resolves
  it to a product through the shared matching service in
  `07-shopping-list-reconciliation.md`, stage 1 and 2 only.

  This is what keeps the client's cost saving intact **and** the data
  trustworthy: re-resolution costs a trigram query, not a vision call, so the
  expensive thing the client avoided is still avoided.
- **`inference_source`, `model_id`, `captured_at`** are stored as metadata and
  must never influence authorization, pricing, or business logic. `captured_at`
  is informational; server time remains authoritative for expiry arithmetic and
  ordering.
- **`is_admin`, storage membership, quantities on hand** — never accepted from
  a client in any form.

## Versioning and stability

The client ships on its own schedule through an app store; a breaking change
deployed on the NAS reaches phones that cannot be updated in step.

- **The current surface is v1.** Paths stay `/api/...`; no path change is
  introduced now, because renaming every endpoint to buy a version number the
  PWA does not need is churn.
- **Additive changes are always allowed** and require no coordination: new
  endpoints, new optional request fields, new response fields. Clients must
  ignore unknown response fields rather than reject them — state this in the
  client's own documentation, because a strict parser turns an additive change
  into an outage.
- **Breaking changes require `/api/v2/`**, served alongside `/api/` for a
  stated deprecation window. A breaking change is: removing or renaming a
  field, changing a type, tightening validation, changing a status code, or
  altering the meaning of an existing value.
- Clients send `X-Client-Version: <name>/<version>` on every request. It is for
  diagnostics only and must never gate behavior — version-sniffing to alter
  responses is how one API quietly becomes several. The server reads it in
  exactly one place, the error serializer, where it is attached to the log line
  for a failed request and nothing else; that is structural rather than a
  convention, because the one function that sees the header is producing a
  response that has already been decided and has no way to change it. Two
  requests differing only in this header get byte-identical answers. (Request
  logging in general belongs to `18-operations-logging-audit-upgrades.md`.)
- Changes to this contract are changes to this spec file. A PR that alters an
  endpoint's shape must say so here, or the reviewers in the ship loop should
  block it.

## Barcode recall

*(This section originally deferred barcode lookup to the Android spike for
containment. That containment ended on 2026-09-20, when the owner accepted
the narrower recall form as core work.)*

Barcode **recall** — resolving a locally-associated code to a product, and
the quick log flow built on it — is specified in
[`20-barcode-recall.md`](20-barcode-recall.md) and is part of the additive
v1 surface: a paired client may call its endpoints like any others, under
the same scoping, idempotency, and `404` rules. What remains outside this
contract is what remains outside the whole system: identification of
unknown items via external UPC/EAN databases
(`00-overview.md`). The containment history lives in
`docs/spikes/21-native-android-app.md` (formerly `S-01`).

## Acceptance criteria

- A pairing code is single-use and expires in 2 minutes; a replayed or expired
  code fails, and the failure is indistinguishable from an invalid one.
- The QR payload contains a pairing code and base URL, never a session id.
- Revoking a phone in Settings → Devices leaves the browser session working.
- A paired client sending `Authorization: Bearer` is subject to identical
  storage scoping and `404` behavior as the browser — verified by a test that
  requests another storage's id over Bearer auth.
- `/admin` and `/api/admin/*` return `404` to a paired client regardless of the
  user's `is_admin` value.
- The same `Idempotency-Key` replayed returns the stored response and creates
  no second `inventory_logs` row.
- The same key with a different body returns `422`.
- A delta request with an `updated_since` older than tombstone retention
  returns `409 resync_required` rather than a silently incomplete delta.
- A client-supplied `product_id` belonging to another storage returns `404`.
