# 03 — Auth & Multi-Tenancy

Depends on: [`02-data-model.md`](02-data-model.md) (`users`, `sessions`,
`storages`, `storage_members`).

## Principles

- **No public registration.** There is no sign-up page and no invite-by-
  email flow. Every `users` row is created by an admin, through the admin
  view described below.
- **Storages are the tenancy boundary, and their existence is itself
  confidential.** A user's access is entirely determined by
  `storage_members`. Beyond that, a non-admin user must have **no way to
  learn that any other storage exists** — not its name, not its id, not
  its size, not the total number of storages, not even that the number is
  greater than one. See "Non-enumeration rules" below; this constrains
  error codes, id allocation visibility, and analytics.
- **Flat rights within a storage.** There are no per-storage roles
  (no "owner"/"editor"/"viewer" distinction). Every member of a storage
  can read and write everything belonging to that storage: locations,
  categories, products, batches, logs, shopping lists.
- **Admin is a separate, orthogonal capability.** `users.is_admin` grants
  access to the admin view (user/storage management) only. It does not by
  itself grant access to any storage's inventory data — an admin who wants
  to use a storage must also be added to it via `storage_members`, exactly
  like any other user.

## Session mechanism

Cookie-based, server-side sessions. The cookie carries **an opaque random
session id and nothing else** — no user id, no username, no role, no
flags, no signed claims. Everything about the caller is looked up
server-side from the `sessions` table on every request, so there is
nothing in the client's possession that could be edited to change who they
are or what they may do.

- `POST /api/auth/login` — body `{username, password}`. Verifies
  `password_hash` with **argon2id**, creates a `sessions` row, and sets
  the session id in an `HttpOnly`, `Secure`, `SameSite=Lax` cookie.
- `POST /api/auth/logout` — deletes the `sessions` row (immediate,
  server-side revocation) and clears the cookie.
- `GET /api/auth/me` — returns `{id, username, display_name, storages: [{id, name}]}`.
  **`is_admin` is deliberately not part of this response** (see below).

All non-auth API routes require a valid, unexpired session; return `401`
otherwise (see `04-backend-api-conventions.md` for the error envelope).

## Admin status is never client-visible or client-trusted

`is_admin` is a server-side fact, checked against the database on **every
single admin-gated request**. It is never returned by any JSON API, never
placed in a cookie, token, or session payload sent to the browser, and
never used as a client-side rendering condition.

Consequently:

- **The entire admin area is server-rendered** with Go `html/template` at
  `/admin`, per `01-architecture-and-deployment.md`. The server decides,
  per request and per DB lookup, whether to render an admin page at all.
- **The JavaScript application contains no admin code** — no admin routes,
  no admin views, no "if admin" branches. There is nothing in the shipped
  frontend for a user to unlock by flipping a client-side value, because
  the admin UI does not exist in it.
- Client-side JS on admin pages is limited to **form validation and visual
  helpers** (show/hide a confirm dialog, disable a submit button). No
  authorization decision, data fetch gating, or page composition may
  depend on it.
- A non-admin requesting any `/admin` page or `/api/admin/*` route gets
  the same `404` treatment described below — the admin area does not
  announce its own existence either.

## Initial admin bootstrap

On first startup with an empty `users` table, the backend creates one
admin user from `ADMIN_INITIAL_USERNAME` / `ADMIN_INITIAL_PASSWORD`
(`01-architecture-and-deployment.md`), with `is_admin = TRUE`. This is the
only account creation path that does not go through the admin view itself
— it exists so there is a way to log in and create further users/storages
on a fresh deployment.

## Admin view

Server-rendered pages under `/admin`, plus the JSON routes they post to.
Every route below re-checks `is_admin` against the database.

| Action | Route |
|---|---|
| List users | `GET /api/admin/users` |
| Create user | `POST /api/admin/users` — body `{username, password, display_name, is_admin}` |
| Delete/deactivate user | `DELETE /api/admin/users/{id}` |
| List storages | `GET /api/admin/storages` |
| Create storage | `POST /api/admin/storages` — body `{name}` |
| Delete storage | `DELETE /api/admin/storages/{id}` |
| List a storage's members | `GET /api/admin/storages/{id}/members` |
| Grant a user access to a storage | `POST /api/admin/storages/{id}/members` — body `{user_id}` |
| Revoke a user's access to a storage | `DELETE /api/admin/storages/{id}/members/{user_id}` |
| Read/update app settings (e.g. Gemini model) | `GET`/`PUT /api/admin/settings` |
| Download regenerated `.env` | `GET /api/admin/settings/env-file` |
| Search the global product catalog | `GET /api/admin/catalog?q=` |
| Delete a catalog entry (moderation) | `DELETE /api/admin/catalog/{id}` |

Catalog moderation exists because `catalog_products` is insert-only
(`02-data-model.md`): a bad or abusive entry cannot be corrected by its
author or anyone else, so deletion by an admin is the only remedy.
Deleting a catalog row never touches any storage's own `products`.

The admin UI is a plain server-rendered table view: user list with a
"storages" column, storage list with a "members" column, add/remove
actions, and the AI-model banner/selector described under AI model
resilience in `01-architecture-and-deployment.md`. Deleting a user must
also delete their `sessions` rows, revoking access immediately.

## Non-enumeration rules

Applied by the storage-scoping middleware on every route under
`/api/storages/{storage_id}/...`:

1. Confirm the caller has a valid session.
2. Confirm `storage_members` contains a row for `(storage_id, caller.id)`.
3. If not — whether the storage does not exist, or exists but the caller
   is not a member — respond with an **identical `404 not_found`**.
   Deliberately *not* `403`: a `403` confirms that the id refers to a real
   storage, which is exactly the leak this rule exists to prevent. The
   response body, headers, and timing characteristics must not differ
   between the two cases.

Further rules elsewhere in the system:

- No endpoint returns a global count, list, or id range of storages to a
  non-admin caller.
- `GET /api/auth/me` returns only the storages the caller belongs to; an
  empty list for a user with no memberships is a normal state, not an
  error that hints at hidden storages.
- Analytics and reporting (`11-reporting-and-analytics.md`) never
  aggregate across storages.
- `catalog_products` (`02-data-model.md`) is the one global table, and is
  explicitly stripped of storage references, counts, and ordering
  metadata for this reason. A user seeing a catalog suggestion learns that
  a product exists in the world, never that another storage exists.

## Dev-mode error reasons

Opaque `403`/`404` responses are correct in production and unhelpful while
developing. When `APP_ENV=dev` (`01-architecture-and-deployment.md`), every
authorization failure additionally includes a `debug_reason` field naming
the exact check that failed:

```json
{
  "error": {
    "code": "not_found",
    "message": "Not found.",
    "debug_reason": "not_storage_member: user 7 is not a member of storage 3"
  }
}
```

Defined values: `session_missing`, `session_expired`, `not_storage_member`,
`storage_not_found`, `not_admin`, `admin_area_hidden`.

The field is omitted unconditionally whenever `APP_ENV != dev`. This must
be enforced at the single point where the error envelope is serialized —
not by remembering to strip it per handler — so that a production build
cannot leak it. The same applies to logging: dev logs the reason at info
level; production logs it server-side only, never in the response.

## Frontend behavior

- On login, `GET /api/auth/me` determines the user's storage list. If the
  user belongs to exactly one storage, the app opens directly into it. If
  more than one, present a storage switcher (see
  `05-frontend-pwa-foundations.md`) and remember the last-selected storage
  (in `localStorage`) for next login.
- If a user belongs to zero storages, show an empty state explaining that
  an admin needs to grant them access — do not show a broken/empty
  inventory UI, and do not imply that storages exist which they cannot
  see.
- The frontend never renders navigation to `/admin`; the admin area is
  reached by URL and gated server-side.
