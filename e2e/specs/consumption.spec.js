// Consumption logging in the browser (docs/specs/09-consumption-logging.md).
//
// Like ingestion.spec.js, no photo here is ever really analysed — the E2E
// stack's GEMINI_API_KEY is a placeholder. What is testable, and tested: the
// review screen built from a proposal seeded in e2e/fixtures/seed.sql in
// exactly the shape internal/consume writes — the existing-product picker
// (including the manual-correction search path, since consumption never
// creates a product), the batch picker's default nearest-expiry allocation,
// and confirming a decrement into real inventory.

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const CONSUME_JOB = "00000000-0000-7000-8000-000000000073";
const YOGURT = "00000000-0000-7000-8000-000000000041";
const FRIDGE_BATCH = "00000000-0000-7000-8000-000000000053";
const PANTRY_BATCH = "00000000-0000-7000-8000-000000000052";

async function logInAsBob(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

test("a consumption proposal is reviewed, corrected and confirmed into a decrement", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/consume-review.html?storage=${HOUSEHOLD}&job=${CONSUME_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  // Row 0: the matched product, with both its batches offered — nearest
  // expiration first, the AI's suggested count of 1 pre-allocated there.
  const yogurt = rows.nth(0);
  await expect(yogurt).toContainText("Empty Yogurt Pot");
  await expect(yogurt.locator('[data-role="product"]')).toHaveValue(`product:${YOGURT}`);

  const batchRows = yogurt.locator('[data-role="batches"] label');
  await expect(batchRows).toHaveCount(2);
  await expect(batchRows.nth(0)).toContainText("Fridge");
  await expect(batchRows.nth(0)).toContainText("expires 2030-01-01");
  await expect(batchRows.nth(1)).toContainText("Pantry");
  await expect(batchRows.nth(1)).toContainText("no expiry date");

  const fridgeInput = yogurt.locator(`input[data-batch-id="${FRIDGE_BATCH}"]`);
  const pantryInput = yogurt.locator(`input[data-batch-id="${PANTRY_BATCH}"]`);
  await expect(fridgeInput).toHaveValue("1", { timeout: 10_000 });
  await expect(pantryInput).toHaveValue("0");

  // Row 1: unrecognized — the AI's guess matched nothing, so there is no
  // preselected product and no batches yet.
  const mystery = rows.nth(1);
  await expect(mystery.locator('[data-role="unrecognized"]')).toBeVisible();
  await expect(mystery.locator('[data-role="product"]')).toHaveValue("search");

  // Searching finds only existing products — consumption never creates one —
  // and picking it loads that product's own batches, proving the correction
  // path resolves through the same stage-1-only lookup as the AI match.
  await mystery.locator('[data-role="product-input"]').fill("Greek Yogurt");
  await expect(mystery.locator('[data-role="unrecognized"]')).toBeHidden();
  await expect(mystery.locator('[data-role="batches"] label')).toHaveCount(2, { timeout: 10_000 });

  // The reviewer decides this one is not actually there after all.
  await mystery.getByRole("button", { name: "Reject" }).click();
  await expect(mystery).toHaveClass(/review-row--rejected/);

  let sent = null;
  let written = null;
  let status = 0;
  await page.route(`**/consume/photos/${CONSUME_JOB}/confirm`, async (route) => {
    sent = route.request().postDataJSON();
    const response = await route.fetch();
    status = response.status();
    written = await response.json();
    await route.fulfill({ response });
  });
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/inbox\.html/);

  expect(sent.items).toEqual([
    {
      row_id: "0",
      decision: "accept",
      product_id: YOGURT,
      decrements: [{ batch_id: FRIDGE_BATCH, quantity: 1 }],
    },
    { row_id: "1", decision: "reject" },
  ]);

  expect(status).toBe(200);
  expect(written.batch_ids).toEqual([FRIDGE_BATCH]);

  await expect(page).toHaveURL(/\/inbox\.html\?.*consumed=1/);
  await expect(page.locator("#notice")).toHaveText("Proposal applied: 1 batch updated.");
  await expect(page.locator(`[data-job-id="${CONSUME_JOB}"]`)).toHaveCount(0);

  // The fridge batch actually lost one unit; the pantry batch is untouched.
  const batches = await (await page.request.get(`/api/storages/${HOUSEHOLD}/products/${YOGURT}/batches`)).json();
  const fridge = batches.items.find((b) => b.id === FRIDGE_BATCH);
  const pantry = batches.items.find((b) => b.id === PANTRY_BATCH);
  expect(fridge.quantity).toBe(2);
  expect(pantry.quantity).toBe(4);

  // And the job cannot be applied a second time.
  const again = await page.request.post(`/api/storages/${HOUSEHOLD}/consume/photos/${CONSUME_JOB}/confirm`, {
    data: { items: [{ row_id: "0", decision: "reject" }, { row_id: "1", decision: "reject" }] },
  });
  expect(again.status()).toBe(409);
});

test("using-up mode is offered on the camera entry point and is sticky", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const usingUp = page.locator('input[name="mode"][value="using_up"]');
  await expect(usingUp).toBeVisible();
  await usingUp.check();
  // Using up decrements batches that already have a location, so the
  // location field has nothing to offer here.
  await expect(page.locator("#location-field")).toBeHidden();

  await page.reload();
  await expect(page.locator('input[name="mode"][value="using_up"]')).toBeChecked();
});
