// The category tree (docs/specs/02-data-model.md,
// docs/specs/08-expiration-and-classification.md), the counterpart of
// locations.spec.js.
//
// What this file verifies: the page loads cleanly for a real member, a visitor
// without a session is sent to the login page, and every category route is
// behind the gates. Creating, nesting and setting a shelf life on a category
// is required journey 3, and lives in e2e/specs/create-and-list.spec.js.

import { test, expect } from "@playwright/test";

// Any well-formed storage id will do for the gate checks: the session check
// runs before the membership check, so the request never gets far enough for
// the id to matter.
const STORAGE_ID = "00000000-0000-4000-8000-000000000000";
const BASE = `/api/storages/${STORAGE_ID}`;

test("categories.html loads a member's tree with no console errors", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    // No exemptions, for the reason locations.spec.js gives.
    if (msg.type() === "error") consoleErrors.push(`${msg.text()} (${msg.location()?.url ?? ""})`);
  });

  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto("/categories.html");
  await expect(page).toHaveTitle(/Categories/);

  // The seeded Canned Goods appearing means the load-time API calls have
  // settled, so a late error cannot slip in after the assertion.
  await expect(page.locator("#tree")).toContainText("Canned Goods");
  await expect(page.locator("#error")).toBeHidden();

  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

test("the storage landing page links to the category tree", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto("/storages.html");
  await page.getByRole("link", { name: "Categories" }).click();

  await expect(page).toHaveURL(/\/categories\.html\?storage=/);
  await expect(page.locator("#tree")).toContainText("Canned Goods");
});

test("categories.html without a session redirects to the login page", async ({ page }) => {
  await page.goto("/categories.html");

  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server
  await expect(page.locator("#login-form")).toBeVisible();
});

test("every category route is behind the session gate", async ({ request }) => {
  // The same claim locations.spec.js checks for its routes, against a real
  // deployment: a category route registered outside RequireSession →
  // RequireStorageMember would answer something other than 401 here.
  const routes = [
    ["get", `${BASE}/categories`],
    ["post", `${BASE}/categories`],
    ["patch", `${BASE}/categories/${STORAGE_ID}`],
    ["delete", `${BASE}/categories/${STORAGE_ID}`],
  ];

  for (const [method, path] of routes) {
    const response = await request[method](path, { data: {} });

    expect(response.status(), `${method.toUpperCase()} ${path} status`).toBe(401);
    const body = await response.json();
    expect(body.error.code, `${method.toUpperCase()} ${path} code`).toBe("unauthorized");
    // APP_ENV=prod in this stack, so the internal reason must not be here.
    expect(body.error.debug_reason, `${method.toUpperCase()} ${path} debug_reason`).toBeUndefined();
  }
});
