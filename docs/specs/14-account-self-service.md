# 14 — Account Self-Service & Credential Lifecycle

Depends on: [`02-data-model.md`](02-data-model.md) (`users`, `sessions`),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md).

## Why this spec exists

`03-auth-and-multi-tenancy.md` gives the admin create and delete for
users — and nothing else. There is no way for anyone, admin included, to
change a password after account creation. In practice that means the
password the admin typed at creation is the password forever, it has
probably been said out loud across a kitchen, and a forgotten password
can only be fixed by deleting the account (losing nothing but dignity,
since data is storage-scoped — but still absurd). This spec closes the
credential lifecycle: self-service password change, admin reset, display
name edit, and the rate limiting that `03` promises for pairing but never
specifies anywhere.

## Changing your own password

`POST /api/auth/password` — session required, body
`{current_password, new_password}`:

- `current_password` is verified against `users.password_hash`
  (argon2id). A wrong value is `422 validation_failed` with
  `fields: {current_password: [...]}` — not a `401`, because the caller
  *is* authenticated; their input is what failed. The check exists so a
  borrowed unlocked phone cannot silently take over the account.
- `new_password` must be at least **10 characters**. No composition
  rules (mandatory digits/symbols breed `Password1!`), no maximum below
  128. `422` otherwise.
- On success, the hash is replaced and **every other session of this
  user is deleted** in the same transaction — browser and `device` kind
  alike — keeping only the session that made the request. Changing a
  password is most often done *because* its secrecy is in doubt; leaving
  old sessions alive would change the lock while leaving the windows
  open. Paired devices re-pair via QR (`12-client-api-contract.md`),
  which is cheap by design.
- This endpoint is rate-limited (below): it accepts password guesses
  from a stolen session, so it must not accept them quickly.

## Admin password reset

`POST /api/admin/users/{id}/password` — body `{new_password}`, behind
the `RequireSession` → `RequireAdmin` chain like every admin route
(`04-backend-api-conventions.md`). This is the "I forgot my password"
remedy in a system with no email and therefore no reset-link flow — the
admin is a person in the same household, and handing over a new
temporary password in the kitchen is the intended UX.

- Same `new_password` rule as above.
- **All** of the target user's sessions are deleted in the same
  transaction — the resetter cannot know which sessions are legitimate,
  so none survive.
- The target is expected to change the temporary password themselves via
  the self-service route. There is deliberately no `must_change_password`
  flag and no forced-change interstitial: at household scale the social
  contract ("change it after you log in") is sufficient, and the flag
  would add a second login state machine to every client for a case that
  occurs a few times a year.
- The admin UI (`03-auth-and-multi-tenancy.md`) gets a "Reset password"
  action on the user list, added to the route table there.

## Display name

`PATCH /api/auth/me` — body `{display_name}` (the only accepted field;
anything else is `422`). Usernames are immutable — they are the login
identifier, referenced in nothing user-facing, and renaming them buys a
migration headache for zero benefit.

## Rate limiting credential endpoints

`03-auth-and-multi-tenancy.md` requires `POST /api/auth/pair` to be
"rate-limited per IP and per user" without defining what that means, and
`POST /api/auth/login` — the other endpoint worth guessing at — has no
limit at all. argon2id makes each guess expensive for us too, which is
its own reason to cut guessing off early.

One limiter, in `internal/httpapi`, applied to **`/api/auth/login`,
`/api/auth/pair`, and `/api/auth/password`**:

- Fixed window: at most **10 failed attempts per 15 minutes**, counted
  separately per client IP and — for login — per submitted username
  (whether or not it exists, so the limiter itself cannot be used to
  probe which usernames are real). A success resets the counters it
  matched.
- Over the limit → `429 rate_limited` with a `Retry-After` header
  (seconds until the window frees). The error envelope and code follow
  `04-backend-api-conventions.md`, whose status table gains this row.
- The counters live **in process memory**. A restart clears them — at
  household scale that is an acceptable trade against a table and a
  sweep for what is fundamentally abuse damping, and a single-process
  deployment (`01-architecture-and-deployment.md`) has no coordination
  problem. This is a deliberate decision, not an oversight.
- The client IP is taken from the connection, or from the
  Traefik-supplied `X-Forwarded-For` only when the connection peer is
  the internal network — never from the header alone, which any client
  can forge.

Failed logins remain `401 unauthorized` with an identical body whether
the username exists or not (`03-auth-and-multi-tenancy.md` non-enumeration
spirit applies to users too).

## Frontend

`settings.html` (which already exists for the local-data reset and
gamification toggle) gains an **Account** section: display name field,
change-password form (current, new, repeat — repeat is client-side
courtesy only), and the existing device list from
`GET /api/auth/devices`. No admin affordances appear here, ever
(`03-auth-and-multi-tenancy.md`).

## Cross-spec edits this spec requires

- `03-auth-and-multi-tenancy.md`: admin route table gains the password
  reset row.
- `04-backend-api-conventions.md`: status table gains
  `429 rate_limited`.

## Acceptance criteria

- Changing one's own password with the correct current password succeeds
  and immediately invalidates every other session of that user,
  including paired devices, while the current session keeps working.
- A wrong `current_password` is `422` with a field error, and repeated
  wrong attempts hit the `429` limit.
- An admin reset invalidates **all** of the target's sessions and never
  returns the new password in any response beyond echoing success.
- The 11th failed login within 15 minutes from one IP (or against one
  username) returns `429` with `Retry-After`; a success before the limit
  resets the count.
- Login responses are byte-identical for "no such user" and "wrong
  password".
- `PATCH /api/auth/me` changes the display name and nothing else;
  unknown fields are rejected, not ignored.
- No route in this spec ever returns `is_admin`, a password, or a hash.
