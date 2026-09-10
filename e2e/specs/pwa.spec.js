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

  const liveBody = await page.evaluate(async () => {
    // Seed a bogus cache entry under the exact cache name sw.js's
    // cacheFirst() would open regardless of path — CACHE_NAME there is
    // `inventory-shell-${CACHE_VERSION}`, so this literal must be kept in
    // step with sw.js's CACHE_VERSION. If the service worker's fetch handler
    // ever stopped excluding /api/* before calling cacheFirst(), this planted
    // response is exactly what a request for the same URL would come back
    // with instead of reaching the network.
    const cache = await caches.open("inventory-shell-v1");
    await cache.put(
      "/api/__e2e_probe__",
      new Response(JSON.stringify({ planted: true }), {
        headers: { "Content-Type": "application/json" },
      }),
    );

    const response = await fetch("/api/__e2e_probe__");
    return response.text();
  });

  // The real backend has no route at this path, so the honest answer is
  // chi's own 404/405 JSON envelope — anything other than the planted
  // `{"planted":true}` body proves the request reached the network rather
  // than being served from the cache this test seeded.
  expect(liveBody).not.toContain("planted");
});
