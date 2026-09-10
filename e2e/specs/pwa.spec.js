// PWA coverage: the manifest and the service worker
// (docs/specs/05-frontend-pwa-foundations.md).

import { test, expect } from "@playwright/test";

test("manifest.json is valid and carries both required icon sizes", async ({ request }) => {
  const response = await request.get("/manifest.json");
  expect(response.status()).toBe(200);
  expect(response.headers()["content-type"]).toContain("application/json");

  const manifest = await response.json();
  expect(manifest.display).toBe("standalone");
  expect(manifest.start_url).toBe("/index.html");

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
