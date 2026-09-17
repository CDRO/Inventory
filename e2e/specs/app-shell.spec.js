// Smoke coverage for the app shell of docs/specs/05-frontend-pwa-foundations.md.
//
// The eight required journeys of that spec each live in their own files:
//
//   1  log in, land in a storage; log out ......... auth-journeys.spec.js
//   2  switch storages, see each one's own data ... storage-switching.spec.js
//   3  create location + category, add product ... create-and-list.spec.js
//   4  shelf photo → review → confirm ............. ingestion.spec.js
//   5  consumption with an overridden count ....... consumption.spec.js
//   6  reconcile a list across all match states ... shopping-list.spec.js
//   7  non-admin gets 404 for the admin area ...... non-disclosure.spec.js
//   8  storage B id gets the nonexistent-id 404 ... non-disclosure.spec.js
//
// Two are incomplete because the product behind them is: journey 3 has no
// category step, since no route or page can create a category (#72); and
// journey 6 stops at each line reaching `resolved`, since confirming a line
// does not yet write the batch or product spec 07 says it should (#74). Both
// are tracked on #30 until those land.

import { test, expect } from "@playwright/test";

test("the login page loads with no console errors", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });

  await page.goto("/index.html");

  // http.FileServer canonicalises /index.html to / (docs/specs/01…, and
  // pinned by internal/httpapi's own test suite) — a real browser follows
  // that redirect transparently, which is exactly what this asserts.
  await expect(page).toHaveURL(/\/$/);
  await expect(page).toHaveTitle("Inventory");

  await expect(page.locator("#username")).toBeVisible();
  await expect(page.locator("#password")).toBeVisible();
  await expect(page.locator('button[type="submit"]')).toBeVisible();

  // This is the check that would have caught the wrong relative import path
  // (`./register-sw.js` instead of `../register-sw.js`) found and fixed while
  // building this page: a module that fails to resolve throws in the
  // browser console, not in anything a Go test could see.
  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

test("every shared JS module is reachable and served as JavaScript", async ({ request }) => {
  const modules = [
    "/js/dom.js",
    "/js/api.js",
    "/js/session.js",
    "/js/storage-switcher.js",
    "/js/jobs.js",
    "/js/review.js",
    "/js/tree.js",
    "/js/inbox-badge.js",
    "/js/location-options.js",
    "/js/register-sw.js",
    "/js/pages/index.js",
    "/js/pages/storages.js",
    "/js/pages/locations.js",
    "/js/pages/shopping-list.js",
    "/js/pages/ingest.js",
    "/js/pages/inbox.js",
    "/js/pages/review.js",
  ];

  for (const path of modules) {
    const response = await request.get(path);
    expect(response.status(), `${path} status`).toBe(200);
    expect(response.headers()["content-type"], `${path} content-type`).toContain("javascript");
  }
});

test("an unregistered path gets the JSON error envelope, not plain text, for any method", async ({
  request,
}) => {
  // The static file mount once handed misses to http.FileServer, whose
  // plain-text "404 page not found" bypassed the one JSON error format the
  // API promises (docs/specs/04-backend-api-conventions.md). Every miss now
  // goes through the serializer — which also keeps the admin area
  // indistinguishable from a path that does not exist (see
  // non-disclosure.spec.js).
  for (const [method, path] of [
    ["post", "/api/not-a-route"],
    ["get", "/api/not-a-route"],
    ["get", "/no/such/asset.js"],
  ]) {
    const response = await request[method](path, { data: method === "get" ? undefined : {} });

    expect(response.status(), `${method} ${path}`).toBe(404);
    expect(response.headers()["content-type"]).toContain("application/json");
    const body = await response.json();
    expect(body.error.code).toBe("not_found");
  }
});
