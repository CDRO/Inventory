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
const TOMATOES = "00000000-0000-7000-8000-000000000040";
const PANTRY = "00000000-0000-7000-8000-000000000020";

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

  // An edit on the row that never opens the modal — proof it survives the
  // other row's use of it.
  await second.locator('[data-role="quantity"]').fill("9");

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
  expect(getCalls).toBe(1);
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  // Preselected in the field whose trigger opened the modal…
  await expect(first.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"]')).not.toHaveValue("");
  // …offered, but not forced, on the row that never opened it, whose own
  // edit is exactly as it was left.
  await expect(second.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(second.locator('[data-role="location"]')).toHaveValue("");
  await expect(second.locator('[data-role="quantity"]')).toHaveValue("9");

  // Cancelling (Esc), with nothing created, leaves the selection exactly as
  // it was.
  const beforeCancel = await second.locator('[data-role="location"]').inputValue();
  await second.locator('[data-role="location-add"]').click();
  await expect(page.getByRole("dialog", { name: "Locations" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Locations" })).toBeHidden();
  await expect(second.locator('[data-role="location"]')).toHaveValue(beforeCancel);
});
