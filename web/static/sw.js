// Hand-written service worker: caches the app shell cache-first, and never
// caches /api/* or any other dynamically-rendered route
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
const CACHE_VERSION = "v5";
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

// Paths that are never cache candidates because the server renders them
// fresh per request, unlike everything under web/static: today that is only
// /admin (docs/specs/03-auth-and-multi-tenancy.md), which the backend
// already answers with Cache-Control: no-store — but that header only
// governs the browser's HTTP cache, not this service worker's own Cache
// Storage, which cacheFirst() writes to directly regardless of any
// Cache-Control header on the response. A future server-rendered route needs
// adding here too, or it will silently get the same stale-until-hard-reload
// treatment /admin did before this list existed.
const NEVER_CACHE = ["/admin"];

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);

  // Never intercept the API, or any other dynamically-rendered route. Falling
  // through here means the browser handles the request exactly as if this
  // service worker did not exist — no cached response is ever substituted
  // for a live one.
  if (url.pathname.startsWith("/api/") || NEVER_CACHE.includes(url.pathname)) {
    return;
  }

  // Only same-origin GET requests are cache candidates; anything else
  // (cross-origin, non-GET) goes straight to the network untouched.
  if (event.request.method !== "GET" || url.origin !== location.origin) {
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
