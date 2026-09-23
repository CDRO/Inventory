// The batch split/move picker on products.html's stock card
// (docs/specs/06-vision-shelf-ingestion.md, "One batch, one location — and
// how to split one"; docs/specs/28-batch-move-quick-create.md, built for
// #145). Split and move are separate endpoints — a split takes a quantity
// strictly below the source batch's current quantity and creates a new
// batch at the target, carrying the source's expiration_date and
// expiration_source; a move relocates the whole batch in place, same id,
// same quantity.
//
// Every assertion about a quantity or a location below comes from a fresh
// `page.request.get`, taken after the UI action, never from the DOM the
// picker just updated — the point is to prove the picker actually drove the
// real endpoints, not that it can render its own optimistic state.
//
// One product per scenario (e2e/fixtures/seed.sql), the same reasoning
// e2e/specs/barcode-recall.spec.js gives for its own fixtures: these tests
// run fullyParallel (playwright.config.js) and each mutates the batch it
// touches, so sharing one product across scenarios would make the order
// tests happen to run in decide their outcome.

import { test, expect } from "@playwright/test";

const STORAGE_ID = "00000000-0000-7000-8000-000000000010";
const BASE = `/api/storages/${STORAGE_ID}`;

const PANTRY = "00000000-0000-7000-8000-000000000020";
const FRIDGE = "00000000-0000-7000-8000-000000000021";

const SPLIT_PRODUCT = "00000000-0000-7000-8000-000000000080";
const SPLIT_BATCH = "00000000-0000-7000-8000-000000000083";
const MOVE_PRODUCT = "00000000-0000-7000-8000-000000000081";
const MOVE_BATCH = "00000000-0000-7000-8000-000000000084";
const REJECT_PRODUCT = "00000000-0000-7000-8000-000000000082";
const REJECT_BATCH = "00000000-0000-7000-8000-000000000085";

async function logIn(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status(), "fixture login").toBe(200);
}

async function openProduct(page, name) {
  await page.goto(`/products.html?storage=${STORAGE_ID}`);
  await page.getByRole("button", { name, exact: true }).click();
}

function batchRow(page, batchId) {
  return page.locator(`[data-role="batch-row"][data-batch-id="${batchId}"]`);
}

async function fetchProduct(page, productId) {
  const res = await page.request.get(`${BASE}/products/${productId}`);
  expect(res.status()).toBe(200);
  return res.json();
}

test("splitting a batch creates a new one at the target and leaves the total unchanged", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Split Source");

  const row = batchRow(page, SPLIT_BATCH);
  await expect(row).toContainText("5 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await form.locator("input").fill("2");
  await form.locator("select").selectOption(FRIDGE);

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${SPLIT_BATCH}/split`) && res.request().method() === "POST",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(201);

  const product = await fetchProduct(page, SPLIT_PRODUCT);
  expect(product.current_stock).toBe(5); // net stock unchanged by a split

  const source = product.batches.find((b) => b.id === SPLIT_BATCH);
  expect(source).toBeTruthy();
  expect(source.quantity).toBe(3); // 5 - 2

  const created = product.batches.find((b) => b.id !== SPLIT_BATCH);
  expect(created).toBeTruthy();
  expect(created.location_id).toBe(FRIDGE);
  expect(created.quantity).toBe(2);
  // The split jars are the same jars: expiry and its source copy across.
  expect(created.expiration_date).toBe("2031-06-15");
  expect(created.expiration_source).toBe("user");
});

test("moving a whole batch keeps its id and quantity and only changes its location", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Move Source");

  const row = batchRow(page, MOVE_BATCH);
  await expect(row).toContainText("2 × Pantry");
  await row.locator('[data-role="move-toggle"]').click();

  const form = row.locator('[data-role="move-form"]');
  await expect(form).toBeVisible();
  await form.locator("select").selectOption(FRIDGE);

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${MOVE_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(200);

  const product = await fetchProduct(page, MOVE_PRODUCT);
  expect(product.current_stock).toBe(2); // total unchanged by a move

  expect(product.batches).toHaveLength(1);
  const moved = product.batches[0];
  expect(moved.id).toBe(MOVE_BATCH); // same batch, not a new one
  expect(moved.quantity).toBe(2); // unchanged
  expect(moved.location_id).toBe(FRIDGE);
});

test("a split quantity the server rejects shows its message and leaves the batch list unchanged", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Reject Source");

  const row = batchRow(page, REJECT_BATCH);
  await expect(row).toContainText("3 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  // The whole batch's quantity: splitting the whole thing is a move, so the
  // endpoint refuses this rather than the frontend pre-checking it.
  await form.locator("input").fill("3");
  await form.locator("select").selectOption(FRIDGE);

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${REJECT_BATCH}/split`) && res.request().method() === "POST",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(422);

  await expect(row.locator('[role="alert"]')).toContainText("The request could not be processed.");

  const product = await fetchProduct(page, REJECT_PRODUCT);
  expect(product.current_stock).toBe(3);
  expect(product.batches).toHaveLength(1);
  const unchanged = product.batches[0];
  expect(unchanged.id).toBe(REJECT_BATCH);
  expect(unchanged.quantity).toBe(3);
  expect(unchanged.location_id).toBe(PANTRY);
});
