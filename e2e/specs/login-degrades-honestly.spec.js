// The login form's failure path: an unknown account must produce a visible,
// non-empty error and a form that can be retried, never a hang or a silent
// no-op. The successful journey lives in auth-journeys.spec.js.

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
