// Required journey 2 of docs/specs/05-frontend-pwa-foundations.md: "A user
// with two storages switches between them and sees each storage's own data."
//
// The switch itself is only half the claim. A switcher that changed the URL
// while the page kept showing the previous household's shelves would satisfy
// every assertion about the control and none about the guarantee, so each
// step below asserts in both directions — the new storage's data is present
// *and* the old storage's data is gone.
//
// Alice is the fixture's two-storage user and Bob the one-storage user
// (e2e/fixtures/seed.sql). Both are needed: "hide the switcher entirely" for a
// single membership is the same rule's other branch, and without Bob a
// switcher that rendered unconditionally would pass this file.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";
const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const OTHER_HOUSEHOLD = "00000000-0000-7000-8000-000000000011";

async function logIn(page, username) {
  await page.goto("/index.html");
  await page.fill("#username", username);
  await page.fill("#password", PASSWORD);
  await page.click('button[type="submit"]');
}

test("a two-storage user picks one, switches to the other, and each shows its own locations", async ({
  page,
}) => {
  await logIn(page, "e2e-alice");

  // Two memberships and nothing remembered yet, so storages.html asks rather
  // than guessing (docs/specs/05-frontend-pwa-foundations.md).
  await expect(page.locator("#main")).toContainText("Choose a storage");
  await page.getByRole("button", { name: "E2E Household", exact: true }).click();

  await expect(page).toHaveURL(new RegExp(`storage=${HOUSEHOLD}$`));
  await expect(page.locator("#main")).toContainText("E2E Household");

  // The locations page is where the two storages actually differ, so the
  // switch is judged on data rather than on the header's own label.
  await page.goto(`/locations.html?storage=${HOUSEHOLD}`);
  await expect(page.locator("#tree")).toContainText("Pantry");
  await expect(page.locator("#tree")).toContainText("Fridge");
  await expect(page.locator("#tree")).not.toContainText("Garage");

  const switcher = page.locator("#storage-switcher select");
  await expect(switcher).toBeVisible();
  await expect(switcher).toHaveValue(HOUSEHOLD);

  // Choosing a different storage is a fresh page load carrying the new id,
  // not a client-side swap — "the URL always describes what is on screen".
  await switcher.selectOption(OTHER_HOUSEHOLD);
  await expect(page).toHaveURL(new RegExp(`storage=${OTHER_HOUSEHOLD}`));

  await expect(page.locator("#tree")).toContainText("Garage");
  await expect(page.locator("#tree")).not.toContainText("Pantry");
  await expect(page.locator("#tree")).not.toContainText("Fridge");
  await expect(page.locator("#error")).toBeHidden();

  // The switcher survives the navigation it caused and now reads the other
  // way round, so a second switch is possible without going via storages.html.
  await expect(page.locator("#storage-switcher select")).toHaveValue(OTHER_HOUSEHOLD);
});

test("the switched-to storage is what a later page load resolves to", async ({ page }) => {
  await logIn(page, "e2e-alice");
  await expect(page.locator("#main")).toContainText("Choose a storage");
  await page.getByRole("button", { name: "E2E Other Household" }).click();
  await expect(page).toHaveURL(new RegExp(`storage=${OTHER_HOUSEHOLD}$`));

  // A page opened with no `?storage=` must not re-ask: the choice was already
  // made and remembered, and asking again on every navigation would make the
  // switcher pointless.
  await page.goto("/locations.html");
  await expect(page).toHaveURL(new RegExp(`storage=${OTHER_HOUSEHOLD}`));
  await expect(page.locator("#tree")).toContainText("Garage");
});

test("a one-storage user never sees the switcher at all", async ({ page }) => {
  // Bob has exactly one membership: "Exactly one storage → use it, and hide
  // the switcher entirely" (docs/specs/05-frontend-pwa-foundations.md). Hidden
  // rather than rendered-but-empty, which is why this checks the container.
  await logIn(page, "e2e-bob");
  await expect(page).toHaveURL(new RegExp(`storage=${HOUSEHOLD}$`));

  await expect(page.locator("#storage-switcher")).toBeHidden();
  await expect(page.locator("#storage-switcher select")).toHaveCount(0);

  await page.goto("/locations.html");
  await expect(page.locator("#tree")).toContainText("Pantry");
  await expect(page.locator("#storage-switcher")).toBeHidden();
});
