// storages.html without a live GET /api/auth/me (issue #27) — the same
// "degrades honestly" property as the login form, on the other page that
// calls the API on load.

import { test, expect } from "@playwright/test";

test("storages.html shows an error rather than hanging when /api/auth/me is unavailable", async ({
  page,
}) => {
  await page.goto("/storages.html");

  // GET /api/auth/me does not exist yet either (issue #27), so this is not
  // the 401-redirect path — it is a plain 404 from the static file server,
  // which api.js's ApiError fallback turns into a message rather than a
  // silent hang. Once #27 lands this becomes a real redirect to the login
  // page instead; either way, the page must never sit on "Loading…" forever.
  const main = page.locator("#main");
  await expect(main).not.toContainText("Loading…");
  await expect(main).toContainText(/could not load/i);
});
