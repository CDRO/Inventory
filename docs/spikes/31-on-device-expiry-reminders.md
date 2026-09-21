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
is closed. Without it, **the PWA has no way to run while nobody has it open**, except
a browser-scheduled background task, and that exists in one engine only:

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
days") and sets the app badge (`navigator.setAppBadge`, also available in workers;
the Badging API is "not Baseline", so support per browser and per installed-app mode
must be checked, see the gate). Works wherever the PWA runs; needs no service-worker
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
  `clients.claim`, a `wrapper-cache-<version>` cache) and a `push` handler. It has no
  `periodicsync` handler and no badge or local-notification code.
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

## What it would cost / conflicts with accepted specs

- **A read endpoint for the buckets.** Spec 17 builds the digest server-side; the
  banner needs the same buckets (`08-expiration-and-classification.md`'s windows:
  expired, critical under 3 days, soon under 14) through an API route. Something like
  `GET /api/storages/{storage_id}/expiry-summary`, shared by the digest builder, the
  dashboard and this. Not specified here.
- **`web/static/sw.js` grows a handler** and its shell-cache rules
  (`05-frontend-pwa-foundations.md`) apply: a changed worker needs a `CACHE_VERSION`
  bump.
- **Per user and per device, not per storage.** Spec 17's settings are per storage.
  An on-device reminder is the user's own setting and must default to off.
- **No conflict with the no-nudge rules**, by spec 17's own boundary: notifications
  about the inventory's state may be opt-in features, notifications about the user's
  behavior stay forbidden. Only expiring food is in scope.

## Gate conditions — all must hold before this is accepted

- [ ] **Measured on the owner's own devices, not read from documentation.** On the
      Android phone with the installed PWA: does `periodicsync` fire, how often over a
      week of normal use, and can the handler show a notification with Notification
      permission granted? On the iPhone: does the installed PWA do anything in the
      background (expected: nothing), and does the Badging API work there?
- [ ] The owner decides that "reminders while the app is used" (B1, or B2 on
      Chromium) is worth having next to spec 17, given that it cannot reach a closed
      app on iOS at all and only as often as the app is used on Android.
- [ ] Spec 17 is built, and its bucket builder is reusable behind a read endpoint.
- [ ] The wrapper decision: adopt the browser half (vendored, worker merged into
      `sw.js`), extend it upstream, or leave it out.
- [ ] The owner decides what the iOS consequence of "no vendor push" means for spec 17
      itself: accept the ntfy polling delay, use ntfy's upstream relay (which is a vendor
      push service), or make iOS in-app only.

## Author's recommendation (not a decision)

Do not promote this as a replacement for spec 17. Without a push service the PWA
cannot wake a closed app on iOS at all, and on Android only as often as the app is
opened, which is the moment a reminder is least needed. If the owner wants B1 anyway,
promote **only that**: the on-open banner and the badge, a summary endpoint, no
background work and no new tables. Otherwise reject the spike and record the iOS
consequence in spec 17.

## Not verified

Whether a `periodicsync` handler may show a notification, and under which
permission; how the Badging API behaves per browser and for an installed iOS web app;
that every browser's Web Push goes through its vendor's service (stated as known, not
re-checked); that UnifiedPush cannot be used from a PWA; how often Chrome really fires
`periodicsync` for a household app. All of these are answered by the first gate
condition or are out of scope for a candidate.
