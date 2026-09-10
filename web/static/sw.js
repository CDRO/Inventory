// Hand-written service worker: caches the app shell cache-first, and never
// caches /api/* (docs/specs/05-frontend-pwa-foundations.md).
//
// There is no offline data editing (docs/specs/00-overview.md non-goals), so
// a cached API response would be actively misleading — a user could read
// last week's inventory count believing it current. The app shell (HTML,
// CSS, JS, icons) is a different matter: it changes only on deploy, and
// caching it is what makes the PWA installable and fast to open.

// Bump this on every release that changes a cached file. The old cache is
// deleted in `activate` below, so a stale version never lingers once a client
// picks up the new service worker.
//
// Nothing outside this file hardcodes the resulting name: e2e/specs/pwa.spec.js
// discovers it through caches.keys() precisely so that a bump here cannot leave
// a test opening a cache the service worker never uses — caches.open() creates
// a missing cache rather than failing, which would have made that test pass
// while checking nothing.
const CACHE_VERSION = "v3";
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

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);

  // Never intercept the API. Falling through here means the browser handles
  // the request exactly as if this service worker did not exist — no cached
  // response is ever substituted for a live one.
  if (url.pathname.startsWith("/api/")) {
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
