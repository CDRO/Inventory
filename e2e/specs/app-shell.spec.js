// Smoke coverage for the app shell of docs/specs/05-frontend-pwa-foundations.md.
// The required journeys live in their own files (auth-journeys.spec.js,
// non-disclosure.spec.js); the rest are tracked in issue #30.

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
