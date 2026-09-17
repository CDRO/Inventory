// The location tree of docs/specs/06-vision-shelf-ingestion.md.
//
// What this file verifies: the page loads cleanly for a real member, a visitor
// without a session is sent to the login page, and every API route is behind
// the gates. Creating and nesting a location is required journey 3, and lives
// in e2e/specs/create-and-list.spec.js. Re-parenting — both the drag-and-drop
// path and its "Move to…" keyboard equivalent — is still uncovered end to
// end; it is a spec 06 frontend requirement rather than one of spec 05's
// eight gate journeys, so it is not in this round's scope.

import { test, expect } from "@playwright/test";

// Any well-formed storage id will do for the gate checks: the session check
// runs before the membership check, so the request never gets far enough for
// the id to matter.
const STORAGE_ID = "00000000-0000-4000-8000-000000000000";
const BASE = `/api/storages/${STORAGE_ID}`;

test("locations.html loads a member's tree with no console errors", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    // No exemptions: with a real session every load-time call succeeds, so any
    // error at all — including a JS module that 404s — is a finding.
    if (msg.type() === "error") consoleErrors.push(`${msg.text()} (${msg.location()?.url ?? ""})`);
  });

  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto("/locations.html");
  await expect(page).toHaveTitle(/Locations/);

  // The seeded Pantry appearing means the load-time API calls have settled, so
  // a late error cannot slip in after the assertion.
  await expect(page.locator("#tree")).toContainText("Pantry");
  await expect(page.locator("#error")).toBeHidden();

  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

test("locations.html without a session redirects to the login page", async ({ page }) => {
  await page.goto("/locations.html");

  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server
  await expect(page.locator("#login-form")).toBeVisible();
});

test("every location route is behind the session gate", async ({ request }) => {
  // The whole storage-scoped API is registered on one sub-router carrying
  // RequireSession → RequireStorageMember (internal/httpapi/router.go). This
  // is that claim checked against a real deployment rather than a Go test's
  // in-process router: if any of these were registered outside the chain, it
  // would answer something other than 401 to a caller with no cookie.
  const routes = [
    ["get", `${BASE}/locations`],
    ["post", `${BASE}/locations`],
    ["patch", `${BASE}/locations/${STORAGE_ID}`],
    ["delete", `${BASE}/locations/${STORAGE_ID}`],
    ["patch", `${BASE}/inventory-batches/${STORAGE_ID}`],
    ["post", `${BASE}/inventory-batches/${STORAGE_ID}/split`],
  ];

  for (const [method, path] of routes) {
    const response = await request[method](path, { data: {} });

    expect(response.status(), `${method.toUpperCase()} ${path} status`).toBe(401);
    expect(response.headers()["content-type"], `${method.toUpperCase()} ${path} type`).toContain(
      "application/json",
    );

    const body = await response.json();
    expect(body.error.code, `${method.toUpperCase()} ${path} code`).toBe("unauthorized");
    // APP_ENV=prod in this stack, so the internal reason must not be here.
    expect(body.error.debug_reason, `${method.toUpperCase()} ${path} debug_reason`).toBeUndefined();
  }
});

test("the expiry routes are behind the session gate", async ({ request }) => {
  // Spec 08's writes are the ones that can silently rewrite a date somebody
  // typed, so they get the same gate check as every other storage-scoped
  // route (docs/specs/08-expiration-and-classification.md).
  const routes = [
    `${BASE}/inventory-batches/${STORAGE_ID}/expiry`,
    `${BASE}/categories/${STORAGE_ID}/shelf-life`,
  ];

  for (const path of routes) {
    const response = await request.patch(path, { data: {} });

    expect(response.status(), `PATCH ${path}`).toBe(401);
    const body = await response.json();
    expect(body.error.code).toBe("unauthorized");
    expect(body.error.debug_reason).toBeUndefined();
  }
});
