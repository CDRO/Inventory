# 31 — Expiry reminders from the installed PWA, without a push service

**Status:** Under evaluation · **Raised:** 2026-09-21
**Source:** the owner asked whether the installed PWA could deliver expiry
notifications, and offered `CDRO/pwa-wrapper` for it. Two owner decisions of the
same day frame this spike and are not open questions: **only expiring food** (no
other events, in particular no job or review status), and **no vendor push** (no
Google, Apple or Mozilla push service).

## The question

[`17-expiry-notifications.md`](../specs/17-expiry-notifications.md) sends an
expiry digest to an ntfy topic, a Gotify server or a webhook. That needs a second
app on the phone. Could the app people already have installed — the PWA — tell them
that something is about to go off, and could the wrapper help with that?

## What the two decisions rule out

Every browser delivers Web Push through its vendor's push service (Chrome through
Google, Firefox through Mozilla, Safari through Apple; known, not re-verified for
this spike). Web Push is also the mechanism that wakes a service worker when the app
is closed. Without it, the PWA is understood to have no way to run while nobody has
it open — this follows from the above but is not itself independently verified for
this spike, see "Not verified" — except a browser-scheduled background task, and
that exists in one engine only:

- **Periodic Background Sync** is Chromium-only (Chrome, Edge, Opera, Samsung
  Internet); Firefox and Safari, including iOS, do not have it. In Chrome it works
  only for an *installed* web app, "a `periodicsync` event won't be fired at all
  unless the engagement score is greater than zero, and its value affects the
  frequency", and "the timing of synchronizations are not controlled by developers.
  The synchronization frequency will align with how often the app is used"
  ([Chrome for Developers](https://developer.chrome.com/docs/capabilities/periodic-background-sync),
  [MDN](https://developer.mozilla.org/en-US/docs/Web/API/Web_Periodic_Background_Synchronization_API)).
  Neither source says whether the `periodicsync` handler may show a notification, or
  under which permission. That has to be tested, not assumed.

So a PWA without a push service can, at best, remind someone **while they use the
app**, plus, on Chromium, a little more often the more they use it. It cannot
promise "you will be told when you have not opened the app", which is what a
digest is for.

## Options inside the constraints

**A. Spec 17 as it is (ntfy, Gotify, webhook).** Server-driven; works with the app
closed. What the "no vendor push" decision does to it, from the ntfy documentation:
on Android a self-hosted ntfy server delivers instantly over a direct connection
("FCM is not involved"); on iOS "iOS heavily restricts background processing, which
sadly makes it impossible to implement instant push notifications without a central
server", so a self-hosted server must set `upstream-base-url` to ntfy.sh, which relays
through Apple's push service; without it messages may take hours, or 20 to 30 minutes
while the phone is in active use
([ntfy docs](https://docs.ntfy.sh/config/#ios-instant-notifications)). Spec 17 says
delivery "stays self-hosted" but does not say this. **Under the owner's rule, spec 17
reaches Android instantly and iOS with a delay.** That is a finding of this spike
about an accepted spec, and it stands whichever option below is chosen.

**B1. On-open reminder.** When the app is opened, or comes back to the foreground,
it asks the API for the expiry buckets and shows a banner ("2 expired, 3 within 3
days") and sets the app badge (`navigator.setAppBadge`, said to also be available in
workers, not independently verified — see "Not verified"; the Badging API is "not
Baseline", so support per browser and per installed-app mode must be checked, see
the gate). Works wherever the PWA runs; needs no service-worker
background work. It tells people what they could have read on the dashboard, one tap
earlier.

**B2. B1 plus Periodic Background Sync on Chromium.** A `periodicsync` handler in the
service worker fetches the same buckets and, if it may, shows a local notification.
Best effort, Chromium only, timed by the browser from how often the app is used.

**C. UnifiedPush** (Android, with a self-hosted distributor such as ntfy). The
distributor is an app, and the receiver would be a native app, not the PWA. That is
[`21-native-android-app.md`](21-native-android-app.md), not this spike.

**D. Web Push.** Rejected by the owner.

## What `pwa-wrapper` contributes

Read from the repository on 2026-09-21 (`README.md`, `docs/PUBLIC_API.md`,
`docs/SUPPORT_MATRIX.md`, `src/`, `server/`):

- It is a **push transport**: capability detection (`getCapabilities`, with a
  `fallback: "in_app_only"` state for browsers that cannot do background delivery),
  `createManifestTemplate`, `promptInstall`, `registerServiceWorker`,
  `subscribeToPush`, `unsubscribeFromPush`. Its public contract lists `initialize()`
  and more as planned, not exported.
- Its service worker owns the `install`/`activate` lifecycle (`skipWaiting`,
  `clients.claim`, a `wrapper-cache-<version>` cache), a `push` handler that calls
  `registration.showNotification(...)`, and a `notificationclick` handler. It has
  notification display for push; it has no `periodicsync` handler and no badge code.
- Its `server/` package is **Node** (an in-memory subscription store and `publish`).
  This project's backend is Go and its host has no Node
  ([`01-architecture-and-deployment.md`](../specs/01-architecture-and-deployment.md)).
  It cannot be used here, and with no push there would be nothing for it to send.
- The browser files `dist/pwa-wrapper.js` and `dist/pwa-wrapper-sw.js` are single
  files without dependencies, so they could be vendored as this project requires
  (`CLAUDE.md`: libraries only as single vendored files). But a scope has one service
  worker: its worker would have to be merged into `web/static/sw.js`, not registered
  beside it.

With push out, the part of the wrapper that is worth having is the capability
detection and the install affordance. The part that *is* the wrapper, the push
transport, would sit unused. Extending the wrapper upstream with a `periodicsync` and
badge helper is a possibility, and it would be the owner's repository to change.

## Why the on-device shape is attractive

- **No new server-side surface.** Web Push would need a subscription table, VAPID
  keys, an outbound sender and a send-time membership re-check for every recipient
  device (a removed member's phone must stop receiving). B1 and B2 need none of it:
  the device asks the API with its own session like any page does, so what it can be
  told is exactly what it can read (`03-auth-and-multi-tenancy.md`), and removing a
  member removes the data source at the next fetch.
- **Nothing leaves the household**, which is spec 17's stated posture.
- It is small: one read endpoint and client code.

## Open security and privacy gaps

The argument above covers the fetch only; a promotion would have to close these
gaps:

- **Stale state after access changes.** After a member is removed, on logout, or
  on session revocation, the app badge, an already shown notification and any
  `periodicsync` registration all persist on the device — nothing today would
  clear them. A promoted design must clear all three.
- **An expired session during a background fetch** is unspecified: what the
  worker does when its background fetch's session has expired (silently doing
  nothing is assumed here, not stated) needs a decision.
- **The notification body is unspecified.** Spec 17's digest lines carry product
  name, quantity and location path (`17-expiry-notifications.md`), and those would
  show on a lock screen. A promoted design should pin the on-device notification
  body to counts only, not digest lines.
- **Where the per-user, per-device setting lives is unspecified.** "One read
  endpoint and client code, no new tables" (below) conflicts with needing a
  per-user, per-device setting somewhere, and a service worker cannot read the
  `localStorage` that holds the active storage (`web/static/js/session.js`).
  Undecided: which storage(s) the worker queries, and whether the badge is per
  storage or summed across storages.
- **Spec 06's own no-nudge rule.** Spec 06 says the inbox's count badge next to
  the storage switcher "is the only nudge — no notifications, no emails"
  (`06-vision-shelf-ingestion.md`). An OS-level icon badge from this spike would
  be a second count badge; that tension is not addressed by the no-nudge bullet
  below, which only checks against spec 17's boundary.

## What it would cost / conflicts with accepted specs

- **A read endpoint for the buckets.** Spec 17 builds the digest server-side; the
  banner needs the same buckets (`08-expiration-and-classification.md`'s windows:
  expired, critical under 3 days, soon under 14) through an API route. Something like
  `GET /api/storages/{storage_id}/expiry-summary`, shared by the digest builder and
  this. **Conflict:** spec 08 defines urgency sorting/filtering as computed
  client-side from `expiration_date`, not through a server-side bucket endpoint, and
  the dashboard does not consume one today — "shared by ... the dashboard" above
  does not hold as stated. Not specified here.
- **`web/static/sw.js` grows a handler.** Spec 05 requires "a versioned cache name
  bumped on release" (`05-frontend-pwa-foundations.md`); the `CACHE_VERSION`
  constant itself lives in `web/static/sw.js`, not in the spec. The bump this work
  would need is for the newly cached JS added to the shell, not because the worker
  gained a handler.
- **Per user and per device, not per storage.** Spec 17's settings are per storage.
  An on-device reminder is the user's own setting and must default to off.
- **Only a partial answer to the no-nudge rules.** Spec 17's own boundary allows
  it: notifications about the inventory's state may be opt-in features,
  notifications about the user's behavior stay forbidden, and only expiring food is
  in scope. Spec 06's separate rule is not addressed by that check — see "Open
  security and privacy gaps" above.

## Gate conditions — all must hold before this is accepted

- [ ] **Measured on the owner's own devices, not read from documentation, against a
      named threshold.** Devices and versions to record before testing: the
      Android phone's exact Android and Chrome version, and the iPhone's exact iOS
      and Safari version. **Pass:** on the Android device, `periodicsync` fires on
      at least 5 of 7 days of the owner's normal use, and the handler can show a
      notification with Notification permission granted. **Fail:** fewer than 5 of
      7 days, or the handler cannot show a notification. On the iPhone: record
      whether the installed PWA does anything in the background (expected:
      nothing) and whether the Badging API works there; these are recorded, not
      pass/fail, since B1 does not depend on iOS background behaviour.
- [ ] The owner decides that "reminders while the app is used" (B1, or B2 on
      Chromium) is worth having next to spec 17, given that it cannot reach a closed
      app on iOS at all and only as often as the app is used on Android.
- [ ] Spec 17 is built, and its bucket builder is reusable behind a read endpoint.
- [ ] The wrapper decision: adopt the browser half (vendored, worker merged into
      `sw.js`), extend it upstream, or leave it out.
- [ ] The ntfy iOS delay finding above is confirmed against a primary source (not
      the ntfy documentation alone), since it is this spike's headline finding
      about an accepted spec and the reviewers had no web access to re-check it.

## Consequence for spec 17 (not a gate for this spike)

The owner still needs to decide what the iOS consequence of "no vendor push" means
for spec 17 itself: accept the ntfy polling delay, use ntfy's upstream relay (which
is a vendor push service), or make iOS in-app only. That decision belongs to spec 17
and does not gate acceptance of this spike either way.

## Author's recommendation (not a decision)

Do not promote this as a replacement for spec 17. Without a push service the PWA
cannot wake a closed app on iOS at all, and on Android only as often as the app is
opened, which is the moment a reminder is least needed. If the owner wants B1 anyway,
promote **only that**: the on-open banner and the badge, a summary endpoint, no
background work and no new tables. Otherwise reject the spike and record the iOS
consequence in spec 17.

## Not verified

Whether a `periodicsync` handler may show a notification, and under which
permission; how the Badging API behaves per browser and for an installed iOS web
app, and whether it is available inside a service worker at all; that every
browser's Web Push goes through its vendor's service (stated as known, not
re-checked); that UnifiedPush cannot be used from a PWA; how often Chrome really
fires `periodicsync` for a household app; that a PWA genuinely has no way to run
while nobody has it open, beyond Periodic Background Sync (see "What the two
decisions rule out"); the exact list of Chromium engines with Periodic Background
Sync — MDN confirms Chrome and Edge, but Opera and Samsung Internet came from a
search summary and were not checked against a primary source; the two Chrome for
Developers quotes in "What the two decisions rule out", whose page could not be
re-fetched by the reviewers; and the ntfy iOS delay finding about spec 17 below,
checked only against the ntfy documentation. Only the `periodicsync`/notification
and Badging-API behaviour is answered by the first gate condition; the ntfy iOS
finding has its own gate; the rest are out of scope for a candidate.
