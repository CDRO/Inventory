// PWA coverage: the manifest and the service worker
// (docs/specs/05-frontend-pwa-foundations.md).

import { test, expect } from "@playwright/test";

test("manifest.json is valid and carries both required icon sizes", async ({ request }) => {
  const response = await request.get("/manifest.json");
  expect(response.status()).toBe(200);
  expect(response.headers()["content-type"]).toContain("application/json");

  const manifest = await response.json();
  expect(manifest.display).toBe("standalone");
  // /storages.html, not /index.html: the login form deliberately never
  // checks for a session, so an installed app starting there showed a
  // signed-in user the login screen on every launch. storages.html resolves
  // the storage and forwards; a signed-out visitor gets a 401 that js/api.js
  // redirects to the login form (docs/specs/34-navigation-and-start-page.md).
  expect(manifest.start_url).toBe("/storages.html");

  const sizes = manifest.icons.map((icon) => icon.sizes);
  expect(sizes).toContain("192x192");
  expect(sizes).toContain("512x512");

  // Every declared icon must actually be fetchable — a manifest pointing at
  // a 404 fails PWA installability silently, with no error a developer
  // would see without checking.
  for (const icon of manifest.icons) {
    const iconResponse = await request.get(icon.src);
    expect(iconResponse.status(), `${icon.src} status`).toBe(200);
  }
});

test("the service worker registers and takes control", async ({ page }) => {
  await page.goto("/index.html");

  const registered = await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    return Boolean(registration.active);
  });

  expect(registered).toBe(true);
});

// This is the invariant that matters most for a service worker in this
// system: there is no offline data editing
// (docs/specs/00-overview.md non-goals), so a cached API response would be
// actively misleading rather than merely stale.
test("the service worker never serves a cached response for /api/*", async ({ page }) => {
  await page.goto("/index.html");
  await page.evaluate(() => navigator.serviceWorker.ready);

  const seeded = await page.evaluate(async () => {
    // Seed a bogus entry in the exact cache sw.js's cacheFirst() opens.
    //
    // The name is discovered rather than hardcoded, and that matters more
    // than it looks. CACHE_NAME in sw.js is `inventory-shell-${CACHE_VERSION}`
    // and the version is bumped whenever the shell changes; a literal here
    // would go stale silently on the next bump, because caches.open() creates
    // a cache that does not exist instead of failing. The probe would then be
    // planted somewhere the service worker never reads, the fetch below would
    // reach the network for that reason rather than because /api/* is
    // excluded, and this test would keep passing while the invariant it
    // guards was broken.
    const names = await caches.keys();
    const shellCaches = names.filter((name) => name.startsWith("inventory-shell-"));
    if (shellCaches.length !== 1) {
      return { cacheName: null, count: shellCaches.length, body: null };
    }

    const cache = await caches.open(shellCaches[0]);
    await cache.put(
      "/api/__e2e_probe__",
      new Response(JSON.stringify({ planted: true }), {
        headers: { "Content-Type": "application/json" },
      }),
    );

    // Prove the seeding worked, so a silently-empty cache cannot masquerade
    // as a passing exclusion check.
    const planted = await cache.match("/api/__e2e_probe__");
    const response = await fetch("/api/__e2e_probe__");
    return {
      cacheName: shellCaches[0],
      count: shellCaches.length,
      plantedOK: planted != null,
      body: await response.text(),
    };
  });

  expect(
    seeded.cacheName,
    `expected exactly one inventory-shell-* cache, found ${seeded.count}`,
  ).not.toBeNull();
  expect(seeded.plantedOK, "the probe response must actually be in the cache").toBe(true);

  const liveBody = seeded.body;

  // The real backend has no route at this path, so the honest answer is
  // chi's own 404/405 JSON envelope — anything other than the planted
  // `{"planted":true}` body proves the request reached the network rather
  // than being served from the cache this test seeded.
  expect(liveBody).not.toContain("planted");
});

// /admin is the one route the server renders fresh per request rather than
// serving from web/static (docs/specs/03-auth-and-multi-tenancy.md), and it
// answers with Cache-Control: no-store for exactly that reason — but that
// header only governs the browser's own HTTP cache, not this service
// worker's Cache Storage, which cacheFirst() writes to directly regardless.
// Confirmed live: before sw.js excluded /admin, adding a user there and
// reopening the page kept showing the stale member list until a hard reload
// bypassed the service worker entirely.
test("the service worker never serves a cached response for /admin", async ({ page }) => {
  const loginRes = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin", password: "e2e-fixture-password" },
  });
  expect(loginRes.status(), "login as e2e-admin").toBe(200);

  await page.goto("/index.html");
  await page.evaluate(() => navigator.serviceWorker.ready);

  const seeded = await page.evaluate(async () => {
    // Same discovery-not-literal reasoning as the /api/* probe above: find
    // the shell cache by prefix so a future CACHE_VERSION bump cannot make
    // this test pass by planting into a cache the service worker never
    // reads.
    const names = await caches.keys();
    const shellCaches = names.filter((name) => name.startsWith("inventory-shell-"));
    if (shellCaches.length !== 1) {
      return { cacheName: null, count: shellCaches.length, body: null };
    }

    const cache = await caches.open(shellCaches[0]);
    await cache.put(
      "/admin",
      new Response("<html><body>planted-admin-page</body></html>", {
        headers: { "Content-Type": "text/html" },
      }),
    );

    const planted = await cache.match("/admin");
    const response = await fetch("/admin");
    return {
      cacheName: shellCaches[0],
      count: shellCaches.length,
      plantedOK: planted != null,
      body: await response.text(),
    };
  });

  expect(
    seeded.cacheName,
    `expected exactly one inventory-shell-* cache, found ${seeded.count}`,
  ).not.toBeNull();
  expect(seeded.plantedOK, "the probe response must actually be in the cache").toBe(true);

  // The real /admin route renders the live page for an authenticated admin —
  // anything other than the planted body proves the request reached the
  // network rather than being served from the cache this test seeded.
  expect(seeded.body).not.toContain("planted-admin-page");
});

// The /admin test above proves the exclusion sw.js already knows about
// works. It does not prove the thing that actually matters: that a route
// *nobody has thought to exclude yet* is still safe. /admin itself shipped
// without an exclusion once (PR #65) and served a stale response until a
// hard reload — the risk this test targets is the next one of those, not
// this one. docs/specs/05-frontend-pwa-foundations.md's allowlist model
// (CACHEABLE_EXACT / CACHEABLE_PREFIXES in sw.js) makes "not recognized as a
// known static shape" the safe default, so a path that is not named
// anywhere in the service worker — this one is made up and does not need to
// be a real route — must still never be served from a poisoned cache entry.
test("the service worker never serves a cached response for a path outside its allowlist", async ({ page }) => {
  await page.goto("/index.html");
  await page.evaluate(() => navigator.serviceWorker.ready);

  const seeded = await page.evaluate(async () => {
    const names = await caches.keys();
    const shellCaches = names.filter((name) => name.startsWith("inventory-shell-"));
    if (shellCaches.length !== 1) {
      return { cacheName: null, count: shellCaches.length, body: null };
    }

    const cache = await caches.open(shellCaches[0]);
    await cache.put(
      "/some-future-dynamic-route",
      new Response("planted-unlisted-route", { headers: { "Content-Type": "text/plain" } }),
    );

    const planted = await cache.match("/some-future-dynamic-route");
    const response = await fetch("/some-future-dynamic-route");
    return {
      cacheName: shellCaches[0],
      count: shellCaches.length,
      plantedOK: planted != null,
      body: await response.text(),
    };
  });

  expect(
    seeded.cacheName,
    `expected exactly one inventory-shell-* cache, found ${seeded.count}`,
  ).not.toBeNull();
  expect(seeded.plantedOK, "the probe response must actually be in the cache").toBe(true);

  // The real backend has no route here either, so the honest answer is chi's
  // own 404 JSON envelope. Anything other than the planted body proves the
  // request reached the network instead of the poisoned cache entry.
  expect(seeded.body).not.toBe("planted-unlisted-route");
});

// CACHE_VERSION bumps exist specifically so a browser that already has an
// old-named cache gets it cleaned up on the next activation, rather than
// carrying it forever. This proves that cleanup actually runs against a
// genuinely stale-named cache, not just that a fresh install ends up with
// one cache (which every other test here would already show incidentally).
test("service worker activation deletes old-versioned shell caches", async ({ page }) => {
  await page.goto("/index.html");
  await page.evaluate(() => navigator.serviceWorker.ready);

  await page.evaluate(async () => {
    const stale = await caches.open("inventory-shell-v0-simulated-stale");
    await stale.put(
      "/locations.html",
      new Response("<html>stale</html>", { headers: { "Content-Type": "text/html" } }),
    );
  });

  // Re-run the install/activate lifecycle a real deploy triggers, without
  // needing to actually publish two different sw.js versions: unregister
  // and let register-sw.js's own registration call on reload install fresh.
  await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    await reg.unregister();
  });
  await page.reload();
  await page.evaluate(() => navigator.serviceWorker.ready);

  const shellCaches = await page.evaluate(async () => {
    const names = await caches.keys();
    return names.filter((name) => name.startsWith("inventory-shell-"));
  });

  expect(
    shellCaches,
    `expected only the current-version cache to survive activation, found ${JSON.stringify(shellCaches)}`,
  ).toHaveLength(1);
  expect(shellCaches[0]).not.toBe("inventory-shell-v0-simulated-stale");
});

// The worker script's own update check is the one mechanism that can ever
// tell a browser a new version exists at all — if a browser or proxy caches
// *this* response, a CACHE_VERSION bump inside it never gets seen in the
// first place. Server-side coverage for the same behaviour already exists
// in internal/httpapi/health_test.go; this proves it end to end, against
// the real prod-target binary docker-compose.e2e.yml runs.
test("GET /sw.js is answered with Cache-Control: no-cache", async ({ request }) => {
  const response = await request.get("/sw.js");
  expect(response.status()).toBe(200);
  expect(response.headers()["cache-control"]).toBe("no-cache");
});

// docs/specs/05-frontend-pwa-foundations.md makes "Reset local app data"
// mandatory as the manual escape hatch for whatever the version-bump
// cleanup above does not anticipate. It is only worth having if it actually
// tears down what it claims to: a typo'd element id, a handler that
// unregisters the worker but forgets to clear Cache Storage (or the other
// way around), or a broken click binding would all ship green without a
// test that drives the real button and inspects real browser state after.
test("the 'Reset local app data' action unregisters the service worker and clears its caches", async ({ page }) => {
  const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-alice", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/settings.html?storage=${HOUSEHOLD}`);
  await page.evaluate(() => navigator.serviceWorker.ready);

  const before = await page.evaluate(async () => {
    const regs = await navigator.serviceWorker.getRegistrations();
    const names = await caches.keys();
    return { registrations: regs.length, caches: names.length };
  });
  expect(before.registrations, "expected a service worker registered before the reset").toBeGreaterThan(0);
  expect(before.caches, "expected at least the shell cache to exist before the reset").toBeGreaterThan(0);

  // resetLocalAppData reloads once it finishes, and register-sw.js would
  // immediately re-register a fresh worker on that very reload — which would
  // make "nothing is registered" impossible to observe and turn this into a
  // test of timing rather than of what the button actually did.
  // addInitScript runs in every document this page loads from here on,
  // including the reload, and — unlike page.route, which does not reliably
  // intercept a service worker's own registration fetch (confirmed live: the
  // route below never aborted it) — actually reaches
  // navigator.serviceWorker.register itself, so the post-reset state
  // reflects only the reset.
  await page.addInitScript(() => {
    navigator.serviceWorker.register = () => Promise.reject(new Error("registration blocked for test"));
  });

  await Promise.all([page.waitForEvent("load"), page.click("#reset-local-data")]);

  const after = await page.evaluate(async () => {
    const regs = await navigator.serviceWorker.getRegistrations();
    const names = await caches.keys();
    return { registrations: regs.length, caches: names.length };
  });
  expect(after.registrations, "the button must actually unregister the service worker").toBe(0);
  expect(after.caches, "the button must actually clear every Cache Storage entry").toBe(0);
});

// The navigation route of docs/specs/29-first-run-admin-guidance.md decides
// where a user with no storage belongs by reading the database on that
// request. A cached answer would therefore be worse than stale — it would keep
// sending someone to the admin area after their admin rights were revoked, or
// to the empty state after they were finally added to a storage, with no
// request reaching the server to notice either.
//
// sw.js does not name /no-storages anywhere, which under the allowlist model
// is what makes it safe; the test above proves that model holds for a made-up
// path, and this one proves it for the real route whose correctness depends
// on it.
test("the service worker never serves a cached response for /no-storages", async ({ page }) => {
  const loginRes = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin", password: "e2e-fixture-password" },
  });
  expect(loginRes.status(), "login as e2e-admin").toBe(200);

  await page.goto("/index.html");
  await page.evaluate(() => navigator.serviceWorker.ready);

  const seeded = await page.evaluate(async () => {
    const names = await caches.keys();
    const shellCaches = names.filter((name) => name.startsWith("inventory-shell-"));
    if (shellCaches.length !== 1) {
      return { cacheName: null, count: shellCaches.length };
    }

    const cache = await caches.open(shellCaches[0]);
    await cache.put(
      "/no-storages",
      new Response("<html><body>planted-no-storages</body></html>", {
        headers: { "Content-Type": "text/html" },
      }),
    );

    const planted = await cache.match("/no-storages");
    // Followed, not manual: what has to be proven is that the real redirect
    // ran, and `redirected` plus the final pathname says so in one go. A
    // planted entry would have resolved to a 200 that went nowhere.
    const response = await fetch("/no-storages");
    return {
      cacheName: shellCaches[0],
      count: shellCaches.length,
      plantedOK: planted != null,
      redirected: response.redirected,
      finalPath: new URL(response.url).pathname,
      body: await response.text(),
    };
  });

  expect(
    seeded.cacheName,
    `expected exactly one inventory-shell-* cache, found ${seeded.count}`,
  ).not.toBeNull();
  expect(seeded.plantedOK, "the probe response must actually be in the cache").toBe(true);

  expect(seeded.body).not.toContain("planted-no-storages");
  expect(seeded.redirected, "the request must have reached the server and been redirected").toBe(true);
  // e2e-admin is an admin with no storage, so the live answer is the admin
  // area — which also shows the redirect was computed rather than replayed.
  expect(seeded.finalPath).toBe("/admin");
});
