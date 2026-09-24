// Localization (docs/specs/19-localization.md). Covers every acceptance
// criterion that needs a real browser rather than a Go unit test:
//
//   - switching the language on settings.html renders de.json and sets
//     <html lang>, and the override survives a reload
//   - a de-CH browser with no override resolves to German; an unsupported
//     browser language falls back to English without an error
//   - Intl date/number formatting actually renders German conventions
//   - apiErrorMessage() renders the localized string for a known error code
//     and the server's own message for an unknown one
//   - the admin area is unaffected by the PWA language override
//
// The Go-side key-parity/placeholder-parity tests live in web/i18n_test.go.

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

  // Switching back to English is the same mechanism in the other direction.
  await Promise.all([
    page.waitForEvent("load"),
    page.selectOption("#language-select", ""),
  ]);
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.locator("#language-section h3")).toHaveText("Language");
});

// Each describe block below runs in its own browser context (Playwright
// creates a fresh one per `test.use({ locale })`), so none of them can see
// e2e-alice's session or the localStorage override the test above set — the
// language resolution these exercise is genuinely override-free, driven only
// by the browser's own reported language.

test.describe("a de-CH browser with no override", () => {
  test.use({ locale: "de-CH" });

  test("resolves to German", async ({ page }) => {
    await page.goto("/index.html");
    await expect(page.locator("html")).toHaveAttribute("lang", "de");
    await expect(page.locator('label[for="username"]')).toHaveText("Benutzername");
    await expect(page.locator('button[type="submit"]')).toHaveText("Anmelden");
  });
});

test.describe("an unsupported browser language with no override", () => {
  test.use({ locale: "fr-FR" });

  test("falls back to English without an error", async ({ page }) => {
    const pageErrors = [];
    page.on("pageerror", (err) => pageErrors.push(err));

    await page.goto("/index.html");
    await expect(page.locator("html")).toHaveAttribute("lang", "en");
    await expect(page.locator('label[for="username"]')).toHaveText("Username");
    await expect(page.locator('button[type="submit"]')).toHaveText("Sign in");

    expect(pageErrors, `expected no page errors, got: ${pageErrors.map((e) => e.message)}`).toHaveLength(0);
  });
});

// formatDate/formatNumber (web/static/js/i18n.js) are exercised directly via
// a dynamic import of the real module the app serves, rather than through a
// page that happens to render a formatted value — this is what actually
// pins "de active -> Intl renders German conventions" against a regression
// that hardcoded "en" inside either function, which no page-level assertion
// on button/label text would ever catch.
test("formatDate and formatNumber render German Intl conventions when de is active", async ({ page }) => {
  await page.goto("/index.html");
  await page.evaluate(() => localStorage.setItem("inventory.language", "de"));
  await page.reload();

  const { date, number } = await page.evaluate(async () => {
    const { formatDate, formatNumber } = await import("/js/i18n.js");
    return {
      date: formatDate(new Date(Date.UTC(2026, 2, 15)), { dateStyle: "long", timeZone: "UTC" }),
      number: formatNumber(1234.5),
    };
  });

  expect(date).toContain("März"); // German month name; "March"/"Mar" in English
  expect(number).toBe("1.234,5"); // German decimal comma + thousands dot, vs. English "1,234.5"
});

// apiErrorMessage (web/static/js/i18n.js) is likewise exercised directly: a
// real server round trip would only prove one status/code combination per
// UI action found to trigger it, whereas this pins the actual boundary the
// acceptance criterion describes — a known code translates, an unknown one
// falls back to the server's own message — independent of which endpoint
// happens to produce either.
test("apiErrorMessage renders the localized string for a known code and the server message for an unknown one", async ({
  page,
}) => {
  await page.goto("/index.html");

  const englishKnown = await page.evaluate(async () => {
    const { apiErrorMessage } = await import("/js/i18n.js");
    return apiErrorMessage({ code: "not_found", message: "Not found." });
  });
  expect(englishKnown).toBe("Not found.");

  const unknown = await page.evaluate(async () => {
    const { apiErrorMessage } = await import("/js/i18n.js");
    return apiErrorMessage({ code: "some_future_code_with_no_translation", message: "A raw server message." });
  });
  expect(unknown).toBe("A raw server message.");

  await page.evaluate(() => localStorage.setItem("inventory.language", "de"));
  await page.reload();

  const germanKnown = await page.evaluate(async () => {
    const { apiErrorMessage } = await import("/js/i18n.js");
    return apiErrorMessage({ code: "not_found", message: "Not found." });
  });
  expect(germanKnown).toBe("Nicht gefunden.");
});

// The one place a language override could leak into the admin area is if
// something ever imported js/i18n.js from an admin template — this proves
// the invariant holds today, not just that no admin file appears in a diff.
test("the admin area stays English regardless of the PWA language override", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin", password: PASSWORD },
  });
  expect(login.status()).toBe(200);

  await page.goto("/index.html");
  await page.evaluate(() => localStorage.setItem("inventory.language", "de"));

  await page.goto("/admin");
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.locator("h1")).toHaveText("Admin");
  await expect(page.locator("#users-heading")).toHaveText("Users");
});

// expiration_date is a bare DATE (migrations/00002_core_schema.sql), not a
// timestamp — round 2 review caught that formatDate() call sites for it
// omitted `timeZone: "UTC"`, so parsing "2030-01-01" as UTC midnight and then
// formatting in the *viewer's* zone rendered every expiry one calendar day
// early for anyone west of UTC. This test only means something under a
// non-UTC zone, which is exactly why the bug shipped once already: this
// suite's other tests all run under the container's default (UTC) timezone.
test.describe("a batch expiry date under a non-UTC browser timezone", () => {
  test.use({ timezoneId: "America/New_York" });

  const HOUSEHOLD_STORAGE = "00000000-0000-7000-8000-000000000010";
  // Greek Yogurt's Fridge batch (e2e/fixtures/seed.sql), expiration_date
  // 2030-01-01 — read-only here, so sharing it with consumption.spec.js's
  // parallel run is safe.
  const YOGURT_FRIDGE_BATCH = "00000000-0000-7000-8000-000000000053";

  test("still renders the calendar date the server sent, not one day early", async ({ page }) => {
    const login = await page.request.post("/api/auth/login", {
      data: { username: "e2e-alice", password: PASSWORD },
    });
    expect(login.status()).toBe(200);

    await page.goto(`/products.html?storage=${HOUSEHOLD_STORAGE}`);
    await page.getByRole("button", { name: "Greek Yogurt", exact: true }).click();

    const row = page.locator('[data-role="batch-row"][data-batch-id="' + YOGURT_FRIDGE_BATCH + '"]');
    await expect(row).toContainText("Jan 1, 2030");
    await expect(row).not.toContainText("Dec 31, 2029");
  });
});
