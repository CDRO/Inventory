// Smoke coverage for the parts of docs/specs/05-frontend-pwa-foundations.md
// that are live today.
//
// This PR's own scope note (issue #10 on GitHub) explains why: the 8
// "required coverage" journeys in the spec's Testing section all depend on
// POST /api/auth/login, which does not exist until spec 03's HTTP surface
// (issue #27) lands — no fixture can route around an unregistered handler.
// What follows instead verifies the app shell that does exist, including a
// regression check for a routing bug this PR found and fixed along the way
// (see internal/httpapi/router.go and its test).

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
    "/js/register-sw.js",
    "/js/pages/index.js",
    "/js/pages/storages.js",
    "/js/pages/locations.js",
  ];

  for (const path of modules) {
    const response = await request.get(path);
    expect(response.status(), `${path} status`).toBe(200);
    expect(response.headers()["content-type"], `${path} content-type`).toContain("javascript");
  }
});

test("a non-GET request to an unregistered API path gets the JSON error envelope, not plain text", async ({
  request,
}) => {
  // The regression this PR fixed in internal/httpapi/router.go: the static
  // file mount used to claim every method, not just GET, so POST
  // /api/auth/login — the request the login form is about to start sending
  // the moment issue #27 lands — got http.FileServer's plain-text 404
  // instead of the one JSON error format the rest of the API promises
  // (docs/specs/04-backend-api-conventions.md).
  const response = await request.post("/api/auth/login", { data: {} });

  expect(response.status()).toBe(405);
  expect(response.headers()["content-type"]).toContain("application/json");

  const body = await response.json();
  expect(body.error.code).toBe("method_not_allowed");
});
