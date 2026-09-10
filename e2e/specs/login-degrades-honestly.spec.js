// docs/specs/05-frontend-pwa-foundations.md's first required journey is
// "Log in, land in a storage; log out" — not testable end to end until
// POST /api/auth/login exists (issue #27). What this file proves instead is
// narrower but real: the login form, talking to the actual backend as it
// exists today, degrades honestly rather than hanging or failing silently.
// Once #27 lands this spec keeps working unchanged — the assertions below
// check that an error is shown and is non-empty, not its exact wording,
// specifically so the switch from today's 405 to a real 401 does not require
// touching this file.

import { test, expect } from "@playwright/test";

test("submitting the login form shows an error rather than hanging or failing silently", async ({
  page,
}) => {
  await page.goto("/index.html");

  await page.fill("#username", "someone");
  await page.fill("#password", "wrong-password");

  const errorBox = page.locator("#login-error");
  await expect(errorBox).toBeHidden();

  await page.click('button[type="submit"]');

  await expect(errorBox).toBeVisible();
  await expect(errorBox).not.toBeEmpty();

  // The submit button must return to a usable state — a login form stuck on
  // "Signing in…" forever, with no way to retry, would be a worse failure
  // than the honest error message itself.
  await expect(page.locator('button[type="submit"]')).toBeEnabled();
  await expect(page.locator('button[type="submit"]')).toHaveText("Sign in");

  // And the page must not have navigated away on failure.
  await expect(page).toHaveURL(/\/$/);
});

test("the login form's own HTML5 validation blocks an empty submission", async ({ page }) => {
  await page.goto("/index.html");

  // No fetch should be attempted at all: the browser's native required-field
  // validation should stop the submit before it happens.
  let requestMade = false;
  page.on("request", (req) => {
    if (req.url().includes("/api/auth/login")) requestMade = true;
  });

  await page.click('button[type="submit"]');
  await page.waitForTimeout(300);

  expect(requestMade).toBe(false);
});
