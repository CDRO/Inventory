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

// formatAudited (web/static/js/audited.js) switched Intl.RelativeTimeFormat
// from the browser's default locale to getLanguage(), the resolved app
// language, in the same PR that introduced getLanguage() — a real behaviour
// change with no test asserting the German phrasing actually renders (#169).
// Exercised through a real page (locations.html) rather than a dynamic
// import like the formatDate/formatNumber test above: audited.js builds its
// one `relative` Intl.RelativeTimeFormat instance at module-load time, so a
// fresh import that runs before the language override is read would not
// prove anything about a page that loaded it after.
test("a location's audited state renders the German relative-time phrase once de is active", async ({
  page,
}) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-alice", password: PASSWORD },
  });
  expect(login.status()).toBe(200);

  await page.goto("/index.html");
  await page.evaluate(() => localStorage.setItem("inventory.language", "de"));

  await page.goto(`/locations.html?storage=${HOUSEHOLD}`);
  await expect(page.locator("html")).toHaveAttribute("lang", "de");

  // "E2E Audited Shelf" (e2e/fixtures/seed.sql), last_audited_at 5 days ago.
  const AUDITED_SHELF = "00000000-0000-7000-8000-0000000000ba";
  const detail = page.locator(`.tree-node[data-id="${AUDITED_SHELF}"] .muted`);
  await expect(detail).toHaveText("geprüft vor 5 Tagen");
  await expect(detail).not.toContainText("days ago");
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

  // The mechanism-level test above proves formatDate() itself renders German
  // conventions; this proves a real page that calls it — products.html's
  // batch list — actually does, once de is active, not just in isolation.
  test("renders the same date in German once the language override is set", async ({ page }) => {
    const login = await page.request.post("/api/auth/login", {
      data: { username: "e2e-alice", password: PASSWORD },
    });
    expect(login.status()).toBe(200);

    await page.goto("/index.html");
    await page.evaluate(() => localStorage.setItem("inventory.language", "de"));

    await page.goto(`/products.html?storage=${HOUSEHOLD_STORAGE}`);
    await expect(page.locator("html")).toHaveAttribute("lang", "de");
    await page.getByRole("button", { name: "Greek Yogurt", exact: true }).click();

    const row = page.locator('[data-role="batch-row"][data-batch-id="' + YOGURT_FRIDGE_BATCH + '"]');
    await expect(row).toContainText("01.01.2030"); // Intl.DateTimeFormat("de", {dateStyle:"medium"})
  });

  // stocktake.js's expiry badge is a separate call site with the identical
  // fix (web/static/js/pages/stocktake.js:161) — proven separately because a
  // revert of just this site would otherwise pass every other test here.
  const FRIDGE_LOCATION = "00000000-0000-7000-8000-000000000021";

  test("stocktake.html's expiry badge also survives the same non-UTC timezone", async ({ page }) => {
    const login = await page.request.post("/api/auth/login", {
      data: { username: "e2e-alice", password: PASSWORD },
    });
    expect(login.status()).toBe(200);

    await page.goto(`/stocktake.html?location=${FRIDGE_LOCATION}&storage=${HOUSEHOLD_STORAGE}`);
    const row = page.locator("#rows .review-row").filter({ hasText: "Greek Yogurt" });
    // stocktake.js's formatDate() call passes no dateStyle, so Intl's default
    // (numeric) format applies: "1/1/2030" for "en".
    await expect(row).toContainText("1/1/2030");
    await expect(row).not.toContainText("12/31/2029");
  });

  // renderFound's found-on-shelf list (stocktake.js:414) is a third call
  // site with the identical fix, exercised through the found-item form
  // rather than a fixture row: addFound() renders the list purely from what
  // was typed, with no round trip to the server, so this needs no seed data
  // beyond a product that already exists in this storage.
  const GREEK_YOGURT = "00000000-0000-7000-8000-000000000041";

  test("the found-on-shelf list also survives the same non-UTC timezone", async ({ page }) => {
    const login = await page.request.post("/api/auth/login", {
      data: { username: "e2e-alice", password: PASSWORD },
    });
    expect(login.status()).toBe(200);

    await page.goto(`/stocktake.html?location=${FRIDGE_LOCATION}&storage=${HOUSEHOLD_STORAGE}`);
    await page.selectOption("#found-product", GREEK_YOGURT);
    await page.fill("#found-expiry", "2030-01-01");
    await page.click("#add-found button[type=submit]");

    // Scoped to the ROW, not to #found itself: renderFound appends one
    // div.row.row--between per entry (web/static/js/pages/stocktake.js:399),
    // and filtering #found — a single container div — by its own subtree
    // resolves back to the whole list. With one entry that passes either way,
    // but the day the list holds two, a correct date on one row would satisfy
    // the assertion for the other. There are no <li>s here; .row is the row.
    const item = page.locator("#found .row").filter({ hasText: "Greek Yogurt" });
    await expect(item).toContainText("1/1/2030");
    await expect(item).not.toContainText("12/31/2029");
  });
});
