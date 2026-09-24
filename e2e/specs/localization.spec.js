// Localization's one required E2E journey (docs/specs/19-localization.md):
// switch the language to German on settings.html, reload, and assert a known
// label renders from de.json and <html lang="de"> is set.
//
// This also covers the fallback direction implicitly: index.html and every
// other page load with no override at all, and this suite's default browser
// locale (Playwright's "en-US") must still render English without error —
// every other spec file's assertions on English button/label text already
// prove that continuously, so this file does not repeat it.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";
const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";

test("switching the language to German on settings.html renders de.json and sets <html lang>", async ({
  page,
}) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-alice", password: PASSWORD },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/settings.html?storage=${HOUSEHOLD}`);
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.locator("#language-section h3")).toHaveText("Language");

  await Promise.all([
    page.waitForEvent("load"),
    page.selectOption("#language-select", "de"),
  ]);

  await expect(page.locator("html")).toHaveAttribute("lang", "de");
  await expect(page.locator("#language-section h3")).toHaveText("Sprache");
  // A second, unrelated section proves the whole catalog switched, not just
  // the language picker's own labels.
  await expect(page.locator("#account-section h3")).toHaveText("Konto");

  // The override survives a plain reload (it is read from localStorage on
  // every page load, not just remembered in memory for this session).
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("lang", "de");
  await expect(page.locator("#language-section h3")).toHaveText("Sprache");

  // Switching back to English is the same mechanism in the other direction,
  // and proves "Match my browser" (empty override) still resolves to English
  // for this suite's en-US browser context.
  await Promise.all([
    page.waitForEvent("load"),
    page.selectOption("#language-select", ""),
  ]);
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.locator("#language-section h3")).toHaveText("Language");
});
