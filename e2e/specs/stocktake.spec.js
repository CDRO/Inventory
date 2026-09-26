// Stocktake entry points (docs/specs/35-stocktake-entry-points.md): the
// location chooser stocktake.html renders with no `?location=`, the batch
// row's "Count this shelf" link on products.html, the navigation bar's
// Stocktake entry, and the sheet's referrer-aware Back/Cancel. Frontend only —
// spec 13's stocktake endpoints and their exact-row-set confirm are unchanged.
//
// Runs against its own storage and user, "E2E Stocktake" / e2e-stocktake
// (e2e/fixtures/seed.sql), because confirming a walk writes quantities and
// last_audited_at — the same reasoning e2e-inbox and e2e-inventory's fixture
// comments give for their own dedicated storages, just for writing rather
// than for deleting or reading.
//
// This file runs serially: the first test confirms a stocktake on "Fridge",
// and the second depends on knowing which locations are and are not audited
// yet — a concurrent run of the two would make that state a race between
// them.

import { test, expect } from "@playwright/test";

test.describe.configure({ mode: "serial" });

const STORAGE = "00000000-0000-7000-8000-00000000001a";
const PANTRY = "00000000-0000-7000-8000-0000000000f0"; // never audited
const FRIDGE = "00000000-0000-7000-8000-0000000000f1"; // holds the one batch
const PRODUCT_NAME = "E2E Stocktake Widget";
const BATCH = "00000000-0000-7000-8000-0000000000f8";

// A location that exists, but not in this storage — "Cellar" in
// "E2E Inventory" (docs/specs/33-inventory-overview-table.md's fixture). Used
// to prove a foreign id 404s exactly like one that was never real.
const FOREIGN_LOCATION = "00000000-0000-7000-8000-0000000000b0";

// "E2E Stocktake Empty" / e2e-stocktake-empty (e2e/fixtures/seed.sql), its own
// dedicated storage with zero locations — #174 item 3. Not "E2E Stocktake"
// above: every location in this file's own fixture exists precisely so the
// stalest-first and single-location scenarios have something to walk.
const EMPTY_STORAGE = "00000000-0000-7000-8000-00000000001b";

async function logIn(page, username = "e2e-stocktake") {
  const res = await page.request.post("/api/auth/login", {
    data: { username, password: "e2e-fixture-password" },
  });
  expect(res.status(), "fixture login").toBe(200);
}

test("Count this shelf on the product page corrects the quantity, and Back returns to the product page", async ({ page }) => {
  await logIn(page);
  await page.goto(`/products.html?storage=${STORAGE}`);
  await page.getByRole("button", { name: PRODUCT_NAME, exact: true }).click();

  // The stock card no longer claims locations or expiry dates are edited on
  // the stocktake sheet — only quantities are, corrected shelf by shelf.
  await expect(page.locator("main")).toContainText("Counts are corrected shelf by shelf on the");
  await expect(page.locator("main")).toContainText("stocktake sheet");

  const row = page.locator(`[data-role="batch-row"][data-batch-id="${BATCH}"]`);
  await expect(row).toContainText("3 × Fridge");
  await row.getByRole("link", { name: "Count this shelf" }).click();

  await expect(page).toHaveURL(new RegExp(`/stocktake\\.html\\?location=${FRIDGE}`));
  await expect(page.locator("#heading")).toContainText("Fridge");

  await page.getByLabel(`Counted quantity of ${PRODUCT_NAME}`).fill("7");
  await page.locator("#confirm").click();
  await expect(page.locator("#notice")).toBeVisible();

  await page.locator("#back").click();
  await expect(page).toHaveURL(new RegExp(`/products\\.html\\?storage=${STORAGE}`));

  // Back lands on products.html with no ?product=, so the detail view — and
  // the batch row inside it — is not shown until the product is selected
  // again (products.js only auto-opens one from a ?product= deep link).
  await page.getByRole("button", { name: PRODUCT_NAME, exact: true }).click();
  await expect(row).toContainText("7 × Fridge");
});

test("Stocktake from the navigation bar shows the Stalest-first chooser, and confirming a walk moves it off the list", async ({ page }) => {
  await logIn(page);
  await page.goto(`/locations.html?storage=${STORAGE}`);
  await page.locator("#nav").getByRole("link", { name: "Stocktake" }).click();

  await expect(page).toHaveURL(new RegExp(`/stocktake\\.html\\?storage=${STORAGE}$`));
  await expect(page.locator("#chooser")).toBeVisible();
  await expect(page.locator("#sheet")).toBeHidden();

  const stalestItems = page.locator("#stalest li");
  await expect(stalestItems).toHaveCount(5);
  // Never-audited first, then oldest-audited first: Pantry, then Shed, Attic,
  // Cellar, Garage in that order (e2e/fixtures/seed.sql). Fridge and Freezer
  // are the two freshest and must not appear. Each <li> renders two <span>s
  // (name, then audited-state), so the first one per item is the name.
  const names = await Promise.all(
    (await stalestItems.all()).map((item) => item.locator("span").first().textContent()),
  );
  expect(names).toEqual(["Pantry", "Shed", "Attic", "Cellar", "Garage"]);

  const stalestText = await page.locator("#stalest").textContent();
  expect(stalestText).not.toContain("Freezer");

  // The chooser's own read-only tree (not just the "Stalest first" list)
  // renders every location with the same audited state and a Count link, and
  // stays read-only: no rename/add-child/move-to controls, unlike the same
  // component on locations.html (js/tree.js's new `editable: false` mode).
  const chooserTree = page.locator("#chooser-tree");
  await expect(chooserTree.locator(".tree-node")).toHaveCount(7);
  await expect(chooserTree.getByRole("link", { name: "Count" })).toHaveCount(7);
  await expect(chooserTree.getByRole("button", { name: "Rename" })).toHaveCount(0);
  await expect(chooserTree.getByRole("button", { name: "Add", exact: true })).toHaveCount(0);
  await expect(chooserTree.getByRole("button", { name: "Move to…" })).toHaveCount(0);

  // Confirm the first entry (Pantry, empty shelf) unchanged.
  await stalestItems.first().getByRole("link", { name: "Count" }).click();
  await expect(page).toHaveURL(new RegExp(`/stocktake\\.html\\?location=${PANTRY}`));
  await expect(page.locator("#empty")).toBeVisible();
  await page.locator("#confirm").click();
  await expect(page.locator("#notice")).toBeVisible();

  // Back returns to the chooser page this walk was opened from.
  await page.locator("#back").click();
  await expect(page.locator("#chooser")).toBeVisible();

  // Pantry is now audited (just now) and has left the list; Freezer (10 days
  // ago) is now the fifth-oldest, since Fridge — audited by the other test in
  // this file — is fresher than it either way this file's tests happen to be
  // scheduled (this file runs serially; see the header comment).
  const namesAfter = await Promise.all(
    (await page.locator("#stalest li").all()).map((item) => item.locator("span").first().textContent()),
  );
  expect(namesAfter).toEqual(["Shed", "Attic", "Cellar", "Garage", "Freezer"]);
});

test("a random or foreign location shows the same not-found message and the chooser below it", async ({ page }) => {
  await logIn(page);

  await page.goto(`/stocktake.html?location=11111111-1111-7111-8111-111111111111&storage=${STORAGE}`);
  await expect(page.locator("#error")).toContainText("could not be found");
  await expect(page.locator("#sheet")).toBeHidden();
  await expect(page.locator("#chooser")).toBeVisible();
  const messageForRandom = await page.locator("#error").textContent();

  await page.goto(`/stocktake.html?location=${FOREIGN_LOCATION}&storage=${STORAGE}`);
  await expect(page.locator("#error")).toContainText("could not be found");
  await expect(page.locator("#sheet")).toBeHidden();
  await expect(page.locator("#chooser")).toBeVisible();
  const messageForForeign = await page.locator("#error").textContent();

  expect(messageForForeign).toBe(messageForRandom);
});

test("Cancel returns to the page the walk was started from, or to locations.html with no referrer", async ({ page }) => {
  await logIn(page);

  await page.goto(`/inventory.html?storage=${STORAGE}`);
  await expect(page.locator("#table")).toBeVisible();
  await page.locator("#rows > tr", { hasText: PRODUCT_NAME }).getByRole("link", { name: "Count this shelf" }).click();
  await expect(page).toHaveURL(new RegExp(`/stocktake\\.html\\?location=${FRIDGE}`));
  await page.locator("#cancel").click();
  await expect(page).toHaveURL(new RegExp(`/inventory\\.html\\?storage=${STORAGE}`));

  // A direct URL visit carries no referrer.
  await page.goto(`/stocktake.html?location=${FRIDGE}&storage=${STORAGE}`);
  await page.locator("#cancel").click();
  await expect(page).toHaveURL(new RegExp(`/locations\\.html\\?storage=${STORAGE}`));
});

// #174 item 3: docs/specs/35-stocktake-entry-points.md — "If a storage has no
// locations at all, the page shows an empty state that links to
// locations.html." Implemented in renderChooser and #chooser-empty, but with
// no dedicated fixture or test in #173.
test("a storage with no locations at all shows the chooser's empty state, linking to locations.html", async ({ page }) => {
  await logIn(page, "e2e-stocktake-empty");

  await page.goto(`/stocktake.html?storage=${EMPTY_STORAGE}`);
  await expect(page.locator("#chooser")).toBeVisible();
  await expect(page.locator("#sheet")).toBeHidden();

  await expect(page.locator("#chooser-empty")).toBeVisible();
  await expect(page.locator("#chooser-tree")).toBeEmpty();
  await expect(page.locator("#stalest")).toBeEmpty();

  await expect(page.locator("#chooser-empty-link")).toHaveAttribute(
    "href",
    new RegExp(`/locations\\.html\\?storage=${EMPTY_STORAGE}`),
  );
});
