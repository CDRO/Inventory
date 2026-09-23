// Hand-written service worker: caches the app shell cache-first, from an
// allowlist of known static paths — /api/* and any other dynamically-rendered
// route are never cache candidates in the first place
// (docs/specs/05-frontend-pwa-foundations.md).
//
// There is no offline data editing (docs/specs/00-overview.md non-goals), so
// a cached API response would be actively misleading — a user could read
// last week's inventory count believing it current. The app shell (HTML,
// CSS, JS, icons) is a different matter: it changes only on deploy, and
// caching it is what makes the PWA installable and fast to open.

// Bump this on every release that changes a cached file, or that changes
// which requests get cached at all: existing clients may already hold a
// stale entry under the old name for something the new fetch handler would
// no longer read, and only a version bump clears it via the `activate`
// cleanup below rather than leaving it as dead weight indefinitely.
//
// Nothing outside this file hardcodes the resulting name: e2e/specs/pwa.spec.js
// discovers it through caches.keys() precisely so that a bump here cannot leave
// a test opening a cache the service worker never uses — caches.open() creates
// a missing cache rather than failing, which would have made that test pass
// while checking nothing.
const CACHE_VERSION = "v14";
const CACHE_NAME = `inventory-shell-${CACHE_VERSION}`;

// The app shell: everything a cold load needs before the network is asked
// for anything. Page-specific modules under js/pages/ are intentionally not
// pre-cached here — that list grows with every feature spec, and the
// fetch handler below caches them lazily (cache-first, falling back to
// network) the first time each is actually requested, which needs no
// maintenance as pages are added.
const SHELL_ASSETS = [
  // "/index.html" is deliberately absent: http.FileServer 301-redirects it to
  // "/" (same content, canonical path), and the Cache API's addAll() rejects
  // ANY redirected response outright — one bad entry fails the whole call, no
  // partial cache, no error surfaced anywhere but a `.ready` promise that
  // never resolves. Confirmed live: with "/index.html" in this list, `install`
  // silently failed and every E2E spec awaiting `serviceWorker.ready` timed
  // out at 30s with no console error to point at the cause.
  "/",
  "/storages.html",
  "/locations.html",
  "/categories.html",
  "/products.html",
  "/stocktake.html",
  "/shopping-list.html",
  "/ingest.html",
  "/inbox.html",
  "/review.html",
  "/manifest.json",
  "/css/tokens.css",
  "/css/base.css",
  "/css/components.css",
  "/js/dom.js",
  "/js/api.js",
  "/js/session.js",
  "/js/storage-switcher.js",
  "/js/jobs.js",
  "/js/review.js",
  "/js/tree.js",
  "/js/inbox-badge.js",
  "/js/location-options.js",
  "/js/tree-modal.js",
  "/js/audited.js",
  "/js/barcode.js",
  "/js/barcode-offer.js",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(SHELL_ASSETS)),
  );
  // Activate this version as soon as it finishes installing, rather than
  // waiting for every open tab to close — the app shell is small and a stale
  // tab still gets the fresh version on its next navigation.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((names) =>
      Promise.all(
        names
          .filter((name) => name.startsWith("inventory-shell-") && name !== CACHE_NAME)
          .map((name) => caches.delete(name)),
      ),
    ),
  );
  self.clients.claim();
});

// Paths this service worker will ever consider serving from cache —
// deliberately an allowlist, not a denylist
// (docs/specs/05-frontend-pwa-foundations.md).
//
// The first version of this file excluded known dynamic paths one at a time
// (/api/*, then the server-rendered admin page, added only after it shipped
// without the exclusion and kept serving a stale member list until a hard
// reload — PR #65). That
// shape needs a human to remember every future server-rendered route before
// it ships, and forgetting is silent: the route works the first time,
// nothing is cached yet, and it only starts serving a stale response on
// every request after that — indistinguishable from a working app until
// someone notices. An allowlist inverts the risk: a route nobody has
// thought about yet — /api/*, the admin page, the navigation route of
// docs/specs/29-first-run-admin-guidance.md, or whatever ships next — is
// safe by default, and caching it is the thing someone opts into
// deliberately by adding it here.
//
// "/index.html" is deliberately absent from CACHEABLE_EXACT, matching
// SHELL_ASSETS above: it 301-redirects to "/", and a request for it should
// simply fall through to the network (a cheap redirect) rather than risk
// caching a redirected response under the wrong key.
const CACHEABLE_EXACT = new Set([
  "/",
  "/manifest.json",
  "/storages.html",
  "/locations.html",
  "/categories.html",
  "/products.html",
  "/shopping-list.html",
  "/ingest.html",
  "/inbox.html",
  "/review.html",
  "/dashboard.html",
  "/settings.html",
  "/consume-review.html",
  "/stocktake.html",
]);
const CACHEABLE_PREFIXES = ["/css/", "/js/", "/icons/", "/vendor/"];

function isCacheable(pathname) {
  if (CACHEABLE_EXACT.has(pathname)) return true;
  return CACHEABLE_PREFIXES.some((prefix) => pathname.startsWith(prefix));
}

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);

  // Only same-origin GET requests are cache candidates at all; anything else
  // (cross-origin, non-GET) goes straight to the network untouched.
  if (event.request.method !== "GET" || url.origin !== location.origin) {
    return;
  }

  // Everything not recognized as a known static shape — /api/*, the admin
  // page, the navigation route that decides where a user with no storage
  // goes, and any route that does not exist yet — falls through here. A
  // navigation route in particular must never be answered from cache: it
  // answers a redirect computed from the database on that request, and a
  // remembered one would keep sending a user where they belonged before
  // their memberships changed. Falling through
  // means the browser handles the request exactly as if this service worker
  // did not exist: no cached response is ever substituted for a live one.
  if (!isCacheable(url.pathname)) {
    return;
  }

  event.respondWith(cacheFirst(event.request));
});

async function cacheFirst(request) {
  const cache = await caches.open(CACHE_NAME);
  const cached = await cache.match(request);
  if (cached) return cached;

  const response = await fetch(request);
  // Only a genuinely complete, successful response is cached — an opaque
  // cross-origin response or a mid-stream failure must not be stored as if
  // it were the real asset.
  if (response.ok) {
    cache.put(request, response.clone());
  }
  return response;
}
