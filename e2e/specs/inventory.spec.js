// The whole-inventory table (docs/specs/33-inventory-overview-table.md).
//
// Runs against its own storage and user, "E2E Inventory" / e2e-inventory,
// seeded in e2e/fixtures/seed.sql. The page is read-only, but its fixture is
// still dedicated and untouched by every other suite: the default-order and
// grouping assertions below depend on exact seeded quantities and dates that
// a parallel stocktake or consume confirm elsewhere must never disturb.

import { test, expect } from "@playwright/test";

const E2E_INVENTORY = "00000000-0000-7000-8000-000000000016";
const CELLAR = "00000000-0000-7000-8000-0000000000b0";

const EXPIRED_PRODUCT = "E2E Inventory Expired Item";
const NO_EXPIRY_PRODUCT = "E2E Inventory No-Expiry Item";
const FUTURE_PRODUCT = "E2E Inventory Future Item";
const GROUPED_PRODUCT = "E2E Inventory Grouped Item";

async function logInAsE2eInventory(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-inventory", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

function rowTexts(page) {
  return page.locator("#rows > tr").allTextContents();
}

test.describe("Inventory overview table", () => {
  test.beforeEach(async ({ page }) => {
    await logInAsE2eInventory(page);
  });

  test("lists every seeded batch with the location resolved root to leaf, expired first and no-expiry last by default", async ({ page }) => {
    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);
    await expect(page.locator("#table")).toBeVisible();

    const rows = page.locator("#rows > tr");
    await expect(rows).toHaveCount(5);

    const texts = await rowTexts(page);
    expect(texts.some((t) => t.includes(EXPIRED_PRODUCT))).toBe(true);
    expect(texts.some((t) => t.includes("Cellar") && t.includes("Shelf A"))).toBe(true);

    // Default sort is expiry ascending, nulls last: the expired batch leads,
    // the no-expiry one trails.
    expect(texts[0]).toContain(EXPIRED_PRODUCT);
    expect(texts[texts.length - 1]).toContain(NO_EXPIRY_PRODUCT);

    // aria-sort marks the active column and direction on the header, not
    // hidden in the button's own text.
    await expect(page.locator("th", { has: page.locator('[data-sort="expiry"]') })).toHaveAttribute("aria-sort", "ascending");
  });

  test("filtering by a location includes its descendants and the summary line counts only the shown rows", async ({ page }) => {
    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);
    await expect(page.locator("#table")).toBeVisible();

    await page.locator("#filter-location").selectOption(CELLAR);

    // Cellar itself holds nothing directly — every row shown here arrives
    // only because Shelf A is its descendant. The grouped product's second
    // batch lives in Freezer, an unrelated top-level location, and must be
    // excluded.
    await expect(page.locator("#rows > tr")).toHaveCount(4);
    const texts = await rowTexts(page);
    for (const text of texts) {
      expect(text).toContain("Shelf A");
    }

    await expect(page.locator("#summary")).toContainText("10");
    await expect(page.locator("#summary")).toContainText("4");
  });

  test("grouping by product folds the two batches into one row with the summed quantity", async ({ page }) => {
    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);
    await expect(page.locator("#table")).toBeVisible();

    await page.locator("#group-toggle").check();
    // Each folded row is followed by its own hidden detail row (the expand
    // target), so the DOM holds twice as many <tr>s as are actually shown.
    await expect(page.locator("#rows > tr:visible")).toHaveCount(4);

    const groupedRow = page.locator("#rows > tr:visible", { hasText: GROUPED_PRODUCT });
    await expect(groupedRow).toContainText("5"); // 3 + 2 summed
    await expect(groupedRow).toContainText("2"); // two distinct locations
  });

  test("a filter matching nothing shows the no-rows-match state with Clear filters, distinct from the empty-storage message", async ({ page }) => {
    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);
    await expect(page.locator("#table")).toBeVisible();

    await page.locator("#filter-text").fill("no such product exists anywhere");

    await expect(page.locator("#empty-filtered")).toBeVisible();
    await expect(page.locator("#empty-storage")).toBeHidden();
    await expect(page.locator("#empty-filtered")).toContainText("No rows match");
    await expect(page.getByRole("button", { name: "Clear filters" })).toBeVisible();

    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.locator("#rows > tr")).toHaveCount(5);
  });

  test("Count this shelf opens that location's stocktake sheet", async ({ page }) => {
    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);
    await expect(page.locator("#table")).toBeVisible();

    const row = page.locator("#rows > tr", { hasText: FUTURE_PRODUCT });
    await row.getByRole("link", { name: "Count this shelf" }).click();

    await expect(page).toHaveURL(/\/stocktake\.html\?.*location=/);
  });

  // With limit rewritten down to 2 on the first request, the client — which
  // always follows next_cursor regardless of the limit it asked for — must
  // issue a second one for the remaining rows. Failing that second request
  // is what forces the "table is incomplete" state a fixture with only 5
  // rows could never reach on its own (they all fit in one real page).
  test("a failed later page renders what loaded and says the table is incomplete", async ({ page }) => {
    let requestCount = 0;
    await page.route(`**/api/storages/${E2E_INVENTORY}/inventory-batches*`, async (route) => {
      requestCount++;
      if (requestCount === 1) {
        const url = new URL(route.request().url());
        url.searchParams.set("limit", "2");
        const response = await route.fetch({ url: url.toString() });
        await route.fulfill({ response });
        return;
      }
      await route.abort("failed");
    });

    await page.goto(`/inventory.html?storage=${E2E_INVENTORY}`);

    await expect(page.locator("#incomplete")).toBeVisible();
    await expect(page.locator("#incomplete")).toContainText("incomplete");
    await expect(page.locator("#rows > tr")).toHaveCount(2);
  });
});

test.describe("Inventory overview table — empty storage", () => {
  // A genuinely empty storage, not "E2E Inventory": the empty-storage
  // message and the no-rows-match-filters message must never be the same
  // wording, and this is what proves the first one only fires when there is
  // truly nothing recorded.
  test("a storage with no batches shows the empty-storage state, not the no-rows-match one", async ({ page }) => {
    const res = await page.request.post("/api/auth/login", {
      data: { username: "e2e-casey", password: "e2e-fixture-password" },
    });
    expect(res.status()).toBe(200);

    await page.goto("/inventory.html?storage=00000000-0000-7000-8000-000000000013");

    await expect(page.locator("#empty-storage")).toBeVisible();
    await expect(page.locator("#empty-filtered")).toBeHidden();
  });
});
