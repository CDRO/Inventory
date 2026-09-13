// Required journeys 7 and 8 of docs/specs/05-frontend-pwa-foundations.md,
// against the production-posture stack (APP_ENV=prod, HTTPS via Traefik):
// the admin area and a storage the caller does not belong to must both be
// indistinguishable from things that do not exist. Plus the admin area working
// for an actual admin, since a gate that refuses everyone would pass the rest.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";
const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const OTHER_HOUSEHOLD = "00000000-0000-7000-8000-000000000011";

async function apiLogIn(page, username) {
  const res = await page.request.post("/api/auth/login", {
    data: { username, password: PASSWORD },
  });
  expect(res.status(), `login as ${username}`).toBe(200);
}

async function snapshot(response) {
  return {
    status: response.status(),
    contentType: response.headers()["content-type"],
    body: await response.text(),
  };
}

test("a non-admin gets the same 404 from the admin area as from a route that does not exist", async ({
  page,
}) => {
  await apiLogIn(page, "e2e-bob");

  const reference = await snapshot(await page.request.get("/definitely-not-a-route"));
  expect(reference.status).toBe(404);
  expect(reference.body).not.toContain("debug_reason");

  const probes = [
    () => page.request.get("/admin"),
    () => page.request.get("/api/admin/users"),
    () => page.request.post("/api/admin/users", { data: { username: "mallory", password: "password123" } }),
    () => page.request.delete(`/api/admin/storages/${HOUSEHOLD}`),
    () => page.request.post("/api/admin/nonexistent", { data: {} }),
  ];
  for (const probe of probes) {
    expect(await snapshot(await probe())).toEqual(reference);
  }
});

test("a storage the caller is not in gets the same 404 as one that does not exist", async ({ page }) => {
  await apiLogIn(page, "e2e-bob");

  const own = await page.request.get(`/api/storages/${HOUSEHOLD}/locations`);
  expect(own.status(), "the check must be able to succeed").toBe(200);

  const notMine = await snapshot(await page.request.get(`/api/storages/${OTHER_HOUSEHOLD}/locations`));
  const missing = await snapshot(
    await page.request.get("/api/storages/00000000-0000-7000-8000-00000000ffff/locations"),
  );
  const malformed = await snapshot(await page.request.get("/api/storages/not-a-uuid/locations"));

  expect(notMine.status).toBe(404);
  expect(missing).toEqual(notMine);
  expect(malformed).toEqual(notMine);
});

test("an admin creates a user and grants them a storage from the admin page", async ({ page, browser }) => {
  const username = `e2e-new-${Date.now()}`;

  await apiLogIn(page, "e2e-admin");
  await page.goto("/admin");
  await expect(page.getByRole("heading", { name: "Admin" })).toBeVisible();

  // The page's only script runs under a nonce-based CSP. If the nonce did not
  // match, the form would fall back to a plain GET submit and this would fail.
  const createForm = page.locator('form[data-action="/api/admin/users"]');
  await createForm.locator('input[name="username"]').fill(username);
  await createForm.locator('input[name="display_name"]').fill("Newcomer");
  await createForm.locator('input[name="password"]').fill("a-fine-password");
  await createForm.getByRole("button", { name: "Create user" }).click();

  // Scoped to the users table: the storages table lists the new user too, as a
  // candidate in every grant form.
  const userRow = () => page.locator('section[aria-labelledby="users-heading"] tbody tr', { hasText: username });
  await expect(userRow()).toBeVisible();

  const grantForm = page.locator(`form[data-action="/api/admin/storages/${HOUSEHOLD}/members"]`);
  await grantForm.locator('select[name="user_id"]').selectOption({ label: `Newcomer (${username})` });
  await grantForm.getByRole("button", { name: "Add" }).click();
  await expect(userRow()).toContainText("E2E Household");

  // The new account works, in a separate browser context with its own cookies.
  // browser.newContext() does not inherit the config's `use` block, so the
  // base URL and certificate handling are passed along explicitly.
  const context = await browser.newContext({
    baseURL: test.info().project.use.baseURL,
    ignoreHTTPSErrors: true,
  });
  const newcomer = await context.newPage();
  await newcomer.goto("/index.html");
  await newcomer.fill("#username", username);
  await newcomer.fill("#password", "a-fine-password");
  await newcomer.click('button[type="submit"]');
  await expect(newcomer.locator("#main")).toContainText("Signed in as Newcomer.");
  await expect(newcomer.locator("#main")).toContainText("E2E Household");
  await context.close();
});
