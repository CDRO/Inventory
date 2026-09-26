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
const LOCATION_MODAL_JOB = "00000000-0000-7000-8000-000000000074";
const ZERO_LOCATIONS_JOB = "00000000-0000-7000-8000-000000000075";
const ZERO_LOCATIONS_HOUSEHOLD = "00000000-0000-7000-8000-000000000012"; // "E2E Admin Household"
const TOMATOES = "00000000-0000-7000-8000-000000000040";
const PANTRY = "00000000-0000-7000-8000-000000000020";
const CANNED_GOODS = "00000000-0000-7000-8000-000000000030";

// Stand-in photo bytes. The upload is refused on the model check, which runs
// before the body is read, so these never need to be a decodable image.
const TINY_JPEG = Buffer.from(
  "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=",
  "base64",
);

// A real, decodable 1×1 PNG, served as a job's photo where the review screen
// draws its crops from it.
const TINY_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
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
  // This job has no photo, so there is nothing to take a picture from and the
  // choice is not offered at all.
  await expect(oatMilk.locator('[data-role="new-product-image-field"]')).toBeHidden();
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

  // Row 1 creates a product ("Oat Milk 1L") with no barcode, so the
  // capture-time offer of docs/specs/20-barcode-recall.md appears between the
  // confirm and the navigation. Declining it is one tap and writes nothing —
  // e2e/specs/barcode-recall.spec.js asserts that offer's own contract; here
  // it is dismissed so this journey goes on testing the confirm.
  const barcodeOffer = page.locator("dialog[aria-labelledby='barcode-offer-title']");
  await expect(barcodeOffer).toBeVisible();
  await barcodeOffer.getByRole("button", { name: "Not this time" }).click();

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
  // Resolved --color-success (#15803d), not the --color-danger red every
  // #error box renders in: a broken token reference inside .alert--success
  // would leave this box either red or uncoloured, and only a computed-style
  // check — not the static class assertion above — would catch that.
  await expect(page.locator("#notice")).toHaveCSS("color", "rgb(21, 128, 61)");
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

  // A failed discard must still surface #error in its ordinary, unchanged
  // danger styling — #68 gave #notice its own success variant but must not
  // have touched what an actual error box looks like.
  const errorBox = page.locator("#error");
  await expect(errorBox).toBeHidden();
  await page.route(`**/jobs/${PRODUCT_JOB}`, async (route) => {
    if (route.request().method() === "DELETE") {
      await route.fulfill({ status: 500, contentType: "application/json", body: "{}" });
    } else {
      await route.continue();
    }
  });
  page.once("dialog", (dialog) => dialog.accept());
  await card.getByRole("button", { name: "Discard" }).click();
  await expect(errorBox).toBeVisible();
  await expect(errorBox).not.toHaveClass(/alert--success/);
  // Resolved --color-danger (#b91c1c), the same red it always was.
  await expect(errorBox).toHaveCSS("color", "rgb(185, 28, 28)");
  await expect(card).toBeVisible();
  await page.unroute(`**/jobs/${PRODUCT_JOB}`);

  page.once("dialog", (dialog) => dialog.accept());
  await card.getByRole("button", { name: "Discard" }).click();
  await expect(card).toHaveCount(0);

  const gone = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${PRODUCT_JOB}`);
  expect(gone.status()).toBe(404);
});

test("a new product's picture can be taken from the reviewed photo", async ({ page }) => {
  // No seeded job has a photo on disk, and the E2E stack has no step to put
  // one there. So the job is read from the server as it really is and then
  // shown to the page as a photo job, with a box on its first row only — the
  // two cases the picture choice must tell apart. What is under test is the
  // review screen: that it offers the right choices and sends the right field.
  // Cutting and storing the picture is covered by the Go tests
  // (internal/httpapi/productimages_test.go).
  //
  // LOOK_ONLY_JOB, because no other test consumes it and this one must not
  // either: the confirm below is answered here, never sent to the server.
  await logInAsBob(page);

  await page.route(`**/jobs/${LOOK_ONLY_JOB}`, async (route) => {
    const response = await route.fetch();
    const job = await response.json();
    job.has_image = true;
    job.payload.rows[0].bounding_box = { x: 0.1, y: 0.2, width: 0.4, height: 0.5 };
    job.payload.rows[1].bounding_box = null;
    await route.fulfill({ response, json: job });
  });
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/image`, (route) =>
    route.fulfill({ contentType: "image/png", body: TINY_PNG }),
  );

  let sent = null;
  await page.route(`**/ingest/${LOOK_ONLY_JOB}/confirm`, async (route) => {
    sent = route.request().postDataJSON();
    await route.fulfill({ json: { batch_ids: [], products_created: 2, locations_created: 0 } });
  });

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const rice = rows.nth(0);
  const lentils = rows.nth(1);
  await expect(rice).toContainText("Rice 1kg");
  await expect(lentils).toContainText("Lentils 500g");

  // Both are new products on a photo job, so both are offered a picture —
  // and neither has one until the reviewer picks it.
  const riceChoice = rice.locator('[data-role="new-product-image"]');
  const lentilsChoice = lentils.locator('[data-role="new-product-image"]');
  await expect(rice.locator('[data-role="new-product-image-field"]')).toBeVisible();
  await expect(lentils.locator('[data-role="new-product-image-field"]')).toBeVisible();
  await expect(riceChoice).toHaveValue("");
  await expect(lentilsChoice).toHaveValue("");

  // Only a row with a box can offer its own crop.
  await expect(riceChoice.locator("option")).toHaveText(["No picture", "This item, cut from the photo", "The whole photo"]);
  await expect(lentilsChoice.locator("option")).toHaveText(["No picture", "The whole photo"]);

  await riceChoice.selectOption("crop");
  await rice.locator('[data-role="location"]').selectOption(PANTRY);
  await lentils.locator('[data-role="location"]').selectOption(PANTRY);

  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/inbox\.html/);

  // The chosen picture travels as new_product.image; the row left on "No
  // picture" sends no image key at all.
  expect(sent.items).toEqual([
    {
      row_id: "0",
      decision: "accept",
      new_product: { name: "Rice 1kg", item_type: "long_shelf_life", image: "crop" },
      quantity: 1,
      location_id: PANTRY,
    },
    {
      row_id: "1",
      decision: "accept",
      new_product: { name: "Lentils 500g", item_type: "long_shelf_life" },
      quantity: 2,
      location_id: PANTRY,
    },
  ]);

  // And the job is still waiting on the server, untouched.
  const job = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${LOOK_ONLY_JOB}`);
  expect((await job.json()).status).toBe("done");
});

// docs/specs/26-location-quick-create.md: the "+ New location" escape hatch
// on every location field, in a storage that already has locations — Pantry
// and Fridge here — so this also proves the trigger is not conditional on an
// empty tree.
test("a location can be created from the review screen without leaving it, refreshing every row from one GET", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const first = rows.nth(0);
  const second = rows.nth(1);

  // Edits on the row that never opens the modal — proof they survive the
  // other row's use of it. The location is a real, non-default choice: a
  // refresh that rebuilt the selects from scratch would snap it back to the
  // placeholder, which a placeholder-vs-placeholder comparison cannot see.
  await second.locator('[data-role="quantity"]').fill("9");
  await second.locator('[data-role="location"]').selectOption(PANTRY);

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Pantry");

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Loft Shelf");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Loft Shelf");

  // Closing the modal once must refresh both rows' option lists from exactly
  // one GET — never one request per open field.
  let getCalls = 0;
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "GET") getCalls++;
    await route.continue();
  });
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Preselected in the field whose trigger opened the modal — the new
  // location itself, not merely "something other than the placeholder"…
  await expect(first.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Loft Shelf");
  // …offered, but not forced, on the row that never opened it, whose own
  // choices are exactly as they were left.
  await expect(second.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(second.locator('[data-role="location"]')).toHaveValue(PANTRY);
  await expect(second.locator('[data-role="quantity"]')).toHaveValue("9");

  // Only now that both rows are confirmed refreshed: exactly one GET did it,
  // never one request per open field.
  expect(getCalls).toBe(1);
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  // Cancelling with nothing created leaves every selection exactly as it was:
  // the opened field's own (Pantry, a non-default choice) and the other row's
  // (Loft Shelf), by both dismissals the spec names. Esc is the browser's own
  // cancel; the backdrop is a click on the ::backdrop, which lands well
  // outside the centred dialog's box, so it exercises tree-modal.js's
  // backdrop handler and not a click on the dialog's own padding.
  for (const dismiss of [() => page.keyboard.press("Escape"), () => page.mouse.click(5, 5)]) {
    await second.locator('[data-role="location-add"]').click();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeVisible();
    await dismiss();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeHidden();
    await expect(second.locator('[data-role="location"]')).toHaveValue(PANTRY);
    await expect(first.locator('[data-role="location"] option:checked')).toContainText("Loft Shelf");
  }
});

// #154 item 1: .card puts its padding on the <dialog> element itself
// (css/components.css), so event.target === dialog is also true for a click
// that never left the dialog's own box. tree-modal.js must tell that apart
// from an actual backdrop click, or a misclick this close to the edge closes
// the modal and loses whatever the user was mid-typing in the add-root form.
test("a click inside the location modal's own padding does not close it or lose an unsubmitted root name", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  const input = dialog.locator('input[aria-label="Name of the new top-level location"]');
  await input.fill("Garage Shelf");

  // Two pixels in from the dialog's own top-left corner: still inside its
  // rendered box (--space-5 padding is 24px, css/tokens.css), on no child
  // element, so event.target is the <dialog> itself — the exact shape of a
  // click on the padding rather than the backdrop.
  const box = await dialog.boundingBox();
  await page.mouse.click(box.x + 2, box.y + 2);

  await expect(dialog).toBeVisible();
  await expect(input).toHaveValue("Garage Shelf");
});

// #154 item 2: close() must wait for a create POST that's still in flight
// before resolving createdIds, or Done/Esc pressed early loses the new
// location from that resolve and the caller's own refresh races the commit.
test("closing the location modal while its create POST is still in flight still preselects the new location", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  // Held open until released below, so Done can be clicked while the create
  // POST this test cares about is still unresolved.
  let releasePost;
  const postHeld = new Promise((resolve) => {
    releasePost = resolve;
  });
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "POST") await postHeld;
    await route.continue();
  });

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Cellar Rack");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();

  // Done, clicked before the POST above returns: the dialog must stay open
  // rather than resolving createdIds without the id that POST will carry.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  releasePost();
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  await expect(dialog).toBeHidden();
  await expect(first.locator('[data-role="location"] option', { hasText: "Cellar Rack" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Cellar Rack");
});

// docs/specs/26-location-quick-create.md's first acceptance criterion, for
// `06` review specifically: "in a storage with zero locations ... the user
// opens the modal, creates a root location, closes the modal, and that
// location is immediately selectable — no page reload." The test above
// covers the *second* bullet (an already-populated tree); this one starts
// from "E2E Admin Household", which — despite the name — also has no
// locations at all (the same as "E2E Zero-Locations Household" does), so
// the placeholder-only select, the empty-tree message inside the modal, and
// setupLocation's option handling with nothing already appended are all
// exercised from a genuine cold start.
test("a location can be created from the review screen in a storage with zero locations", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin-2", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/review.html?storage=${ZERO_LOCATIONS_HOUSEHOLD}&job=${ZERO_LOCATIONS_JOB}`);
  const row = page.locator("#rows .review-row").first();
  await expect(row).toBeVisible();
  await expect(row.locator('[data-role="location"] option')).toHaveCount(1); // the placeholder alone

  await row.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toContainText("No locations yet");

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Hall Closet");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Hall Closet");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Immediately selectable — no reload.
  await expect(row.locator('[data-role="location"] option', { hasText: "Hall Closet" })).toHaveCount(1);
  await expect(row.locator('[data-role="location"]')).not.toHaveValue("");
});

// docs/specs/27-category-quick-create.md: js/tree-modal.js generalized to a
// "categories" kind, with the same "+ New category" escape hatch beside a
// new product's category field. Reuses LOCATION_MODAL_JOB — never
// confirmed, so it can be revisited by this test however many times it is
// run, same as the location test above — and adds the new category as a
// child of the seeded "Canned Goods" node, matching the acceptance
// criterion's own example ("adds it as a child of an existing node").
test("a category can be created from the review screen's new-product form, as a child of an existing node", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const first = rows.nth(0);
  const second = rows.nth(1);

  // Neither row matched an existing product, so the new-product fields —
  // category included — are already visible.
  await expect(first.locator('[data-role="new-product"]')).toBeVisible();

  // Edits on the row that never opens the modal — proof they survive the
  // other row's use of it. The category is a real, non-default choice, for the
  // same reason as the location in the test above.
  await second.locator('[data-role="new-product-name"]').fill("Ground Cumin");
  await second.locator('[data-role="new-product-category"]').selectOption(CANNED_GOODS);

  await first.locator('[data-role="new-product-category-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Categories" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Canned Goods");

  const cannedGoods = dialog.locator(`.tree-node[data-id="${CANNED_GOODS}"]`);

  // The shelf-life-rule detail beside "Canned Goods" is categories.html's
  // own renderer (js/category-shelf-life.js), not a stripped-down copy: its
  // label and editor behave here exactly as there. No save — mutating this
  // shared household's shelf life would ripple into every other test that
  // reads Canned Tomatoes' batches.
  await expect(cannedGoods).toContainText("730 days");
  await cannedGoods.getByRole("button", { name: "730 days" }).click();
  await expect(cannedGoods.locator('input[type="number"]')).toHaveValue("730");
  await cannedGoods.getByRole("button", { name: "Cancel" }).click();
  await expect(cannedGoods).toContainText("730 days");

  await cannedGoods.getByRole("button", { name: "Add" }).click();
  await dialog.locator("li.row input[type=text]").fill("Frozen Goods");
  await dialog.locator("li.row").getByRole("button", { name: "Add" }).click();
  await expect(dialog).toContainText("Frozen Goods");

  // Closing the modal once must refresh both rows' category lists from exactly
  // one GET, as spec 26 requires of the location fields — openCategoryField is
  // its own copy of that loop, so the location test's count does not cover it.
  let categoryGets = 0;
  const countCategoryGets = async (route) => {
    if (route.request().method() === "GET") categoryGets++;
    await route.continue();
  };
  const categoriesUrl = (url) => url.pathname === `/api/storages/${HOUSEHOLD}/categories`;
  await page.route(categoriesUrl, countCategoryGets);
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Preselected in the field whose trigger opened the modal…
  await expect(first.locator('[data-role="new-product-category"] option', { hasText: "Frozen Goods" })).toHaveCount(1);
  const categoryId = await first.locator('[data-role="new-product-category"]').inputValue();
  expect(categoryId).not.toBe("");
  // …offered, but not forced, on the row that never opened it, whose own
  // choices are exactly as they were left.
  await expect(second.locator('[data-role="new-product-category"] option', { hasText: "Frozen Goods" })).toHaveCount(1);
  await expect(second.locator('[data-role="new-product-category"]')).toHaveValue(CANNED_GOODS);
  await expect(second.locator('[data-role="new-product-name"]')).toHaveValue("Ground Cumin");

  // Only now that both rows are confirmed refreshed: exactly one GET did it.
  expect(categoryGets).toBe(1);
  await page.unroute(categoriesUrl, countCategoryGets);

  // Completing the row's confirm against it: the category created above
  // travels through unchanged, on the same POST .../confirm review.js
  // already sends — no second creation path. The confirm itself is
  // intercepted rather than let through for real, so this job stays
  // "never confirmed" for any other test that reuses it; the second row is
  // rejected so it needs no location of its own.
  await first.locator('[data-role="location"]').selectOption(PANTRY);
  await second.locator('[data-action="reject"]').click();

  let confirmBody;
  await page.route(`**/api/storages/${HOUSEHOLD}/ingest/${LOCATION_MODAL_JOB}/confirm`, async (route) => {
    confirmBody = route.request().postDataJSON();
    await route.fulfill({
      json: { batch_ids: ["00000000-0000-7000-8000-0000000000ff"], products_created: 1, locations_created: 0 },
    });
  });
  await page.click("#confirm");
  await expect(page).toHaveURL(/\/inbox\.html/);

  const accepted = confirmBody.items.find((item) => item.row_id === "0");
  expect(accepted.decision).toBe("accept");
  expect(accepted.new_product.category_id).toBe(categoryId);
});

// docs/specs/27-category-quick-create.md's first acceptance criterion, the
// same "zero" scenario the location test above covers, for categories: "E2E
// Admin Household" has none seeded either, so the placeholder-only select
// and the modal's empty-tree message are both exercised from a genuine cold
// start.
test("a category can be created from the review screen in a storage with zero categories", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin-2", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/review.html?storage=${ZERO_LOCATIONS_HOUSEHOLD}&job=${ZERO_LOCATIONS_JOB}`);
  const row = page.locator("#rows .review-row").first();
  await expect(row).toBeVisible();
  await expect(row.locator('[data-role="new-product-category"] option')).toHaveCount(1); // "No category" alone

  await row.locator('[data-role="new-product-category-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Categories" });
  await expect(dialog).toContainText("No categories yet");

  await dialog.getByRole("button", { name: "Add top-level category" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level category"]').fill("Games");
  await dialog.locator("#category-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Games");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Immediately selectable — no reload.
  await expect(row.locator('[data-role="new-product-category"] option', { hasText: "Games" })).toHaveCount(1);
  await expect(row.locator('[data-role="new-product-category"]')).not.toHaveValue("");
});
