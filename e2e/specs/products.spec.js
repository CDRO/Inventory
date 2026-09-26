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
const CROSS_SPLIT_PRODUCT = "00000000-0000-7000-8000-000000000089";
const CROSS_SPLIT_BATCH = "00000000-0000-7000-8000-00000000008b";
const CROSS_MOVE_PRODUCT = "00000000-0000-7000-8000-00000000008a";
const CROSS_MOVE_BATCH = "00000000-0000-7000-8000-00000000008c";
const QUICK_CREATE_PRODUCT = "00000000-0000-7000-8000-000000000090";
const QUICK_CREATE_BATCH = "00000000-0000-7000-8000-000000000091";
const CANCEL_PRODUCT = "00000000-0000-7000-8000-000000000092";
const CANCEL_BATCH = "00000000-0000-7000-8000-000000000093";
const MOVE_QUICK_CREATE_PRODUCT = "00000000-0000-7000-8000-000000000096";
const MOVE_QUICK_CREATE_BATCH = "00000000-0000-7000-8000-000000000097";
const MOVE_CANCEL_BATCH = "00000000-0000-7000-8000-0000000000bc";
const DOUBLE_CLICK_PRODUCT = "00000000-0000-7000-8000-000000000099";
const DOUBLE_CLICK_BATCH = "00000000-0000-7000-8000-00000000009a";
const DOUBLE_CLICK_MOVE_PRODUCT = "00000000-0000-7000-8000-00000000009c";
const DOUBLE_CLICK_MOVE_BATCH = "00000000-0000-7000-8000-00000000009d";
const EMPTY_SUBMIT_BATCH = "00000000-0000-7000-8000-0000000000b4";
const NESTED_LOCATION_BATCH = "00000000-0000-7000-8000-0000000000b8";

// "E2E Other Household" (...011) — Alice's, not Bob's — and its own Garage
// location, used only as a target_location_id/location_id from *another*
// storage. This is never reachable through the picker's own <select>: it is
// built by js/location-options.js's fetchLocations against
// GET /api/storages/{storage_id}/locations, which is scoped to the storage
// in the URL, so the option simply never exists to click. Sent directly here
// to prove the deployed stack still refuses it — the same guarantee
// docs/specs/06-vision-shelf-ingestion.md gives every location id in a
// request, and the one docs/specs/28-batch-move-quick-create.md's picker
// acceptance criteria say this frontend leans on rather than reimplements.
const FOREIGN_LOCATION = "00000000-0000-7000-8000-000000000022";

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

test("a target location from another storage is refused on both split and move, exactly like a nonexistent one", async ({ page }) => {
  await logIn(page);

  // Sent directly against the deployed endpoints — see FOREIGN_LOCATION's
  // comment above for why the picker's own UI cannot produce this request.
  const splitRes = await page.request.post(`${BASE}/inventory-batches/${CROSS_SPLIT_BATCH}/split`, {
    data: { quantity: 1, target_location_id: FOREIGN_LOCATION },
  });
  expect(splitRes.status()).toBe(404);
  expect((await splitRes.json()).error.code).toBe("not_found");

  const moveRes = await page.request.patch(`${BASE}/inventory-batches/${CROSS_MOVE_BATCH}`, {
    data: { location_id: FOREIGN_LOCATION },
  });
  expect(moveRes.status()).toBe(404);
  expect((await moveRes.json()).error.code).toBe("not_found");

  // Neither refusal wrote anything.
  const splitProduct = await fetchProduct(page, CROSS_SPLIT_PRODUCT);
  expect(splitProduct.batches).toHaveLength(1);
  expect(splitProduct.batches[0].quantity).toBe(5);

  const moveProduct = await fetchProduct(page, CROSS_MOVE_PRODUCT);
  expect(moveProduct.batches[0].location_id).toBe(PANTRY);
  expect(moveProduct.batches[0].quantity).toBe(2);
});

// docs/specs/28-batch-move-quick-create.md (#107, built for #145's picker
// above): a location that doesn't exist yet is created without leaving
// products.html, and the split then completes against it. This is also
// where the spec's two picker-wide acceptance criteria live — one GET per
// modal close, and no third location-creation path beyond the one 06 and 26
// already use — so both are asserted here rather than in a separate test.
test("a location can be created from the split picker, without leaving products.html, and the split completes against it", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Quick-Create Source");

  const row = batchRow(page, QUICK_CREATE_BATCH);
  await expect(row).toContainText("6 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await form.locator("input").fill("2");

  await form.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Pantry"); // the existing tree, not an empty one

  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/api/storages/${STORAGE_ID}/locations`) && res.request().method() === "POST",
    ),
    (async () => {
      await dialog.getByRole("button", { name: "Add top-level location" }).click();
      await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Quick Create Shelf");
      await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
    })(),
  ]);
  // The same POST /api/storages/{storage_id}/locations 06 and 26 already use
  // — no third creation path.
  expect(createResponse.status()).toBe(201);
  await expect(dialog).toContainText("Quick Create Shelf");

  // Closing the modal must refresh the split form's own target field from
  // exactly one GET.
  let getCalls = 0;
  await page.route(`**/api/storages/${STORAGE_ID}/locations`, async (route) => {
    if (route.request().method() === "GET") getCalls++;
    await route.continue();
  });
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();
  expect(getCalls).toBe(1);
  await page.unroute(`**/api/storages/${STORAGE_ID}/locations`);

  // Preselected automatically — the split's quantity, entered before the
  // modal ever opened, survived the refresh untouched.
  const targetSelect = form.locator("select");
  await expect(targetSelect.locator("option:checked")).toContainText("Quick Create Shelf");
  await expect(form.locator("input")).toHaveValue("2");

  // Read before the submit below, not after: confirming the split rebuilds
  // the batch list, and with it this form, so a read afterwards races the
  // re-render and comes back empty whenever the rebuild wins.
  const newLocationId = await targetSelect.inputValue();

  const [splitResponse] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${QUICK_CREATE_BATCH}/split`) && res.request().method() === "POST",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(splitResponse.status()).toBe(201);
  const product = await fetchProduct(page, QUICK_CREATE_PRODUCT);
  expect(product.current_stock).toBe(6); // net stock unchanged by a split

  const source = product.batches.find((b) => b.id === QUICK_CREATE_BATCH);
  expect(source.quantity).toBe(4); // 6 - 2

  const created = product.batches.find((b) => b.id !== QUICK_CREATE_BATCH);
  expect(created).toBeTruthy();
  expect(created.location_id).toBe(newLocationId);
  expect(created.quantity).toBe(2);
});

// docs/specs/28-batch-move-quick-create.md's cancel criterion: closing the
// modal without creating anything leaves the split form exactly as the user
// left it. Non-default values in both fields (a typed quantity, a real
// target location rather than the placeholder) — a placeholder-versus-
// placeholder comparison could never fail either way.
test("cancelling the location modal from the split picker leaves the in-progress quantity and selected location untouched", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Quick-Create Cancel Source");

  const row = batchRow(page, CANCEL_BATCH);
  await expect(row).toContainText("4 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await form.locator("input").fill("3");
  await form.locator("select").selectOption(FRIDGE);

  // Esc is the browser's own dialog cancel; the backdrop click lands on the
  // ::backdrop well outside the centred dialog box, exercising tree-modal.js's
  // own backdrop handler rather than a click on the dialog's padding
  // (matching e2e/specs/ingestion.spec.js's identical two-dismissal check).
  for (const dismiss of [() => page.keyboard.press("Escape"), () => page.mouse.click(5, 5)]) {
    await form.locator('[data-role="location-add"]').click();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeVisible();
    await dismiss();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeHidden();
    await expect(form.locator("input")).toHaveValue("3");
    await expect(form.locator("select")).toHaveValue(FRIDGE);
  }
});

// Same cancel criterion as above, against the *move* form's own trigger.
// splitLocationAdd and moveLocationAdd (web/static/js/pages/products.js) are
// two independent closures, each with its own openedSelect — the split
// form's cancel wiring proven above says nothing about the move form's own.
// The move form has no quantity field, so the only in-progress state to
// assert is the selected location; it still has to be a non-default value
// (a real target rather than the placeholder), same reasoning as above.
// Deliberately its own dedicated batch (MOVE_CANCEL_BATCH) rather than
// MOVE_QUICK_CREATE_BATCH: that fixture is read and mutated by the
// create-then-complete-the-move test below, and this test runs
// fullyParallel with it.
test("cancelling the location modal from the move picker leaves the in-progress selected location untouched", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Quick-Create Move Cancel Source");

  const row = batchRow(page, MOVE_CANCEL_BATCH);
  await expect(row).toContainText("7 × Pantry");
  await row.locator('[data-role="move-toggle"]').click();

  const form = row.locator('[data-role="move-form"]');
  await expect(form).toBeVisible();
  await form.locator("select").selectOption(FRIDGE);

  for (const dismiss of [() => page.keyboard.press("Escape"), () => page.mouse.click(5, 5)]) {
    await form.locator('[data-role="location-add"]').click();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeVisible();
    await dismiss();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeHidden();
    await expect(form.locator("select")).toHaveValue(FRIDGE);
  }
});

// The split and move forms wire their own "+ New location" trigger
// independently in products.js (each its own openedSelect, pushed into the
// same shared locationSelects array) — a test of the split form's trigger
// (above) says nothing about whether the move form's is wired correctly,
// so this covers the move form on its own dedicated product/batch.
test("a location can be created from the move picker, without leaving products.html, and the move completes against it", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Quick-Create Move Source");

  const row = batchRow(page, MOVE_QUICK_CREATE_BATCH);
  await expect(row).toContainText("3 × Pantry");
  await row.locator('[data-role="move-toggle"]').click();

  const form = row.locator('[data-role="move-form"]');
  await expect(form).toBeVisible();

  await form.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Quick Create Move Shelf");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Quick Create Move Shelf");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Preselected in the move form's own select, not merely appended somewhere
  // — proof the trigger's openedSelect points at moveTarget, not splitTarget.
  const targetSelect = form.locator("select");
  await expect(targetSelect.locator("option:checked")).toContainText("Quick Create Move Shelf");

  // Read before the submit below, not after: confirming the move rebuilds
  // the batch list, and with it this form, so a read afterwards races the
  // re-render and comes back empty whenever the rebuild wins.
  const newLocationId = await targetSelect.inputValue();

  const [moveResponse] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${MOVE_QUICK_CREATE_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.locator('button[type="submit"]').click(),
  ]);
  expect(moveResponse.status()).toBe(200);
  const product = await fetchProduct(page, MOVE_QUICK_CREATE_PRODUCT);
  expect(product.batches).toHaveLength(1);
  const moved = product.batches[0];
  expect(moved.id).toBe(MOVE_QUICK_CREATE_BATCH); // same batch, not a new one
  expect(moved.quantity).toBe(3); // unchanged
  expect(moved.location_id).toBe(newLocationId);
});

// #161: two quick clicks on Split could each pass the server's own
// per-request validation and stack silently — splitting 2 off a batch of 5
// twice legally succeeds both times, leaving the source at 1 and two new
// batches instead of the one the user intended. products.js now disables the
// submit button for the duration of the request, the same pattern
// openLocationField already uses in js/location-options.js.
//
// The button's own `disabled` state is what stops a second dispatched click
// from doing anything — a disabled button does not fire click, so calling
// .click() on it twice back to back (synchronously, in the page) is a
// faithful stand-in for two real clicks landing before the first request
// settles: the second call lands after the handler has already set
// `disabled = true` but well before the network response (delayed below)
// lets the button through `finally` again.
test("a second click on Split while the first request is in flight creates only one new batch", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Double-Click Split Source");

  const row = batchRow(page, DOUBLE_CLICK_BATCH);
  await expect(row).toContainText("5 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();
  await form.locator("input").fill("2");
  await form.locator("select").selectOption(FRIDGE);

  let splitRequests = 0;
  await page.route(`**/inventory-batches/${DOUBLE_CLICK_BATCH}/split`, async (route) => {
    splitRequests++;
    await new Promise((resolve) => setTimeout(resolve, 300));
    await route.continue();
  });

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${DOUBLE_CLICK_BATCH}/split`) && res.request().method() === "POST",
    ),
    form.evaluate((formEl) => {
      const button = formEl.querySelector('button[type="submit"]');
      button.click();
      button.click(); // the double-click: a no-op once the first click disabled the button
    }),
  ]);
  expect(response.status()).toBe(201);
  await page.unroute(`**/inventory-batches/${DOUBLE_CLICK_BATCH}/split`);

  expect(splitRequests).toBe(1);

  const product = await fetchProduct(page, DOUBLE_CLICK_PRODUCT);
  expect(product.current_stock).toBe(5); // net stock unchanged by a split
  expect(product.batches).toHaveLength(2); // source + one split, not two splits

  const source = product.batches.find((b) => b.id === DOUBLE_CLICK_BATCH);
  expect(source).toBeTruthy();
  expect(source.quantity).toBe(3); // 5 - 2, not 5 - 2 - 2
});

// The move form's own guard (products.js, same disable/finally pattern as
// the split form above) — a test of the split submit handler says nothing
// about whether the move submit handler is guarded too, since each wires its
// own button independently.
test("a second click on Move while the first request is in flight sends only one PATCH", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Double-Click Move Source");

  const row = batchRow(page, DOUBLE_CLICK_MOVE_BATCH);
  await expect(row).toContainText("3 × Pantry");
  await row.locator('[data-role="move-toggle"]').click();

  const form = row.locator('[data-role="move-form"]');
  await expect(form).toBeVisible();
  await form.locator("select").selectOption(FRIDGE);

  let moveRequests = 0;
  await page.route(`**/inventory-batches/${DOUBLE_CLICK_MOVE_BATCH}`, async (route) => {
    if (route.request().method() === "PATCH") {
      moveRequests++;
      await new Promise((resolve) => setTimeout(resolve, 300));
    }
    await route.continue();
  });

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().endsWith(`/inventory-batches/${DOUBLE_CLICK_MOVE_BATCH}`) && res.request().method() === "PATCH",
    ),
    form.evaluate((formEl) => {
      const button = formEl.querySelector('button[type="submit"]');
      button.click();
      button.click(); // the double-click: a no-op once the first click disabled the button
    }),
  ]);
  expect(response.status()).toBe(200);
  await page.unroute(`**/inventory-batches/${DOUBLE_CLICK_MOVE_BATCH}`);

  expect(moveRequests).toBe(1);

  const product = await fetchProduct(page, DOUBLE_CLICK_MOVE_PRODUCT);
  expect(product.batches).toHaveLength(1);
  const moved = product.batches[0];
  expect(moved.id).toBe(DOUBLE_CLICK_MOVE_BATCH); // same batch, not a new one
  expect(moved.quantity).toBe(3); // unchanged
  expect(moved.location_id).toBe(FRIDGE);
});

// #161 items 2/3: splitQuantity/splitTarget/moveTarget now carry `required`,
// so an empty submit is refused by the browser's own native validation
// instead of the handler's own guard silently returning with no feedback.
// No request of any kind should reach the server — this only proves the
// browser blocks the submit event before products.js's handler ever runs.
test("submitting the split form with an empty quantity and no target is blocked by native validation, not a silent no-op", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Empty Submit Source");

  const row = batchRow(page, EMPTY_SUBMIT_BATCH);
  await expect(row).toContainText("4 × Pantry");
  await row.locator('[data-role="split-toggle"]').click();

  const form = row.locator('[data-role="split-form"]');
  await expect(form).toBeVisible();

  let requestSeen = false;
  page.on("request", (req) => {
    if (req.url().includes("/inventory-batches/") && req.method() !== "GET") requestSeen = true;
  });

  await form.locator('button[type="submit"]').click();
  expect(await form.locator("input").evaluate((el) => el.validity.valid)).toBe(false);
  expect(await form.locator("select").evaluate((el) => el.validity.valid)).toBe(false);
  await expect(form).toBeVisible(); // still open — the submit never went through

  expect(requestSeen).toBe(false);
});

// #174 item 2: locationPathFor joins a batch's full ancestor path with " › ",
// but every other batch row in this file sits at root-level Pantry or Fridge
// — a bug in the join order (child-first instead of root-first) or in the
// separator would pass every other test here. "Door Bin" is two levels below
// Fridge (e2e/fixtures/seed.sql), so this is the one row that can catch it.
test("a batch two locations deep renders its full path, root first", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Nested Location Source");

  const row = batchRow(page, NESTED_LOCATION_BATCH);
  await expect(row).toContainText("6 × Fridge › Door Bin");
});

// --- #135: the picture-change path ------------------------------------------
//
// docs/specs/16-product-maintenance.md describes the detail view as showing
// the image "with the change paths from 07 — suggestion picker, custom
// upload". Spec 16 shipped the edit surface without them; these cover the
// half that has a route to call.
//
// What is deliberately NOT asserted is a picture actually arriving. This
// stack configures no SerpAPI key and no Iconify reachability
// (docker-compose.e2e.yml: "external services are not stubbed here yet"), so
// the suggestion list is empty or the provider is unreachable, and pinning a
// specific one of the picker's three terminal states would pin this stack's
// provider configuration rather than the wiring. What these prove is the
// wiring itself: the button reaches the suggestions endpoint, and the PATCH
// behind the picker's own choices does what the picker asks of it.

const PICTURE_PICKER_PRODUCT = "00000000-0000-7000-8000-0000000000fa";
const PICTURE_CLEAR_PRODUCT = "00000000-0000-7000-8000-0000000000fb";

// A product of Alice's "E2E Other Household" (...011), used only as a product
// id from *another* storage. Bob is not a member there, so the image route
// must answer exactly as it does for an id that does not exist at all.
const FOREIGN_PRODUCT = "00000000-0000-7000-8000-000000000042";

test("the product detail view offers a picture change that reaches the suggestions endpoint", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Picture Picker Source");

  const picker = page.locator('[data-role="picture-picker"]');
  await expect(picker).toBeHidden(); // closed until asked for

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) => res.url().includes("/image-suggestions?query=") && res.request().method() === "GET",
    ),
    page.locator('[data-role="change-picture"]').click(),
  ]);

  // The query is the product's own name, and it is the *storage-scoped* route
  // that answers — the scoping the server does, not something re-derived here.
  expect(decodeURIComponent(new URL(response.url()).search)).toContain("query=E2E Picture Picker Source");
  expect(new URL(response.url()).pathname).toBe(`/api/storages/${STORAGE_ID}/image-suggestions`);

  await expect(picker).toBeVisible();
});

test("clearing a product's picture goes through the image route and shows on the page", async ({
  page,
}) => {
  await logIn(page);

  const before = await fetchProduct(page, PICTURE_CLEAR_PRODUCT);
  expect(before.icon_name).toBe("noto:cheese-wedge"); // the fixture must start out set

  // Exactly the body the picker's "No picture" button sends.
  const res = await page.request.patch(`${BASE}/products/${PICTURE_CLEAR_PRODUCT}/image`, {
    data: { image: null, icon_name: null },
  });
  expect(res.status()).toBe(200);

  const after = await fetchProduct(page, PICTURE_CLEAR_PRODUCT);
  expect(after.icon_name).toBeNull();

  await openProduct(page, "E2E Picture Clear Source");
  await expect(page.locator('[data-role="product-picture"]')).toContainText("No picture yet.");
});

test("the image route answers 404 for a product in another storage, not 403", async ({ page }) => {
  await logIn(page);

  const res = await page.request.patch(`${BASE}/products/${FOREIGN_PRODUCT}/image`, {
    data: { image: null, icon_name: null },
  });

  // 404, never 403: an inaccessible product must be indistinguishable from one
  // that does not exist (docs/specs/03-auth-and-multi-tenancy.md). The page
  // never reimplements this — it is the handler's, and this is the deployed
  // stack saying so.
  expect(res.status()).toBe(404);
  const body = await res.json();
  expect(body.error.code).toBe("not_found");
  expect(body.error.debug_reason).toBeUndefined(); // APP_ENV=prod in this stack
});
