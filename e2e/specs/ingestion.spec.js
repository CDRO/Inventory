// Photo ingestion in the browser (docs/specs/06-vision-shelf-ingestion.md),
// required journey 4 of docs/specs/05-frontend-pwa-foundations.md as far as a
// stack without a vision model allows.
//
// The E2E stack's GEMINI_API_KEY is a placeholder, so no photo is ever really
// analysed here. What that leaves testable, and tested: the upload screen
// answering an unavailable model as a configuration problem, and — from
// proposals seeded in e2e/fixtures/seed.sql in exactly the shape the analysis
// job writes — the inbox, the review screen, confirming into real inventory,
// and discarding.

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const SHELF_JOB = "00000000-0000-7000-8000-000000000070";
const PRODUCT_JOB = "00000000-0000-7000-8000-000000000071";
const LOOK_ONLY_JOB = "00000000-0000-7000-8000-000000000072";
const TOMATOES = "00000000-0000-7000-8000-000000000040";
const PANTRY = "00000000-0000-7000-8000-000000000020";

// Stand-in photo bytes. The upload is refused on the model check, which runs
// before the body is read, so these never need to be a decodable image.
const TINY_JPEG = Buffer.from(
  "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=",
  "base64",
);

async function logInAsBob(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

test("uploading without a usable model says so, as a configuration problem", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  await page.locator("#photos").setInputFiles({ name: "shelf.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG });
  await page.getByRole("button", { name: "Upload" }).click();

  const card = page.locator("#uploads .card").first();
  // The model check lists models with a placeholder key, which can take a
  // moment to fail; the answer is still a 503, whatever the network does.
  await expect(card).toContainText("Not uploaded", { timeout: 20_000 });
  await expect(card).toContainText("AI model is unavailable");
});

test("the inbox lists waiting proposals with their size, and the badge counts them", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/storages.html?storage=${HOUSEHOLD}`);

  const badge = page.locator("#inbox-link .badge");
  await expect(badge).toBeVisible();
  await expect(badge).not.toHaveText("0");

  await page.locator("#inbox-link a").click();
  await expect(page).toHaveURL(/\/inbox\.html/);
  const shelf = page.locator(`[data-job-id="${LOOK_ONLY_JOB}"]`);
  await expect(shelf).toContainText("Shelf photo");
  await expect(shelf).toContainText("2 items");
  await expect(shelf.getByRole("link", { name: "Review" })).toBeVisible();
});

test("a proposal is reviewed and confirmed into inventory", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${SHELF_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(3);

  // Row 0: the matched product on the matched shelf — accepted as proposed.
  const tomatoes = rows.nth(0);
  await expect(tomatoes).toContainText("Canned Tomatoes 400g");
  await expect(tomatoes.locator('[data-role="product"]')).toHaveValue(`product:${TOMATOES}`);

  // Row 1: a new product on a shelf the photo proposed; the reviewer fixes the
  // quantity and sets the date.
  const oatMilk = rows.nth(1);
  await expect(oatMilk.locator('[data-role="location"]')).toHaveValue("new");
  await expect(oatMilk.locator('[data-role="new-product-name"]')).toHaveValue("Oat Milk 1L");
  await oatMilk.locator('[data-role="quantity"]').fill("4");
  await oatMilk.locator('[data-role="expiry"]').fill("2027-02-01");

  // Row 2: a false positive. Rejected rows stay visible and struck through.
  const mystery = rows.nth(2);
  await mystery.getByRole("button", { name: "Reject" }).click();
  await expect(mystery).toHaveClass(/review-row--rejected/);

  // The confirm is passed through a route handler so its body can be read:
  // the page navigates to the inbox the moment it succeeds, and a response
  // observed from outside loses its body with the page that made it.
  let sent = null;
  let written = null;
  let status = 0;
  await page.route(`**/ingest/${SHELF_JOB}/confirm`, async (route) => {
    sent = route.request().postDataJSON();
    const response = await route.fetch();
    status = response.status();
    written = await response.json();
    await route.fulfill({ response });
  });
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/inbox\.html/);

  // What the screen sent: every row decided, and the reviewer's edits intact —
  // the edited quantity and date, the proposed shelf as new_location below
  // Pantry, the existing product by id, and the rejection with nothing else.
  expect(sent.items).toEqual([
    { row_id: "0", decision: "accept", product_id: TOMATOES, quantity: 2, location_id: PANTRY },
    {
      row_id: "1",
      decision: "accept",
      new_product: { name: "Oat Milk 1L", item_type: "long_shelf_life" },
      quantity: 4,
      new_location: { parent_id: PANTRY, names: ["Top Shelf"] },
      expiration_date: "2027-02-01",
    },
    { row_id: "2", decision: "reject" },
  ]);

  // What the server wrote: two batches (the rejection wrote none), one new
  // product, one new location. How those rows persist quantity, date and
  // expiration_source is pinned against PostgreSQL in internal/store's
  // ingestion tests.
  expect(status).toBe(200);
  expect(written.batch_ids).toHaveLength(2);
  expect(written.products_created).toBe(1);
  expect(written.locations_created).toBe(1);

  await expect(page).toHaveURL(/\/inbox\.html\?.*confirmed=2/);
  await expect(page.locator("#notice")).toHaveText(
    "Proposal applied: 2 items added to your inventory, 1 new product, 1 new location.",
  );
  await expect(page.locator("#notice")).toHaveClass(/\balert--success\b/);
  await expect(page.locator(`[data-job-id="${SHELF_JOB}"]`)).toHaveCount(0);

  // The proposed shelf now exists under Pantry.
  const tree = await (await page.request.get(`/api/storages/${HOUSEHOLD}/locations`)).json();
  const pantry = tree.items.find((n) => n.name === "Pantry");
  expect(pantry.children.map((c) => c.name)).toContain("Top Shelf");

  // And the job cannot be applied a second time.
  const again = await page.request.post(`/api/storages/${HOUSEHOLD}/ingest/${SHELF_JOB}/confirm`, {
    data: { items: [{ row_id: "0", decision: "reject" }, { row_id: "1", decision: "reject" }, { row_id: "2", decision: "reject" }] },
  });
  expect(again.status()).toBe(409);
});

test("a proposal can be discarded from the inbox", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/inbox.html?storage=${HOUSEHOLD}`);

  const card = page.locator(`[data-job-id="${PRODUCT_JOB}"]`);
  await expect(card).toBeVisible();
  page.once("dialog", (dialog) => dialog.accept());
  await card.getByRole("button", { name: "Discard" }).click();
  await expect(card).toHaveCount(0);

  const gone = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${PRODUCT_JOB}`);
  expect(gone.status()).toBe(404);
});
