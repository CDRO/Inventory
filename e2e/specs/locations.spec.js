// The location tree of docs/specs/06-vision-shelf-ingestion.md, covering what
// is genuinely live today.
//
// The full journey — log in, create a location, drag it onto another — is one
// of the deferred journeys tracked in issue #30: it needs a session, and
// POST /api/auth/login does not exist until issue #27 lands. What can be
// verified right now is stronger than a smoke test, though: the API routes are
// registered, they are behind the authorization gates, and the page fails
// honestly instead of hanging.

import { test, expect } from "@playwright/test";

// Any well-formed storage id will do: the session check runs before the
// membership check, so the request never gets far enough for the id to matter.
const STORAGE_ID = "00000000-0000-4000-8000-000000000000";
const BASE = `/api/storages/${STORAGE_ID}`;

test("locations.html loads with no console errors of its own", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    if (msg.type() !== "error") return;

    // This page, unlike the login page, calls the API on load — and
    // GET /api/auth/me does not exist until issue #27, so the browser logs a
    // failed-resource error every time. That one is expected; everything else
    // is not.
    //
    // The filter is by URL rather than by message, and that distinction is the
    // whole point: a JS module that 404s (the wrong relative import path this
    // suite caught once already) logs the *identical* "Failed to load
    // resource… 404" text. Matching on the message would mask exactly the bug
    // this check exists to find. Matching on the URL keeps it caught, and the
    // exemption disappears on its own once /api/auth/me is real.
    const url = msg.location()?.url ?? "";
    if (url.endsWith("/api/auth/me")) return;

    consoleErrors.push(`${msg.text()} (${url})`);
  });

  await page.goto("/locations.html");
  await expect(page).toHaveTitle(/Locations/);

  // Wait for the page's own load-time API call to have settled, so a late
  // error cannot slip in after the assertion.
  await expect(page.locator("#error")).toBeVisible();

  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

test("locations.html reports a problem rather than hanging when the session API is unavailable", async ({
  page,
}) => {
  await page.goto("/locations.html");

  // GET /api/auth/me does not exist yet (issue #27). The page must say so
  // rather than sitting on an empty tree that looks like an empty storage —
  // "you have no locations" and "we could not ask" are very different
  // statements to make to someone who just filled three shelves.
  const error = page.locator("#error");
  await expect(error).toBeVisible();
  await expect(error).not.toBeEmpty();
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
