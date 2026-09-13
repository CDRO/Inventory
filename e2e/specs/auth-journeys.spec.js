// Required journeys 1 and 2 of docs/specs/05-frontend-pwa-foundations.md:
// log in and land on a resolved storage; log out and land back on the login
// page. Fixture users and storages come from e2e/fixtures/seed.sql.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";

async function logIn(page, username) {
  await page.goto("/index.html");
  await page.fill("#username", username);
  await page.fill("#password", PASSWORD);
  await page.click('button[type="submit"]');
}

test("a member of one storage logs in and lands directly in it", async ({ page }) => {
  // Bob belongs to exactly one storage, so there is nothing to choose.
  await logIn(page, "e2e-bob");

  await expect(page).toHaveURL(/\/storages\.html\?storage=00000000-0000-7000-8000-000000000010$/);
  await expect(page.locator("#main")).toContainText("E2E Household");
  await expect(page.locator("#main")).toContainText("Signed in as Bob.");
});

test("a member of two storages is asked which one", async ({ page }) => {
  await logIn(page, "e2e-alice");

  await expect(page).toHaveURL(/\/storages\.html/);
  await expect(page.locator("#main")).toContainText("Choose a storage");
  await expect(page.getByRole("button", { name: "E2E Household", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "E2E Other Household" })).toBeVisible();
});

test("a wrong password is refused with the generic message and no session", async ({ page }) => {
  await page.goto("/index.html");
  await page.fill("#username", "e2e-bob");
  await page.fill("#password", "not-the-password");
  await page.click('button[type="submit"]');

  await expect(page.locator("#login-error")).toHaveText("Incorrect username or password.");
  const cookies = await page.context().cookies();
  expect(cookies.find((c) => c.name === "inventory_session")).toBeUndefined();
});

test("logging out returns to the login page and the session is dead", async ({ page }) => {
  await logIn(page, "e2e-bob");
  await expect(page.locator("#main")).toContainText("Signed in as Bob.");

  const cookie = (await page.context().cookies()).find((c) => c.name === "inventory_session");
  expect(cookie, "login sets the session cookie").toBeDefined();
  expect(cookie.httpOnly).toBe(true);
  expect(cookie.secure).toBe(true);

  await page.click("#logout");
  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server

  // Replaying the old session id must not work: logout deleted the row, it
  // did not merely clear the browser's copy.
  const replay = await page.request.get("/api/auth/me", {
    headers: { Authorization: `Bearer ${cookie.value}` },
  });
  expect(replay.status()).toBe(401);

  // And a storage-scoped page now bounces to the login screen.
  await page.goto("/storages.html");
  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server
});
