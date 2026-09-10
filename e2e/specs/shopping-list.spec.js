// Shopping list reconciliation (docs/specs/07-shopping-list-reconciliation.md),
// covering what is live without a session.
//
// The full journey — paste a list, resolve each line — needs a login, which
// does not exist until issue #27. What is checkable today is the part with the
// sharpest consequences if it were wrong: the routes are gated, and the image
// endpoints never hand a provider URL to the browser.

import { test, expect } from "@playwright/test";

const STORAGE_ID = "00000000-0000-4000-8000-000000000000";
const BASE = `/api/storages/${STORAGE_ID}`;
const HASH = "a".repeat(64);

test("shopping-list.html loads with no console errors of its own", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    if (msg.type() !== "error") return;
    // GET /api/auth/me does not exist until #27; that one failure is expected.
    // Matched by URL rather than message text because a missing JS module logs
    // the identical "Failed to load resource… 404" line.
    const url = msg.location()?.url ?? "";
    if (url.endsWith("/api/auth/me")) return;
    consoleErrors.push(`${msg.text()} (${url})`);
  });

  await page.goto("/shopping-list.html");
  await expect(page).toHaveTitle(/Shopping list/);
  await expect(page.locator("#error")).toBeVisible();

  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

test("every shopping list and image route is behind the session gate", async ({ request }) => {
  const routes = [
    ["post", `${BASE}/shopping-lists`],
    ["get", `${BASE}/shopping-lists/${STORAGE_ID}`],
    ["post", `${BASE}/shopping-lists/${STORAGE_ID}/items/${STORAGE_ID}/rematch`],
    ["post", `${BASE}/shopping-lists/${STORAGE_ID}/items/${STORAGE_ID}/resolve`],
    ["get", `${BASE}/image-suggestions?query=milk`],
    ["get", `${BASE}/images/${HASH}`],
  ];

  for (const [method, path] of routes) {
    const response = await request[method](path, { data: {} });

    expect(response.status(), `${method.toUpperCase()} ${path}`).toBe(401);
    const body = await response.json();
    expect(body.error.code).toBe("unauthorized");
    // APP_ENV=prod in this stack: the internal reason must not be disclosed.
    expect(body.error.debug_reason).toBeUndefined();
  }
});

test("the image endpoints are never reachable without a session, so no provider URL can leak", async ({
  request,
}) => {
  // The strongest statement this suite can make about the "browser never talks
  // to SerpAPI/Iconify" rule without a login: nothing an unauthenticated
  // client can reach returns an off-origin image URL.
  const response = await request.get(`${BASE}/image-suggestions?query=tomatoes`);
  expect(response.status()).toBe(401);

  const text = await response.text();
  expect(text).not.toContain("serpapi.com");
  expect(text).not.toContain("api.iconify.design");
  expect(text).not.toContain("googleusercontent");
});
