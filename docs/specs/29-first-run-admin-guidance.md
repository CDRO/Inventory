# 29 — First-Run Guidance for Admins

Depends on: [`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md)
(the admin area, non-disclosure, "Frontend behavior"),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md) (the
storages page, the service worker).

Amends: the last two bullets of "Frontend behavior" in `03` — they gain a
pointer to this spec; nothing in them is weakened.

## Why this spec exists

On a fresh deployment the only account is the bootstrap admin, and that admin
belongs to **no storage**: `is_admin` grants the admin view only, and access to
a storage needs a `storage_members` row like anyone else (`03`, "Admin is a
separate, orthogonal capability"). So the first thing the operator sees after
their first login is the empty state of the storages page — *"No storage yet.
Ask an admin to add you to a storage."* For the one person who **is** the admin
that is a dead end: nothing on the page says the admin view exists, and `03`
says the frontend never renders navigation to `/admin`, so they have to know to
type the URL.

The wanted behavior: an admin who belongs to no storage is taken to the admin
view, where they can create the storage and add themselves.

## Why the obvious implementation is forbidden

The obvious implementation is a link on the empty state, shown "if the user is
an admin". `03` ("Admin status is never client-visible or client-trusted")
rules out every ingredient of it:

- `is_admin` is never returned by any JSON API and is **never a client-side
  rendering condition**, so the page cannot decide by itself whether to show
  the link.
- The shipped JavaScript contains **no admin code and no navigation to
  `/admin`**.
- A link shown to everyone would announce the admin area to people who get a
  `404` from it — and `03` says the area does not announce its own existence.

So the decision moves to the only place that may make it: **the server, per
request, from the database**. The client is handed a neutral navigation target
and never learns what the server decided.

## Behavior

### `GET /no-storages`

A browser navigation route. It has no body: it only redirects. It is not an
API route and not part of the admin area — it must **not** sit behind the admin
gate, because every user with zero storages passes through it. The server
decides from the database state of *this request*:

| Caller | Response |
|---|---|
| No valid session | `302` to the login page — the same target the SPA's `401` handling uses |
| Valid session, at least one `storage_members` row | `302` to `/storages.html` |
| Valid session, zero memberships, `users.is_admin = TRUE` | `302` to `/admin` |
| Valid session, zero memberships, not an admin | `302` to `/storages.html?empty=1` |

- `is_admin` is read from the database on every call, exactly as the admin gate
  does. It is never cached on the session and never taken from anything the
  client sent.
- Every response carries `Cache-Control: no-store`.
- `GET` only. Any other method gets the standard `404` envelope of every
  unrouted request (`04-backend-api-conventions.md`).
- The `Location` of an admin's redirect is sent only to that admin. No other
  response, in any state, mentions `/admin`.

### The storages page

`web/static/js/pages/storages.js` handles the zero-storage case as follows:

- Zero storages **and** `empty=1` in the query string: render the existing
  empty state (`renderEmptyState`), unchanged in copy and layout. No
  navigation.
- Zero storages and **no** `empty=1`: navigate to `/no-storages` with
  `location.replace`, so the Back button does not bounce the user through the
  redirect again.
- One or more storages: unchanged.

The page never learns which of the four responses it got. It only ever sees
its own outcome: a non-admin ends on `storages.html?empty=1`, an admin ends on
the admin page, and neither is told anything about the other.

### What the operator experiences

Fresh deployment: first login → `storages.html` → `/no-storages` → `/admin`.
The admin creates a storage and adds themselves as a member (`03`: no automatic
access), returns to the app, and now has a storage, so the redirect no longer
applies. An admin who belongs to at least one storage is **never** redirected;
they can still reach `/admin` by URL, as before.

The redirect applies on every navigation to the storages page while the user has
zero memberships, not only on the first login: it is stateless on purpose, with
no "first login" flag to store or to get wrong. An admin with zero memberships
who wants something else (for example `settings.html`) navigates there
directly; only the storages page bounces them.

## Acceptance criteria

- [ ] `GET /no-storages` with no valid session is a `302` to the login page.
- [ ] With a valid session and at least one membership it is a `302` to `/storages.html`, whether or not the user is an admin.
- [ ] With a valid session, zero memberships and `is_admin = TRUE` it is a `302` to `/admin`.
- [ ] With a valid session, zero memberships and `is_admin = FALSE` it is a `302` to `/storages.html?empty=1`.
- [ ] `is_admin` is read from the database on each request: a test uses one session, calls the route, revokes the user's admin flag directly in the database, calls it again and sees the redirect change from `/admin` to `/storages.html?empty=1` without a new login.
- [ ] Every response from the route carries `Cache-Control: no-store`; any method other than `GET` returns the standard `404` envelope.
- [ ] No JSON response and no cookie changes: `GET /api/auth/me` still returns exactly `{id, username, display_name, storages}` and no field that differs for an admin.
- [ ] A test scans every file embedded from `web/static/` and fails if any contains the string `/admin` (the existing test in `web/embed_test.go` covers paths only; this covers content).
- [ ] On `storages.html`, zero storages without `empty=1` navigates to `/no-storages` using `location.replace`; zero storages with `empty=1` renders the empty state and does not navigate; there is no path on which the page redirects to itself.
- [ ] The service worker neither pre-caches nor answers from cache a navigation to `/no-storages` (it is in no cache list), and if `storages.js` is served from a cached copy, `CACHE_VERSION` is bumped so installed clients pick up the change (`05`).
- [ ] E2E (`05`): a freshly seeded deployment's bootstrap admin, after logging in, ends on the admin page without typing a URL.
- [ ] E2E: a non-admin with no storage ends on the empty state and stays there; an admin who belongs to a storage lands on the normal storage flow and is not redirected.

## Watch for

- The redirect targets differ by state, so the tests must assert the **exact**
  `Location`, not just "a redirect happened".
- Do not add `is_admin`, a role, or any hint of it to `GET /api/auth/me`, to a
  cookie, to `localStorage`, or to a query string the server *generates* for the
  client. `?empty=1` is set by the server only for non-admins and carries no
  admin information; an admin who types it by hand merely sees the empty state.
- The route must not be placed inside the group guarded by `RequireAdmin`: that
  gate answers `404` to everyone else, which would strand exactly the users who
  need the redirect. It needs the session gate only.
- Keep `router.go` append-only: add the route, do not reorder or regroup the
  existing ones.
- Redirecting an admin away from the storages page is only right while they have
  zero memberships. Test the boundary — after adding themselves to a storage the
  same admin must reach the storages page normally.

## Out of scope

- A link to the admin panel on the empty state, or an "Admin" entry anywhere in
  the SPA. Both remain forbidden by `03`; this spec is how the same outcome is
  reached without them.
- Giving admins automatic access to storages (`03` keeps the two capabilities
  orthogonal).
- Changing the empty state's copy. Localization of it belongs to `19`.
