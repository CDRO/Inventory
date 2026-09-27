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
const EMPTY_SUBMIT_MOVE_BATCH = "00000000-0000-7000-8000-0000000000fd";

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

// #224: the split test above proves native validation blocks an empty
// submit, but only for splitQuantity/splitTarget. moveTarget carries the
// same `required` (products.js) yet the move form has no quantity field and,
// more importantly, moveBatch's own no-op-on-same-location guard
// (internal/store/batches.go) means an empty-target submit that *did* reach
// the server would look just like one the guard silently absorbed — a
// request-absence check alone can't tell those two cases apart. Asserting
// moveTarget.validity.valid directly is what actually distinguishes "native
// validation blocked this" from "the server no-op'd it".
test("submitting the move form with no target is blocked by native validation, not a silent no-op", async ({
  page,
}) => {
  await logIn(page);
  await openProduct(page, "E2E Empty Submit Move Source");

  const row = batchRow(page, EMPTY_SUBMIT_MOVE_BATCH);
  await expect(row).toContainText("4 × Pantry");
  await row.locator('[data-role="move-toggle"]').click();

  const form = row.locator('[data-role="move-form"]');
  await expect(form).toBeVisible();

  let requestSeen = false;
  page.on("request", (req) => {
    if (req.url().includes("/inventory-batches/") && req.method() !== "GET") requestSeen = true;
  });

  await form.locator('button[type="submit"]').click();
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
// suggestion-picker half, which was the only one with a route to call when
// they were written. The custom upload is #248's block at the end of this
// file.
//
// What is deliberately NOT asserted here is a picture actually arriving. This
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
const FOREIGN_STORAGE = "00000000-0000-7000-8000-000000000011";

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

  // Same-storage validation: the session IS a member of the storage in the
  // path, and the product simply belongs to a different one. 404, never 403,
  // and never a leak that the product exists elsewhere. The page never
  // reimplements this — it is the handler's, and this is the deployed stack
  // saying so (docs/specs/03-auth-and-multi-tenancy.md).
  expect(res.status()).toBe(404);
  const body = await res.json();
  expect(body.error.code).toBe("not_found");
  expect(body.error.debug_reason).toBeUndefined(); // APP_ENV=prod in this stack
});

// The other half of the same invariant, which the test above does not reach:
// there the storage was the session's own. Here it is not, so this is the
// inaccessible-storage case — and it has to be answered exactly as an
// unknown storage id is, or membership becomes discoverable by probing.
test("the image route answers 404 for a storage the session is not a member of", async ({ page }) => {
  await logIn(page); // e2e-bob, a member of "E2E Household" only

  const inaccessible = await page.request.patch(
    `/api/storages/${FOREIGN_STORAGE}/products/${FOREIGN_PRODUCT}/image`,
    { data: { image: null, icon_name: null } },
  );
  const unknown = await page.request.patch(
    `/api/storages/00000000-0000-7000-8000-0000000000ee/products/${FOREIGN_PRODUCT}/image`,
    { data: { image: null, icon_name: null } },
  );

  expect(inaccessible.status()).toBe(404);
  expect(unknown.status()).toBe(404);
  // Identical either way, body included: anything that differed would say
  // "this storage exists, you just cannot see it".
  expect(await inaccessible.json()).toEqual(await unknown.json());
});

// The picker's own click path, for both of the module's callers. Without a
// provider this stack returns no suggestions at all, so the suggestion list is
// mocked at the browser — the same page.route technique analyze-again.spec.js
// and inbox-discard-all.spec.js use for responses this deployment cannot
// produce on demand. What is NOT mocked is the picker: the buttons, the
// selection state and the hash it extracts from a suggestion URL are the real
// module, which is the part a bad extraction would break.
const SUGGESTION_HASH_A = "b".repeat(64);
const SUGGESTION_HASH_B = "c".repeat(64);

async function mockSuggestions(page, suggestions) {
  // A predicate, not a glob: Playwright treats "?" in a URL pattern as a
  // single-character wildcard, so "**/image-suggestions?*" would not mean
  // "with a query string".
  await page.route(
    (url) => url.pathname.endsWith("/image-suggestions"),
    (route) => route.fulfill({ json: { suggestions } }),
  );
}

test("picking a suggestion sends that suggestion's hash to the image route", async ({ page }) => {
  await logIn(page);
  await mockSuggestions(page, [
    { url: `/api/storages/${STORAGE_ID}/images/${SUGGESTION_HASH_A}`, type: "photo" },
    { url: `/api/storages/${STORAGE_ID}/images/${SUGGESTION_HASH_B}`, type: "icon" },
  ]);

  let sent = null;
  await page.route(`**/products/${PICTURE_PICKER_PRODUCT}/image`, async (route) => {
    sent = route.request().postDataJSON();
    await route.fulfill({ json: { image_url: "/api/storages/x/product-images/p.jpg", icon_name: null } });
  });

  await openProduct(page, "E2E Picture Picker Source");
  await page.locator('[data-role="change-picture"]').click();

  const choices = page.locator('[data-role="picture-suggestion"]');
  await expect(choices).toHaveCount(2);

  // The second one, so a picker that always sends the first would fail here.
  await choices.nth(1).click();
  await expect.poll(() => sent).not.toBeNull();

  // The hash of the picked suggestion, never its URL: a URL would let a
  // caller point this household's product at a server of their choosing.
  expect(sent).toEqual({ image: SUGGESTION_HASH_B, icon_name: null });
  await expect(choices.nth(1)).toHaveAttribute("aria-pressed", "true");
  await expect(choices.nth(0)).toHaveAttribute("aria-pressed", "false");
  await expect(page.locator('[data-role="picture-none"]')).toHaveAttribute("aria-pressed", "false");
});

// The regression test for the round-1 Go review's blocking finding: the clear
// button used to be built only in the branch where suggestions loaded and came
// back non-empty, so a product that already had a picture could not be cleared
// whenever the provider was down or returned nothing — which is this stack's
// own default state, and therefore the likeliest state of a real deployment
// with no SerpAPI key.
for (const [name, fulfil] of [
  ["the provider is unreachable", (route) => route.fulfill({ status: 503, json: { error: { code: "upstream_failed" } } })],
  ["no pictures are found", (route) => route.fulfill({ json: { suggestions: [] } })],
]) {
  test(`a picture can still be removed when ${name}`, async ({ page }) => {
    await logIn(page);
    await page.route((url) => url.pathname.endsWith("/image-suggestions"), fulfil);

    let sent = null;
    await page.route(`**/products/${PICTURE_CLEAR_PRODUCT}/image`, async (route) => {
      sent = route.request().postDataJSON();
      await route.fulfill({ json: { image_url: null, icon_name: null } });
    });

    await openProduct(page, "E2E Picture Clear Source");
    await page.locator('[data-role="change-picture"]').click();

    const clear = page.locator('[data-role="picture-none"]');
    await expect(clear).toBeVisible();
    await clear.click();

    await expect.poll(() => sent).toEqual({ image: null, icon_name: null });
  });
}

// #249 and #252: renderImagePicker's in-flight guard used to be scoped to one
// call of the function, so it stopped a double click within one open picker
// but not a close-and-reopen of the same logical picker. Both regression
// tests below hold a mocked response open and release it on a schedule the
// test controls, rather than racing real timing, which would be flaky in
// either direction — the same technique ingestion.spec.js uses for its own
// overlapping-request races: the #252 test below holds two overlapping
// fetches via a `releases` array, matching "closing the location modal while
// two creates overlap..."; the #249 test holds a single one via one
// `releaseFirst`/`...Held` promise, matching the simpler "closing the
// location modal while its create POST is still in flight...".

// #252: a reopen started before the first invocation's suggestions fetch has
// resolved must not let that stale fetch land its own row beside the
// reopened picker's.
test("reopening the picker while its suggestions fetch is in flight does not duplicate the row", async ({
  page,
}) => {
  await logIn(page);

  // Each suggestions fetch this test triggers is held open independently,
  // released in an order the test controls below.
  const releases = [];
  await page.route((url) => url.pathname.endsWith("/image-suggestions"), async (route) => {
    await new Promise((resolve) => releases.push(resolve));
    await route.fulfill({
      json: {
        suggestions: [{ url: `/api/storages/${STORAGE_ID}/images/${SUGGESTION_HASH_A}`, type: "photo" }],
      },
    });
  });

  await openProduct(page, "E2E Picture Picker Source");

  // Invocation 1 opens the picker; its fetch starts and is held.
  await page.locator('[data-role="change-picture"]').click();
  await expect.poll(() => releases.length).toBe(1);

  // Invocation 2 re-invokes the same picker before invocation 1's fetch has
  // resolved — clearing the container and starting its own held fetch.
  await page.locator('[data-role="change-picture"]').click();
  await expect.poll(() => releases.length).toBe(2);

  // Invocation 1's stale fetch resolves after invocation 2 already owns the
  // container — the exact ordering #252 describes — then invocation 2's own
  // fetch resolves.
  releases[0]();
  releases[1]();

  await expect(page.locator('[data-role="picture-suggestion"]')).toHaveCount(1);
  await expect(page.locator('[data-role="picture-none"]')).toHaveCount(1);
});

// #249: a write started by one invocation must still block a write from a
// later reopen of the same logical picker, not just a second click inside
// the same open picker.
test("a suggestion click after a reopen is refused while an earlier write from the same picker is still in flight", async ({
  page,
}) => {
  await logIn(page);
  await mockSuggestions(page, [
    { url: `/api/storages/${STORAGE_ID}/images/${SUGGESTION_HASH_A}`, type: "photo" },
  ]);

  // The first PATCH this test triggers is held open until released below. A
  // second entry in `sent` would mean the fix let a reopened picker's click
  // start a write while the first one was still outstanding.
  const sent = [];
  let releaseFirst;
  const firstHeld = new Promise((resolve) => {
    releaseFirst = resolve;
  });
  await page.route(`**/products/${PICTURE_PICKER_PRODUCT}/image`, async (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    sent.push(route.request().postDataJSON());
    if (sent.length === 1) await firstHeld;
    await route.fulfill({ json: { image_url: null, icon_name: null } });
  });

  await openProduct(page, "E2E Picture Picker Source");
  await page.locator('[data-role="change-picture"]').click();

  const choice = page.locator('[data-role="picture-suggestion"]');
  await expect(choice).toBeVisible();
  await choice.click(); // the held first PATCH

  await expect.poll(() => sent.length).toBe(1);

  // The "close and reopen mid-write" from #249: re-invoke the same logical
  // picker and click its suggestion again while the first write is still
  // unresolved.
  await page.locator('[data-role="change-picture"]').click();
  await expect(choice).toBeVisible();
  await choice.click();

  // The guard's refusal is synchronous, so this only gives a bug a chance to
  // show up rather than racing the fix.
  await page.waitForTimeout(200);
  expect(sent.length, "the reopened picker's click must be refused, not raced").toBe(1);

  // Let the held write finish so the mocked route's own promise chain settles
  // cleanly rather than leaving it dangling when the test ends. Nothing below
  // this point is a further check of the guard — that was already proven by
  // the assertion above.
  releaseFirst();
  await expect.poll(() => sent.length).toBe(1);
});

// #254: the picker's rollback contract. choose() (js/image-picker.js) catches
// a failed write and undoes the optimistic selection, and setPicture
// (js/pages/products.js) deliberately rethrows so that catch fires — every
// picker journey above this one exercises only a successful PATCH, so nothing
// before this test would notice if the rethrow were removed, the catch made
// to swallow, or the rollback stopped restoring the previous selection.
//
// The PATCH is mocked to fail, so nothing is written and no fixture is
// mutated — the same reasoning the failed-upload test below gives for
// sharing PICTURE_PICKER_PRODUCT rather than taking a dedicated one.
test("a failed write rolls the picker's selection back to its pre-click state", async ({ page }) => {
  await logIn(page);
  await mockSuggestions(page, [
    { url: `/api/storages/${STORAGE_ID}/images/${SUGGESTION_HASH_A}`, type: "photo" },
  ]);
  await page.route(`**/products/${PICTURE_PICKER_PRODUCT}/image`, async (route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    await route.fulfill({
      status: 500,
      json: { error: { code: "internal", message: "The server could not save that picture." } },
    });
  });

  await openProduct(page, "E2E Picture Picker Source");
  await page.locator('[data-role="change-picture"]').click();

  const choice = page.locator('[data-role="picture-suggestion"]');
  await expect(choice).toHaveCount(1);
  await expect(choice).toHaveAttribute("aria-pressed", "false"); // pre-click: nothing chosen yet

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) =>
        res.url().endsWith(`/products/${PICTURE_PICKER_PRODUCT}/image`) &&
        res.request().method() === "PATCH",
    ),
    choice.click(),
  ]);
  expect(response.status()).toBe(500);

  // The caller's own words, through the error catalog — proof the rethrow and
  // the catch actually ran, not just that a button's class changed.
  await expect(page.locator('[data-role="picture-status"]')).toContainText(
    "The server could not save that picture.",
  );

  // Rolled back to the pre-click state: nothing was selected before the
  // click, so nothing is selected after the failed write either.
  await expect(choice).toHaveAttribute("aria-pressed", "false");
  await expect(page.locator('[data-role="picture-none"]')).toHaveAttribute("aria-pressed", "false");

  // And the picture block still shows the server's actual picture — none —
  // rather than the clicked suggestion.
  await expect(page.locator('[data-role="product-picture"]')).toContainText("No picture yet.");

  const product = await fetchProduct(page, PICTURE_PICKER_PRODUCT);
  expect(product.image_url).toBeNull();
  expect(product.icon_name).toBeNull();
});

// --- #248: the custom-upload change path ------------------------------------
//
// Spec 07's other change path: "pick one of the 3, upload a custom photo
// instead (`POST /api/storages/{storage_id}/products/{id}/image` multipart)".
// Unlike the picker's half above, this one needs no provider at all — the photo
// comes from the caller — so these scenarios *can* assert a picture actually
// arriving, and do.
//
// One product per scenario, for the reason the ...fa/...fb block in
// e2e/fixtures/seed.sql gives: both of these write a picture, and this suite
// runs fullyParallel.

const PICTURE_UPLOAD_PRODUCT = "00000000-0000-7000-8000-0000000000ff";
const PICTURE_UPLOAD_UI_PRODUCT = "00000000-0000-7000-8000-000000000100";

// A real, decodable 1x1 PNG — the same fixture ingestion.spec.js uses where an
// image has to survive an actual decode. It has to be a real one here: the
// server reads the format from the magic bytes and re-encodes the pixels
// (internal/httpapi/upload.go), so a stand-in would be refused as a 422.
const TINY_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
  "base64",
);

function photoUpload(buffer) {
  return { multipart: { image: { name: "IMG_0042.png", mimeType: "image/png", buffer } } };
}

test("a custom photo upload becomes the product's picture and is served back", async ({ page }) => {
  await logIn(page);

  const before = await fetchProduct(page, PICTURE_UPLOAD_PRODUCT);
  expect(before.image_url, "the fixture must start out with no picture").toBeNull();

  const res = await page.request.post(
    `${BASE}/products/${PICTURE_UPLOAD_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );
  expect(res.status(), await res.text()).toBe(200);
  const body = await res.json();

  // The address is the server's own, under this storage, and the name is
  // generated — never what the request called the file.
  expect(body.image_url).toMatch(
    new RegExp(`^${BASE}/product-images/[0-9a-f-]+\\.png$`),
  );
  expect(body.image_url).not.toContain("IMG_0042");
  expect(body.icon_name).toBeNull();

  const after = await fetchProduct(page, PICTURE_UPLOAD_PRODUCT);
  expect(after.image_url).toBe(body.image_url);

  // And the file is really there: permanent storage, served back through the
  // storage-scoped picture route rather than merely recorded on the row.
  const served = await page.request.get(body.image_url);
  expect(served.status()).toBe(200);
  expect(served.headers()["content-type"]).toBe("image/png");
});

test("the upload route answers 404 for a product in another storage, not 403", async ({ page }) => {
  await logIn(page);

  const res = await page.request.post(
    `${BASE}/products/${FOREIGN_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );

  // Same-storage validation on the new route too: the session IS a member of
  // the storage in the path, the product simply belongs to a different one
  // (docs/specs/03-auth-and-multi-tenancy.md).
  expect(res.status()).toBe(404);
  const body = await res.json();
  expect(body.error.code).toBe("not_found");
  expect(body.error.debug_reason).toBeUndefined(); // APP_ENV=prod in this stack
});

test("the upload route answers 404 for a storage the session is not a member of", async ({
  page,
}) => {
  await logIn(page); // e2e-bob, a member of "E2E Household" only

  const inaccessible = await page.request.post(
    `/api/storages/${FOREIGN_STORAGE}/products/${FOREIGN_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );
  const unknown = await page.request.post(
    `/api/storages/00000000-0000-7000-8000-0000000000ee/products/${FOREIGN_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );

  expect(inaccessible.status()).toBe(404);
  expect(unknown.status()).toBe(404);
  // Identical either way, body included, or membership becomes discoverable by
  // probing.
  expect(await inaccessible.json()).toEqual(await unknown.json());
});

test("the detail view's upload control posts the chosen photo and shows it", async ({ page }) => {
  await logIn(page);
  await openProduct(page, "E2E Picture Upload UI Source");

  const upload = page.locator('[data-role="picture-upload"]');
  await expect(upload).toBeHidden(); // closed until the change affordance is used

  // The same click that opens the picker reveals the upload control, and
  // reveals it without waiting on the suggestions request — which in this
  // stack has no provider to reach.
  await page.locator('[data-role="change-picture"]').click();
  await expect(upload).toBeVisible();

  const [response] = await Promise.all([
    page.waitForResponse(
      (res) =>
        res.url().endsWith(`/products/${PICTURE_UPLOAD_UI_PRODUCT}/image`) &&
        res.request().method() === "POST",
    ),
    page
      .locator('[data-role="upload-picture"]')
      .setInputFiles({ name: "IMG_0043.png", mimeType: "image/png", buffer: TINY_PNG }),
  ]);
  expect(response.status()).toBe(200);

  // The picture is re-rendered from what the route answered, and the edit form
  // beside it survives — renderPicture redraws the picture alone.
  const picture = page.locator('[data-role="product-picture"] img');
  await expect(picture).toHaveAttribute(
    "src",
    new RegExp(`^${BASE}/product-images/[0-9a-f-]+\\.png$`),
  );
  await expect(page.locator('[data-role="picture-status"]')).toContainText("Picture updated.");
  await expect(page.locator("#p-name")).toHaveValue("E2E Picture Upload UI Source");

  // And the row really changed, read back from the server rather than from the
  // DOM this page just updated.
  const product = await fetchProduct(page, PICTURE_UPLOAD_UI_PRODUCT);
  expect(product.image_url).not.toBeNull();
  expect(product.icon_name).toBeNull();
});

// A product id that is never seeded, in the same spirit as the unknown
// storage id the test above uses: it has to stay unseeded for that test to
// mean anything, so do not give this id a fixture row.
const UNKNOWN_PRODUCT = "00000000-0000-7000-8000-0000000000ed";

// The product-level half of #248's item 2, which the storage-level test above
// does not reach: there the storage varies and the product id is held
// constant, so it proves nothing about telling one product id from another.
// Here the storage is Bob's own in both requests and only the product id
// changes — one that exists in Alice's storage, one that exists nowhere.
//
// What this catches: a `GetProduct` pre-check added ahead of the write that
// answers the two cases differently — a different error code, a 422 for one
// and a 404 for the other, a message that names the product. Any of those
// hands a member of one storage a working oracle for which product ids exist
// in another (docs/specs/03-auth-and-multi-tenancy.md).
test("the upload route cannot tell an unknown product from one in another storage", async ({
  page,
}) => {
  await logIn(page);

  const foreign = await page.request.post(
    `${BASE}/products/${FOREIGN_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );
  const unknown = await page.request.post(
    `${BASE}/products/${UNKNOWN_PRODUCT}/image`,
    photoUpload(TINY_PNG),
  );

  expect(foreign.status()).toBe(404);
  expect(unknown.status()).toBe(404);
  expect(await foreign.json()).toEqual(await unknown.json());
});

// The failure path through the control itself, which the success scenario
// above cannot reach. Both of the guards renderPictureUpload leans on are
// only observable here: the input is cleared *before* the write, and the
// in-flight guard is released whether the write succeeded or not.
//
// The POST is mocked, so nothing is written and no fixture is mutated — which
// is why this shares the picker's product rather than taking one of its own.
// The seed file's one-product-per-scenario rule is about scenarios that write.
test("a failed upload says so and leaves the control ready to try again", async ({ page }) => {
  await logIn(page);

  let attempts = 0;
  await page.route(`**/products/${PICTURE_PICKER_PRODUCT}/image`, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    attempts += 1;
    await route.fulfill({
      status: 413,
      json: { error: { code: "payload_too_large", message: "The uploaded file is too large." } },
    });
  });

  await openProduct(page, "E2E Picture Picker Source");
  await page.locator('[data-role="change-picture"]').click();

  const input = page.locator('[data-role="upload-picture"]');
  await input.setInputFiles({ name: "IMG_0044.png", mimeType: "image/png", buffer: TINY_PNG });

  // The server's own words, through the error catalog rather than a generic
  // "something went wrong" (docs/specs/19-localization.md).
  await expect(page.locator('[data-role="picture-status"]')).toContainText(
    "The uploaded file is too large.",
  );
  // Still no picture: a failed write must not leave the page claiming one.
  await expect(page.locator('[data-role="product-picture"]')).toContainText("No picture yet.");

  // Both guards the control leans on, asserted on what each one actually
  // changes.
  //
  // The clear-before-write, read through `files` rather than `value`:
  // Playwright's setInputFiles never populates a file input's `value`, so
  // `toHaveValue("")` reads "" whether the handler cleared the control or not
  // and would pass against the very regression it is meant to catch. `files`
  // is what `value = ""` empties, and it is what a browser consults when
  // deciding whether re-picking the same file is a change at all.
  await expect.poll(() => input.evaluate((el) => el.files.length)).toBe(0);

  // And the in-flight guard released, so the same file picked a second time
  // really does reach the route a second time. A regression leaving
  // `uploading` or `disabled` stuck true after a rejected request fails here.
  await expect(input).toBeEnabled();
  await input.setInputFiles({ name: "IMG_0044.png", mimeType: "image/png", buffer: TINY_PNG });
  await expect.poll(() => attempts).toBe(2);
});
