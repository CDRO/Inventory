// Batch containers on products.html's stock card
// (docs/specs/39-batch-containers.md): what a batch is physically held in,
// independent of where it sits.
//
// Every assertion about a container comes from a fresh `page.request.get` of
// the product, taken after the UI action, never from the DOM the batch list
// just re-rendered — the point is to prove the form drove the real endpoints,
// not that it can render its own optimistic state.
//
// One product per scenario (e2e/fixtures/seed.sql), the same reasoning
// e2e/specs/products.spec.js gives for its own fixtures: these run
// fullyParallel (playwright.config.js) and each mutates the container it
// touches, so sharing one product would make the order tests happen to run in
// decide their outcome.

import { test, expect } from "@playwright/test";

import { fixtureLogin } from "../support/fixture-login.js";

const STORAGE_ID = "00000000-0000-7000-8000-000000000010";
const BASE = `/api/storages/${STORAGE_ID}`;

const PANTRY = "00000000-0000-7000-8000-000000000020";
const FRIDGE = "00000000-0000-7000-8000-000000000021";

const LABEL_PRODUCT = "00000000-0000-7000-8000-000000000102";
const LABEL_BATCH = "00000000-0000-7000-8000-000000000110";
const RENAME_PRODUCT = "00000000-0000-7000-8000-000000000103";
const RENAME_BATCH = "00000000-0000-7000-8000-000000000111";
const RENAME_CONTAINER = "00000000-0000-7000-8000-000000000120";
const CLEAR_PRODUCT = "00000000-0000-7000-8000-000000000104";
const CLEAR_BATCH = "00000000-0000-7000-8000-000000000112";
const CLEAR_CONTAINER = "00000000-0000-7000-8000-000000000121";
const DESTROY_PRODUCT = "00000000-0000-7000-8000-000000000105";
const DESTROY_BATCH = "00000000-0000-7000-8000-000000000113";
const DESTROY_CONTAINER = "00000000-0000-7000-8000-000000000122";
const PACK_PRODUCT = "00000000-0000-7000-8000-000000000106";
const PACK_BATCH = "00000000-0000-7000-8000-000000000114";
const PACK_CONTAINER = "00000000-0000-7000-8000-000000000123";
const TARGET_PRODUCT = "00000000-0000-7000-8000-000000000107";
const TARGET_BATCH = "00000000-0000-7000-8000-000000000115";
const TARGET_CONTAINER = "00000000-0000-7000-8000-000000000124";
const BOTH_PRODUCT = "00000000-0000-7000-8000-000000000108";
const BOTH_BATCH = "00000000-0000-7000-8000-000000000116";
const BOTH_CONTAINER = "00000000-0000-7000-8000-000000000125";
const NEITHER_PRODUCT = "00000000-0000-7000-8000-000000000109";
const NEITHER_BATCH = "00000000-0000-7000-8000-000000000117";
const SPLIT_DESTROY_PRODUCT = "00000000-0000-7000-8000-00000000010a";
const SPLIT_DESTROY_BATCH = "00000000-0000-7000-8000-000000000118";
const SPLIT_DESTROY_CONTAINER = "00000000-0000-7000-8000-000000000127";
const FREE_SPLIT_PRODUCT = "00000000-0000-7000-8000-00000000010b";
const FREE_SPLIT_BATCH = "00000000-0000-7000-8000-000000000119";

// A container in "E2E Other Household" (...011) — Alice's, not Bob's. It is
// never reachable through this page: the destroy action is only rendered for a
// container the batch in front of you already holds. Sent directly here to
// prove the deployed stack refuses it, with the identical 404 an unknown id
// gets — this is the one container operation whose id comes straight from the
// URL (docs/specs/39-batch-containers.md, "Acceptance criteria").
const FOREIGN_CONTAINER = "00000000-0000-7000-8000-00000000012c";

async function logIn(page) {
  await fixtureLogin(page, "e2e-bob");
}

// The list defaults to filter-only (docs/specs/16-product-maintenance.md).
//
// `#nav` starts `hidden` and only turns visible once products.js's init()
// has resolved `/api/me` — the same synchronous stretch that, right
// afterward, attaches the `#filter` "input" listener
// (web/static/js/pages/products.js). Under parallel load that resolution can
// lag behind page.goto()'s own load event, so filling `#filter` first can
// fire into a page with no listener yet and the debounced search that would
// render the row never starts (#423). Waiting for `#nav` here is the same
// "page is ready" idiom auth-journeys.spec.js and start-page.spec.js already
// rely on; the `toHaveCount(1)` wait, rather than clicking straight off
// `fill`, separately covers the 250ms search debounce itself
// (products.js's SEARCH_DEBOUNCE_MS) under load.
async function openProduct(page, name) {
  await page.goto(`/products.html?storage=${STORAGE_ID}`);
  await expect(page.locator("#nav")).toBeVisible();
  await page.locator("#filter").fill(name);
  const link = page.getByRole("link", { name, exact: true });
  await expect(link).toHaveCount(1);
  await link.click();
}

function batchRow(page, batchId) {
  return page.locator(`[data-role="batch-row"][data-batch-id="${batchId}"]`);
}

async function fetchProduct(page, productId) {
  const res = await page.request.get(`${BASE}/products/${productId}`);
  expect(res.status()).toBe(200);
  return res.json();
}

function batchOf(product, batchId) {
  const batch = product.batches.find((b) => b.id === batchId);
  expect(batch, `batch ${batchId} is still there`).toBeTruthy();
  return batch;
}

// openContainerForm opens one batch row's container form and returns it.
async function openContainerForm(page, batchId) {
  const row = batchRow(page, batchId);
  await row.locator('[data-role="container-toggle"]').click();
  const form = row.locator('[data-role="container-form"]');
  await expect(form).toBeVisible();
  return form;
}

// splitWithDisposition drives one batch row's split form, picking a
// container_disposition radio, and waits for the real POST.
async function splitWithDisposition(page, batchId, quantity, disposition) {
  const row = batchRow(page, batchId);
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await form.locator('input[data-field="quantity"]').fill(String(quantity));
  await form.locator("select").selectOption(FRIDGE);

  const radios = form.locator('[data-role="container-disposition"] input[type="radio"]');
  await expect(radios).toHaveCount(5);
  await form.locator(`[data-role="container-disposition"] input[value="${disposition}"]`).check();

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${batchId}/split`) && res.request().method() === "POST",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(201);
  return response;
}

test("labelling a batch creates a container and attaches it", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Label Source");

  const before = await fetchProduct(page, LABEL_PRODUCT);
  expect(batchOf(before, LABEL_BATCH).container_id).toBeNull();

  const form = await openContainerForm(page, LABEL_BATCH);
  await form.locator('input[data-field="container-label"]').fill("Rice bag");
  await form.locator('input[data-field="container-type"]').fill("bag");

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${LABEL_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(200);

  const after = await fetchProduct(page, LABEL_PRODUCT);
  const batch = batchOf(after, LABEL_BATCH);
  expect(batch.container_id).not.toBeNull();
  expect(batch.container_label).toBe("Rice bag");
  expect(batch.container_type).toBe("bag");
  expect(batch.quantity).toBe(6); // a container says nothing about a quantity

  // And the label is on the row itself, not only in the response.
  await expect(batchRow(page, LABEL_BATCH).locator('[data-role="container-label"]')).toContainText("Rice bag");
});

test("renaming a container keeps the same container row", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Rename Source");

  const form = await openContainerForm(page, RENAME_BATCH);
  const labelField = form.locator('input[data-field="container-label"]');
  // The form opens pre-filled with what the batch is actually in, so a rename
  // is an edit rather than a re-type from memory.
  await expect(labelField).toHaveValue("E2E Old Bag");
  await labelField.fill("E2E New Bag");

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${RENAME_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(response.status()).toBe(200);

  const after = await fetchProduct(page, RENAME_PRODUCT);
  const batch = batchOf(after, RENAME_BATCH);
  expect(batch.container_label).toBe("E2E New Bag");
  expect(batch.container_id).toBe(RENAME_CONTAINER);
});

test("clearing a container detaches the batch without destroying the container", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Clear Source");

  const form = await openContainerForm(page, CLEAR_BATCH);
  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${CLEAR_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.locator('[data-role="container-clear"]').click(),
  ]);
  expect(response.status()).toBe(200);

  const after = await fetchProduct(page, CLEAR_PRODUCT);
  const batch = batchOf(after, CLEAR_BATCH);
  expect(batch.container_id).toBeNull();
  expect(batch.container_label).toBeNull();
  expect(batch.quantity).toBe(6); // detaching moves no stock

  // The container row itself survived: destroying it now succeeds, which a row
  // the detach had deleted could not. This is the only way the API lets a
  // client observe an unreferenced container at all, and it is exactly the
  // distinction docs/specs/39-batch-containers.md draws between "I mislabelled
  // this batch" and "the box is gone".
  const destroy = await page.request.post(`${BASE}/containers/${CLEAR_CONTAINER}/destroy`);
  expect(destroy.status()).toBe(204);
});

test("destroying a container clears it off the batch and cannot be done twice", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Destroy Source");

  const form = await openContainerForm(page, DESTROY_BATCH);
  page.once("dialog", (dialog) => {
    // Confirmed because it is not undoable and clears every batch that held
    // it, which the message has to say (docs/specs/39-batch-containers.md).
    expect(dialog.message()).toContain("E2E Crate To Bin");
    dialog.accept();
  });

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/containers/${DESTROY_CONTAINER}/destroy`) && res.request().method() === "POST",
    ),
    form.locator('[data-role="container-destroy"]').click(),
  ]);
  expect(response.status()).toBe(204);

  const after = await fetchProduct(page, DESTROY_PRODUCT);
  const batch = batchOf(after, DESTROY_BATCH);
  expect(batch.container_id).toBeNull();
  expect(batch.quantity).toBe(6); // destroying a container destroys no stock

  // Destroying is not idempotent: the second call is "already gone", which is
  // the same answer an id that never existed gets.
  const again = await page.request.post(`${BASE}/containers/${DESTROY_CONTAINER}/destroy`);
  expect(again.status()).toBe(404);
});

test("a container from another storage is the same 404 an unknown id gets", async ({ page }) => {
  await logIn(page);

  const foreign = await page.request.post(`${BASE}/containers/${FOREIGN_CONTAINER}/destroy`);
  const unknown = await page.request.post(`${BASE}/containers/00000000-0000-7000-8000-0000000009ff/destroy`);

  expect(foreign.status()).toBe(404);
  expect(unknown.status()).toBe(404);
  expect(await foreign.text()).toBe(await unknown.text());

  // And the refusal is a refusal: the other household's container is still
  // there to be destroyed by somebody who may.
  await fixtureLogin(page, "e2e-alice");
  const allowed = await page.request.post(
    `/api/storages/00000000-0000-7000-8000-000000000011/containers/${FOREIGN_CONTAINER}/destroy`,
  );
  expect(allowed.status()).toBe(204);
});

// The shape docs/specs/39-batch-containers.md exists for, driven through the
// page: 24 in the pack, two carried to the fridge, 22 left in the pack, and the
// two in the fridge in nothing at all.
test("splitting two out of a 24-pack leaves 22 in the pack and 2 in nothing", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Pack Source");

  await expect(batchRow(page, PACK_BATCH).locator('[data-role="container-label"]')).toContainText("E2E 24-pack Box");
  await splitWithDisposition(page, PACK_BATCH, 2, "source");

  const after = await fetchProduct(page, PACK_PRODUCT);
  expect(after.current_stock).toBe(24); // a split moves stock, it does not create it

  const pack = batchOf(after, PACK_BATCH);
  expect(pack.quantity).toBe(22);
  expect(pack.container_id).toBe(PACK_CONTAINER);
  expect(pack.container_label).toBe("E2E 24-pack Box");

  const carried = after.batches.find((b) => b.id !== PACK_BATCH);
  expect(carried).toBeTruthy();
  expect(carried.quantity).toBe(2);
  expect(carried.location_id).toBe(FRIDGE);
  expect(carried.container_id).toBeNull();
});

test("container_disposition 'target' hands the container to the split-off amount", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Target Source");

  await splitWithDisposition(page, TARGET_BATCH, 4, "target");

  const after = await fetchProduct(page, TARGET_PRODUCT);
  expect(batchOf(after, TARGET_BATCH).container_id).toBeNull();

  const carried = after.batches.find((b) => b.id !== TARGET_BATCH);
  expect(carried.container_id).toBe(TARGET_CONTAINER);
  expect(carried.container_label).toBe("E2E Cooler Bag");
  expect(carried.container_type).toBe("bag");
});

test("container_disposition 'both' points both batches at the same container", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Both Source");

  await splitWithDisposition(page, BOTH_BATCH, 4, "both");

  const after = await fetchProduct(page, BOTH_PRODUCT);
  const source = batchOf(after, BOTH_BATCH);
  const carried = after.batches.find((b) => b.id !== BOTH_BATCH);

  expect(source.container_id).toBe(BOTH_CONTAINER);
  expect(carried.container_id).toBe(BOTH_CONTAINER);

  // And a destroy then clears both, not just the row the action was opened on
  // — the case the confirmation has to be able to describe.
  const destroyed = await page.request.post(`${BASE}/containers/${BOTH_CONTAINER}/destroy`);
  expect(destroyed.status()).toBe(204);

  const cleared = await fetchProduct(page, BOTH_PRODUCT);
  for (const batch of cleared.batches) {
    expect(batch.container_id).toBeNull();
  }
  expect(cleared.current_stock).toBe(10); // and no stock moved
});

test("container_disposition 'neither' leaves both batches without a container", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Neither Source");

  await splitWithDisposition(page, NEITHER_BATCH, 4, "neither");

  const after = await fetchProduct(page, NEITHER_PRODUCT);
  for (const batch of after.batches) {
    expect(batch.container_id).toBeNull();
  }
  expect(after.current_stock).toBe(10);
});

test("container_disposition 'destroy' destroys the container in the split itself", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Destroy Split Source");

  await splitWithDisposition(page, SPLIT_DESTROY_BATCH, 4, "destroy");

  const after = await fetchProduct(page, SPLIT_DESTROY_PRODUCT);
  for (const batch of after.batches) {
    expect(batch.container_id).toBeNull();
  }
  expect(after.current_stock).toBe(10);
  expect(batchOf(after, SPLIT_DESTROY_BATCH).quantity).toBe(6);

  // The container really was destroyed by the split, not merely detached from
  // both batches: destroying it again is the "already gone" 404.
  const again = await page.request.post(`${BASE}/containers/${SPLIT_DESTROY_CONTAINER}/destroy`);
  expect(again.status()).toBe(404);
});

// The radios exist only when there is a container to decide about: on a
// container-less batch the split form asks five questions about nothing
// (docs/specs/39-batch-containers.md, "Product detail UI").
test("the split form offers no container choice for a batch in nothing", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Container Free Split Source");

  const row = batchRow(page, FREE_SPLIT_BATCH);
  await expect(row.locator('[data-role="container-label"]')).toHaveCount(0);
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await expect(form.locator('[data-role="container-disposition"]')).toHaveCount(0);

  // The batch's location stays PANTRY until something actually splits it.
  const product = await fetchProduct(page, FREE_SPLIT_PRODUCT);
  expect(batchOf(product, FREE_SPLIT_BATCH).location_id).toBe(PANTRY);
});
