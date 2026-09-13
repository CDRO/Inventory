// storages.html without a session. The page calls GET /api/auth/me on load;
// a 401 from it must send the visitor to the login page rather than leaving
// them on "Loading…" or an empty shell (docs/specs/05-frontend-pwa-foundations.md).

import { test, expect } from "@playwright/test";

test("storages.html without a session redirects to the login page", async ({ page }) => {
  await page.goto("/storages.html");

  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server
  await expect(page.locator("#login-form")).toBeVisible();
});
