// docs/specs/29-first-run-admin-guidance.md, end to end against the
// production-posture stack (APP_ENV=prod, HTTPS via Traefik).
//
// The thing under test is a journey, not a route: nobody types a URL in any of
// these tests. Each user logs in through the form and the suite asserts where
// they end up, because "the operator stops being stranded on a dead end" is
// only true if the redirect chain — login → storages.html → /no-storages →
// somewhere useful — runs end to end in a real browser with a service worker
// installed. The route's own four answers are covered as unit tests in
// internal/httpapi/navigation_test.go, which is also where the exact Location
// of each is pinned.
//
// Fixture users come from e2e/fixtures/seed.sql: e2e-admin is an admin with no
// storage (a fresh deployment's bootstrap account), e2e-nomad a non-admin with
// no storage, e2e-admin-2 an admin who belongs to one.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";
const ADMIN_HOUSEHOLD = "00000000-0000-7000-8000-000000000012";

async function logIn(page, username) {
  await page.goto("/index.html");
  await page.fill("#username", username);
  await page.fill("#password", PASSWORD);
  await page.click('button[type="submit"]');
}

// Journey: "a freshly seeded deployment's bootstrap admin, after logging in,
// ends on the admin page without typing a URL."
//
// This is the whole point of the spec. Before it, the first thing the operator
// of a new deployment saw was "Ask an admin to add you to a storage" — with no
// admin but themselves, and nothing on the page admitting the admin view
// exists, because docs/specs/03-auth-and-multi-tenancy.md forbids the SPA from
// mentioning it.
test("the bootstrap admin, who has no storage, lands in the admin area after logging in", async ({
  page,
}) => {
  await logIn(page, "e2e-admin");

  await expect(page).toHaveURL(/\/admin$/);
  await expect(page.getByRole("heading", { name: "Admin" })).toBeVisible();

  // And they got there without ever being shown the dead end.
  await expect(page.locator("body")).not.toContainText("Ask an admin to add you to a storage");
});

// Journey: "a non-admin with no storage ends on the empty state and stays
// there."
//
// Same starting position, different answer, and the decision was made by the
// server: this user's browser ran exactly the same JavaScript as the admin's.
// "Stays there" is the half that would catch a redirect loop — the page
// bouncing to /no-storages, being sent back, and bouncing again.
test("a non-admin with no storage lands on the empty state and stays on it", async ({ page }) => {
  await logIn(page, "e2e-nomad");

  await expect(page).toHaveURL(/\/storages\.html\?empty=1$/);
  await expect(page.getByRole("heading", { name: "No storage yet" })).toBeVisible();
  await expect(page.locator("#main")).toContainText("Ask an admin to add you to a storage");

  // Nothing about the admin area reaches this user, in the URL or on the page.
  await expect(page.locator("body")).not.toContainText("Admin");

  // Settled, not mid-flight: re-reading the URL after the empty state has
  // rendered is what proves the page is not about to bounce again. The
  // redirect the page performs is a location.replace issued during init(), so
  // it would already have happened by the time the heading above was visible.
  await expect(page).toHaveURL(/\/storages\.html\?empty=1$/);

  // Back must not walk into the redirect again: location.replace left no
  // history entry for it to return to, so this leaves the app rather than
  // looping.
  await page.goBack();
  await expect(page).not.toHaveURL(/\/storages\.html/);
});

// Journey: "an admin who belongs to a storage lands on the normal storage flow
// and is not redirected."
//
// The boundary. Redirecting an admin away from the storages page is right only
// while they have nothing there; an admin who has added themselves to a
// storage is an ordinary user of it, and sending them to the admin area every
// time they opened the app would be the new dead end.
test("an admin who belongs to a storage lands in it, not in the admin area", async ({ page }) => {
  await logIn(page, "e2e-admin-2");

  // Forwarded to their storage's start page rather than shown a landing
  // card (docs/specs/34-navigation-and-start-page.md); the default is the
  // dashboard. The claim under test is unchanged — they were not sent to
  // the admin area — and is now made against where they *did* land.
  await expect(page).toHaveURL(new RegExp(`/dashboard[.]html[?]storage=${ADMIN_HOUSEHOLD}$`));
  await expect(page.locator("#main h2").first()).toHaveText("Dashboard");

  // Still an admin — the redirect is about memberships, not about rights —
  // which is what makes this test a boundary rather than a second non-admin.
  const admin = await page.request.get("/admin");
  expect(admin.status(), "they must still be able to reach the admin area by URL").toBe(200);
});
