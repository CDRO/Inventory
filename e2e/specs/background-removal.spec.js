// Background removal on a new product's picture
// (docs/specs/09-consumption-logging.md, "Optional: background removal on a
// user photo").
//
// The E2E stack sets no GEMINI_IMAGE_MODEL, which is exactly the acceptance
// criterion tested against the real server first: no control anywhere. The
// offer itself cannot be produced by this stack, so the second test answers the
// page's requests the way a deployment with the model would, and checks what
// the review screen shows and sends. Making and storing cutouts is covered by
// the Go tests (internal/httpapi/cutouts_test.go, internal/images/cutout_test.go).
//
// LOOK_ONLY_JOB, as in ingestion.spec.js: no test may consume it, so every
// confirm here is answered by the test, never sent to the server.

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const LOOK_ONLY_JOB = "00000000-0000-7000-8000-000000000072";
const PANTRY = "00000000-0000-7000-8000-000000000020";
const CUTOUT_ID = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";

// A real, decodable 1×1 PNG, standing in for the photo and for the cutout.
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

// asPhotoJob shows the page the real job as a photo job with a box on its
// first row, and — unless offer is given — the server's own word on whether
// background removal is available.
async function asPhotoJob(page, { offer } = {}) {
  const seen = {};
  await page.route(`**/jobs/${LOOK_ONLY_JOB}`, async (route) => {
    const response = await route.fetch();
    const job = await response.json();
    seen.backgroundRemoval = job.background_removal;
    job.has_image = true;
    job.payload.rows[0].bounding_box = { x: 0.1, y: 0.2, width: 0.4, height: 0.5 };
    job.payload.rows[1].bounding_box = null;
    if (offer !== undefined) job.background_removal = offer;
    await route.fulfill({ response, json: job });
  });
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/image`, (route) =>
    route.fulfill({ contentType: "image/png", body: TINY_PNG }),
  );
  return seen;
}

test("with no image model configured, no background-removal control is rendered", async ({ page }) => {
  await logInAsBob(page);
  const seen = await asPhotoJob(page);

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);
  expect(seen.backgroundRemoval).toBe(false);

  // Even once a picture from the photo is chosen — the moment the offer would
  // otherwise appear.
  await rows.nth(0).locator('[data-role="new-product-image"]').selectOption("crop");
  await rows.nth(1).locator('[data-role="new-product-image"]').selectOption("photo");
  await expect(page.getByRole("button", { name: "Remove background" })).toHaveCount(0);
  await expect(page.locator('[data-role="cutout"]:visible')).toHaveCount(0);

  // And there is no route to ask anyway.
  const refused = await page.request.post(`/api/storages/${HOUSEHOLD}/ingest/${LOOK_ONLY_JOB}/cutouts`, {
    data: { row_id: "0", source: "crop" },
  });
  expect(refused.status()).toBe(404);
});

test("a cutout is shown beside the original, and is sent only when chosen", async ({ page }) => {
  await logInAsBob(page);
  await asPhotoJob(page, { offer: true });

  const asked = [];
  await page.route(`**/ingest/${LOOK_ONLY_JOB}/cutouts`, async (route) => {
    const body = route.request().postDataJSON();
    asked.push(body);
    if (body.row_id === "1") {
      // The model failed for this one: the screen keeps the original.
      await route.fulfill({
        status: 502,
        json: { error: { code: "upstream_failed", message: "The AI provider could not complete this request." } },
      });
      return;
    }
    await route.fulfill({
      status: 201,
      json: { cutout_id: CUTOUT_ID, url: `/api/storages/${HOUSEHOLD}/ingest/${LOOK_ONLY_JOB}/cutouts/${CUTOUT_ID}` },
    });
  });
  await page.route(`**/ingest/${LOOK_ONLY_JOB}/cutouts/${CUTOUT_ID}`, (route) =>
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

  // Never offered before the reviewer picks a picture from their own photo.
  await expect(rice.getByRole("button", { name: "Remove background" })).toBeHidden();
  await rice.locator('[data-role="new-product-image"]').selectOption("crop");
  await rice.getByRole("button", { name: "Remove background" }).click();

  // Side by side, with the original still the one chosen.
  const compare = rice.locator('[data-role="cutout-compare"]');
  await expect(compare).toBeVisible();
  await expect(rice.getByRole("radio", { name: "Original" })).toBeChecked();
  await expect(rice.getByRole("radio", { name: "Without background" })).not.toBeChecked();
  await expect(rice.locator('[data-role="cutout-image"]')).toHaveAttribute(
    "src",
    `/api/storages/${HOUSEHOLD}/ingest/${LOOK_ONLY_JOB}/cutouts/${CUTOUT_ID}`,
  );
  expect(asked).toEqual([{ row_id: "0", source: "crop" }]);

  // A different picture starts over: the cutout belonged to the crop.
  await rice.locator('[data-role="new-product-image"]').selectOption("photo");
  await expect(compare).toBeHidden();
  await rice.locator('[data-role="new-product-image"]').selectOption("crop");
  await rice.getByRole("button", { name: "Remove background" }).click();
  await expect(compare).toBeVisible();
  await rice.getByRole("radio", { name: "Without background" }).check();

  // A failure is shrugged off: a note, the original kept, the flow unblocked.
  await lentils.locator('[data-role="new-product-image"]').selectOption("photo");
  await lentils.getByRole("button", { name: "Remove background" }).click();
  await expect(lentils.locator('[data-role="cutout-status"]')).toContainText("original picture is kept");
  await expect(lentils.locator('[data-role="cutout-compare"]')).toBeHidden();
  await expect(page.locator("#error")).toBeHidden();

  await rice.locator('[data-role="location"]').selectOption(PANTRY);
  await lentils.locator('[data-role="location"]').selectOption(PANTRY);
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/inbox\.html/);

  expect(sent.items).toEqual([
    {
      row_id: "0",
      decision: "accept",
      new_product: { name: "Rice 1kg", item_type: "long_shelf_life", image: "cutout", cutout_id: CUTOUT_ID },
      quantity: 1,
      location_id: PANTRY,
    },
    {
      row_id: "1",
      decision: "accept",
      new_product: { name: "Lentils 500g", item_type: "long_shelf_life", image: "photo" },
      quantity: 2,
      location_id: PANTRY,
    },
  ]);
});
