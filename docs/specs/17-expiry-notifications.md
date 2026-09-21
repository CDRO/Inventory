# 17 — Expiry Notifications (Opt-In, Self-Hosted Delivery)

Depends on: [`02-data-model.md`](02-data-model.md),
[`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md),
[`08-expiration-and-classification.md`](08-expiration-and-classification.md)
(urgency windows), [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md)
(timezone handling, LAN-only posture).

## Why this spec exists — and why it does not violate the non-goals

The expiry tracker is only as good as the odds someone looks at the
dashboard before the milk turns. A short daily digest — "2 items expired,
3 expire within 3 days" — delivered to a channel the household already
watches closes that gap.

Two existing rules sound like they forbid this, and deliberately do not:

- `06-vision-shelf-ingestion.md`: the inbox badge is "the only nudge —
  no notifications, no emails." That rule governs **review-inbox
  nagging** — pressure to do app chores. It stands, untouched.
- `50-gamification-overview.md`: "no push notifications." That rule
  governs the **gamification layer**. It stands, untouched.

This spec is neither: it is a **subscribed report about the food**, off
by default, sent only to an endpoint the household itself operates and
explicitly configured. Nothing here nags anyone to use the app; it tells
them their yogurt is about to go off, which is the product's actual job.
The boundary rule for all future work: notifications about *the
inventory's state* may be specified as opt-in features; notifications
about *the user's behavior* (streaks, unreviewed jobs, "we miss you")
remain forbidden.

**Delivery stays self-hosted.** The supported targets are
[ntfy](https://ntfy.sh) and [Gotify](https://gotify.net) — both commonly
run on the same NAS — plus a generic webhook. No vendor push service, no
email (the stack has no MTA and gains none), no accounts with third
parties. On a LAN/Tailscale-only deployment the notification never
leaves the private network.

## Configuration — per storage

Notifications are configured **per storage**, not per user: the digest
concerns shared food, and a household channel (one ntfy topic everyone
subscribes to) is the natural target. Rights are flat
(`03-auth-and-multi-tenancy.md`), so any member may configure it.

```sql
CREATE TABLE notification_settings (
    storage_id   UUID PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
    enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    kind         TEXT NOT NULL CHECK (kind IN ('ntfy', 'gotify', 'webhook')),
    url          TEXT NOT NULL,
    token        TEXT,                    -- optional auth token, kind-specific
    send_hour    INT NOT NULL DEFAULT 8 CHECK (send_hour BETWEEN 0 AND 23),
    include_soon BOOLEAN NOT NULL DEFAULT FALSE,
    last_run_at  TIMESTAMPTZ,
    last_result  TEXT,                    -- 'sent', 'empty', or an error summary
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- `GET` / `PUT /api/storages/{storage_id}/notification-settings` —
  members only, standard scoping. `PUT` validates `url` (http/https
  only, non-empty host) and returns the stored row; `token` is
  write-only — reads return whether one is set, never the value.
- `POST /api/storages/{storage_id}/notification-settings/test` — sends a
  test message now, returns the delivery outcome inline. This is how a
  typo in the URL is found at setup time instead of silently, days
  later.
- The settings UI lives on `settings.html`, visible only when the
  active storage is resolved.

## The digest

A scheduler in the app process ticks hourly. For every storage with
`enabled = TRUE`, `send_hour` equal to the current hour (server
timezone — the tzdata shipped per `01-architecture-and-deployment.md`),
and `last_run_at` not already in the current day, it builds the digest:

- **Expired** — batches with `expiration_date < today`.
- **Critical** — expiring within 3 days.
- **Soon** — within 14 days, only when `include_soon` is set.

The windows are exactly the urgency definitions from
`08-expiration-and-classification.md` — one classification, everywhere.

Each line: product name, quantity, location path, date. **Plain text,
short, no images ever** — a photo taken inside the home must not be
pushed to a relay, even a self-hosted one. If every bucket is empty,
**nothing is sent** (`last_result = 'empty'`): an "all fine" message
daily is how a channel gets muted.

Delivery per `kind`:

- `ntfy` — `POST {url}` with the digest as body, a title header, and
  `Authorization: Bearer {token}` when set. The `url` is the full topic
  URL.
- `gotify` — `POST {url}/message?token={token}` with
  `{"title": …, "message": …, "priority": 4}`.
- `webhook` — `POST {url}` with
  `{"title": …, "message": …, "items": [{name, quantity, location,
  expiration_date, urgency}]}` and `Authorization: Bearer {token}` when
  set, for anything the operator wants to script.

Failures are recorded in `last_result` and shown in the settings UI;
there is **no retry queue** — the next day's run is the retry, and the
test button is the debugging tool. A notification is a nicety and must
never grow durable-delivery machinery.

## The outbound request — trust posture

The URL is member-supplied and the request originates from the NAS.
Within one household of mutually trusting adults that is acceptable, but
the request must stay inert:

- Schemes restricted to `http`/`https`; redirects are **not** followed;
  timeout 10 seconds; response body discarded (only the status code is
  recorded).
- The request carries only the digest and the configured token — never
  cookies, sessions, API keys, or anything from `.env`.
- The digest contains data of this storage only, like every other
  surface (`03-auth-and-multi-tenancy.md`).

## Acceptance criteria

- With no `notification_settings` row, or `enabled = FALSE`, the
  scheduler does nothing for that storage and no code path can send.
- A storage is notified at most once per day, at its configured hour,
  and only when at least one bucket is non-empty.
- The digest's buckets match the dashboard's urgency classification for
  the same instant — no separate expiry arithmetic.
- Messages contain product names, quantities, locations, and dates of
  the configured storage only; never images, never another storage's
  data, never credentials.
- The test endpoint reports success/failure inline and writes
  `last_result`.
- A stored token is never returned by any read.
- Redirect responses from the target are treated as delivery failure,
  not followed.
- Disabling notifications, or deleting the storage, stops delivery
  immediately (row gone via `ON DELETE CASCADE`).
