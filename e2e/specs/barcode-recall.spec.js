// Barcode recall (docs/specs/20-barcode-recall.md).
//
// Everything here runs as Dana against "E2E Barcode Household", which is hers
// alone (e2e/fixtures/seed.sql). Two reasons: a barcode names exactly one
// product per storage, so associating, deleting and re-associating a code in a
// shared storage would collide with any other suite that later wanted one; and
// the capture-time offer's state lives on the *user* row, so a shared user
// would have this file's on/off flips land in another suite's run.
//
// One exception: the barcode_prompt_seen_at test near the end (#157) runs as
// e2e-barcode-first against "E2E Barcode First" instead. It needs a user whose
// seen_at is still NULL, which Dana no longer is by the time this file reaches
// that test — every earlier test here has already shown her the offer at
// least once.
//
// Serial, unlike most files here. playwright.config.js sets fullyParallel, and
// two of these tests move the same per-user preference in opposite directions —
// which is a race whatever storage they use. Each test also gets its own
// product from the fixture, so the order they run in decides nothing else.
//
// The criteria this file exists for are the ones no Go test can reach, because
// they are about what a person is shown:
//
//   * the offer presents exactly three actions, never fewer;
//   * "not this time" writes nothing and reappears on the next product;
//   * "turn this off" stops it for good, and settings.html turns it back on;
//   * turning it off takes no feature away — the product page still works.

import { test, expect } from "@playwright/test";

test.describe.configure({ mode: "serial" });

const BARCODE_HOUSEHOLD = "00000000-0000-7000-8000-000000000014";
const BASE = `/api/storages/${BARCODE_HOUSEHOLD}`;
const LARDER = "00000000-0000-7000-8000-000000000026";

// One product per test (e2e/fixtures/seed.sql).
const RECALL = "00000000-0000-7000-8000-000000000048";
const CLAIMED = "00000000-0000-7000-8000-000000000049";
const RIVAL = "00000000-0000-7000-8000-00000000004a";
const OFFERED = "00000000-0000-7000-8000-00000000004b";
const REOFFERED = "00000000-0000-7000-8000-00000000004c";
const UNOFFERED = "00000000-0000-7000-8000-00000000004d";
const HAND_TYPED = "00000000-0000-7000-8000-00000000004e";
const PRE_CODED = "00000000-0000-7000-8000-00000000004f";
const CONTROL = "00000000-0000-7000-8000-000000000050";

const RECALL_CODE = "4006381333931";
const CONFLICT_CODE = "5449000000996";
const TYPED_CODE = "7622210449283";
const PRE_CODED_CODE = "3017620422003";

// #157's own household, user and codes — see the file header for why they
// cannot be Dana's.
const FIRST_TIMER_HOUSEHOLD = "00000000-0000-7000-8000-00000000001c";
const FIRST_TIMER_BASE = `/api/storages/${FIRST_TIMER_HOUSEHOLD}`;
const FIRST_TIMER_PRE_CODED = "00000000-0000-7000-8000-000000000054";
const FIRST_TIMER_CONTROL = "00000000-0000-7000-8000-000000000055";
const FIRST_TIMER_CODE = "0043000009805";

const OFFER_DIALOG = "dialog[aria-labelledby='barcode-offer-title']";

async function loginAsDana(page) {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-dana", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);
}

async function loginAsFirstTimer(page) {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-barcode-first", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);
}

// showOfferFor drives the offer component the three capture screens load, from
// the page it is being shown on.
//
// A dynamic import rather than a whole ingestion or shopping-list journey: it
// is the same module, in a real browser, talking to the real endpoints, and
// asserting its contract this way does not make the assertion depend on some
// other spec's flow still reaching this point. The promise is deliberately not
// awaited — it settles only when the dialog is answered.
async function showOfferFor(page, productId) {
  await page.evaluate(
    async ([storageId, id]) => {
      const module = await import("/js/barcode-offer.js");
      window.__offer = module.offerBarcodeCapture(storageId, { productId: id });
    },
    [BARCODE_HOUSEHOLD, productId],
  );
}

test("a scanned code recalls the product, and only the confirm writes anything", async ({ page }) => {
  await loginAsDana(page);

  // Nothing known yet: the miss is a 404, which is what sends the UI to the
  // single-product photo path instead.
  expect((await page.request.get(`${BASE}/barcodes/${RECALL_CODE}`)).status()).toBe(404);

  const associated = await page.request.post(`${BASE}/products/${RECALL}/barcodes`, {
    data: { barcode: RECALL_CODE },
  });
  expect(associated.status()).toBe(201);
  expect((await associated.json()).product_id).toBe(RECALL);

  // The recall itself: one GET, no external call, this storage's own product.
  const hit = await page.request.get(`${BASE}/barcodes/${RECALL_CODE}`);
  expect(hit.status()).toBe(200);
  const resolved = await hit.json();
  expect(resolved.product.product_id).toBe(RECALL);
  expect(resolved.product.name).toBe("Recall Beans");
  expect(resolved.catalog_suggestion).toBeNull();
  const before = resolved.product.current_stock;

  // Scanning wrote nothing: a second lookup sees the same stock.
  const again = await page.request.get(`${BASE}/barcodes/${RECALL_CODE}`);
  expect((await again.json()).product.current_stock).toBe(before);

  // The confirm is what writes, and it writes the batch and its log row
  // together.
  const logged = await page.request.post(`${BASE}/barcodes/${RECALL_CODE}/log`, {
    data: { direction: "in", quantity: 3, location_id: LARDER },
  });
  expect(logged.status()).toBe(201);
  const wrote = await logged.json();
  expect(wrote.current_stock).toBe(before + 3);
  expect(wrote.created_batch_id).not.toBeNull();

  // Read back independently of the response that claimed it.
  const detail = await page.request.get(`${BASE}/products/${RECALL}`);
  expect(detail.status()).toBe(200);
  const product = await detail.json();
  expect(product.current_stock).toBe(before + 3);
  expect(product.logs.some((entry) => entry.change_qty === 3 && entry.reason === "purchase")).toBe(true);

  // The other direction, under spec 09's rules.
  const used = await page.request.post(`${BASE}/barcodes/${RECALL_CODE}/log`, {
    data: { direction: "out", quantity: 1 },
  });
  expect(used.status()).toBe(201);
  expect((await used.json()).current_stock).toBe(before + 2);

  // Over-decrementing is refused rather than clamped: a clamped decrement
  // would record a consumption that did not happen.
  const tooMany = await page.request.post(`${BASE}/barcodes/${RECALL_CODE}/log`, {
    data: { direction: "out", quantity: 10_000 },
  });
  expect(tooMany.status()).toBe(422);
});

test("a code names one product per storage, and deleting it frees the code", async ({ page }) => {
  await loginAsDana(page);

  expect(
    (await page.request.post(`${BASE}/products/${CLAIMED}/barcodes`, { data: { barcode: CONFLICT_CODE } })).status(),
  ).toBe(201);

  const clash = await page.request.post(`${BASE}/products/${RIVAL}/barcodes`, {
    data: { barcode: CONFLICT_CODE },
  });
  expect(clash.status()).toBe(409);
  // The refusal names nothing about any other storage — there is nothing
  // about another storage to name.
  expect(await clash.text()).not.toContain("storage");

  expect((await page.request.delete(`${BASE}/products/${CLAIMED}/barcodes/${CONFLICT_CODE}`)).status()).toBe(204);
  expect(
    (await page.request.post(`${BASE}/products/${RIVAL}/barcodes`, { data: { barcode: CONFLICT_CODE } })).status(),
  ).toBe(201);
});

test("the decode fallback refuses a photo with no barcode in it", async ({ page }) => {
  await loginAsDana(page);

  // A 1x1 PNG: a real, decodable image with nothing in it.
  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
    "base64",
  );
  const response = await page.request.post(`${BASE}/barcodes/decode`, {
    multipart: { image: { name: "photo.png", mimeType: "image/png", buffer: png } },
  });

  expect(response.status()).toBe(422);
  const body = await response.json();
  expect(body.error.code).toBe("validation_failed");
  // Never a guessed value and never an empty string.
  expect(body).not.toHaveProperty("barcode");
});

test("the capture-time offer presents exactly three actions, and declining writes nothing", async ({ page }) => {
  await loginAsDana(page);
  expect((await page.request.patch("/api/auth/barcode-prompt", { data: { enabled: true } })).status()).toBe(200);
  await page.goto(`/products.html?storage=${BARCODE_HOUSEHOLD}`);

  await showOfferFor(page, OFFERED);
  const dialog = page.locator(OFFER_DIALOG);
  await expect(dialog).toBeVisible();

  const buttons = dialog.locator("button");
  await expect(buttons).toHaveCount(3);
  await expect(buttons.nth(0)).toHaveText("Scan it now");
  await expect(buttons.nth(1)).toHaveText("Not this time");
  await expect(buttons.nth(2)).toHaveText("Turn this off");

  // "Not this time" records nothing: no barcode, and no change of preference.
  await buttons.nth(1).click();
  await expect(dialog).toHaveCount(0);

  const codes = await page.request.get(`${BASE}/products/${OFFERED}/barcodes`);
  expect(codes.status()).toBe(200);
  expect((await codes.json()).items).toEqual([]);
  expect((await (await page.request.get("/api/auth/barcode-prompt")).json()).enabled).toBe(true);

  // …and it reappears on the next qualifying product, with the same three
  // actions.
  await showOfferFor(page, REOFFERED);
  await expect(page.locator(OFFER_DIALOG)).toBeVisible();
  await expect(page.locator(`${OFFER_DIALOG} button`)).toHaveCount(3);
  await page.locator(`${OFFER_DIALOG} button`).nth(1).click();
  await expect(page.locator(OFFER_DIALOG)).toHaveCount(0);
});

// The rejection half of the criterion: "a new product created with one
// already resolvable (e.g. the shopping-list line matched via a catalog
// barcode hint) does not" get the offer.
//
// It needs its own test because it is the branch that fails silently. Every
// other assertion here drives the offer's *visible* path; delete the
// eligibility check in js/barcode-offer.js and all of those stay green while
// the offer starts appearing for products that can already be recalled.
test("a product that already has a barcode is never offered another", async ({ page }) => {
  await loginAsDana(page);
  expect((await page.request.patch("/api/auth/barcode-prompt", { data: { enabled: true } })).status()).toBe(200);

  // The product carries a code before the offer is ever considered — the same
  // state a product created from a catalogue barcode hint is in.
  expect(
    (await page.request.post(`${BASE}/products/${PRE_CODED}/barcodes`, { data: { barcode: PRE_CODED_CODE } })).status(),
  ).toBe(201);

  await page.goto(`/products.html?storage=${BARCODE_HOUSEHOLD}`);

  // Awaited this time, unlike showOfferFor: the whole assertion is that it
  // resolves without ever putting a dialog on screen.
  await page.evaluate(
    async ([storageId, id]) => {
      window.__offerSettled = false;
      const module = await import("/js/barcode-offer.js");
      await module.offerBarcodeCapture(storageId, { productId: id });
      window.__offerSettled = true;
    },
    [BARCODE_HOUSEHOLD, PRE_CODED],
  );
  await expect.poll(() => page.evaluate(() => window.__offerSettled)).toBe(true);
  await expect(page.locator(OFFER_DIALOG)).toHaveCount(0);

  // The control: the very same call, on the very same page, for a product
  // with no code, does show it. Without this the test above would also pass
  // if the offer were broken outright.
  //
  // Its own fixture product, not one another test also writes to: a control
  // that silently stopped being code-less would turn this test green for the
  // wrong reason.
  await showOfferFor(page, CONTROL);
  await expect(page.locator(OFFER_DIALOG)).toBeVisible();
  await page.locator(`${OFFER_DIALOG} button`).nth(1).click();
  await expect(page.locator(OFFER_DIALOG)).toHaveCount(0);
});

// #157: the test above asserts that no dialog appears for a pre-coded
// product, but no dialog appears in *either* line order of
// hasBarcodeAlready/shouldOffer inside offerBarcodeCapture, since shouldOffer
// is never reached either way once hasBarcodeAlready has returned true. What
// only the correct order (hasBarcodeAlready first) guarantees is that
// shouldOffer's POST to .../shown never fires for that product — so this
// pins the side effect docs/specs/20-barcode-recall.md actually requires:
// "barcode_prompt_seen_at is set exactly once, on the first time the offer is
// shown". Swap the two lines and this product would silently burn that
// one-time flag without the offer ever appearing.
//
// Needs its own fixture user whose seen_at is still NULL — see the file
// header for why that cannot be Dana.
test("a pre-coded product's offer check leaves barcode_prompt_seen_at untouched", async ({ page }) => {
  await loginAsFirstTimer(page);

  // Never shown yet: first_time reads true straight from seen_at IS NULL.
  const before = await page.request.get("/api/auth/barcode-prompt");
  expect(before.status()).toBe(200);
  expect(await before.json()).toEqual({ enabled: true, first_time: true });

  // The product carries a code before the offer is ever considered — the same
  // state a product created from a catalogue barcode hint is in.
  expect(
    (
      await page.request.post(`${FIRST_TIMER_BASE}/products/${FIRST_TIMER_PRE_CODED}/barcodes`, {
        data: { barcode: FIRST_TIMER_CODE },
      })
    ).status(),
  ).toBe(201);

  await page.goto(`/products.html?storage=${FIRST_TIMER_HOUSEHOLD}`);

  await page.evaluate(
    async ([storageId, id]) => {
      const module = await import("/js/barcode-offer.js");
      await module.offerBarcodeCapture(storageId, { productId: id });
    },
    [FIRST_TIMER_HOUSEHOLD, FIRST_TIMER_PRE_CODED],
  );
  await expect(page.locator(OFFER_DIALOG)).toHaveCount(0);

  // The assertion #157 exists for: still first_time, because nothing was ever
  // shown to burn it.
  const stillFirst = await page.request.get("/api/auth/barcode-prompt");
  expect(await stillFirst.json()).toEqual({ enabled: true, first_time: true });

  // The control: the same call, same page, same user, for a codeless product
  // does show the offer and does burn first_time. Without this, the
  // assertion above would also pass if the offer were broken outright and
  // never marked anything shown at all.
  //
  // Not showOfferFor: that helper is fixed to BARCODE_HOUSEHOLD, which is not
  // this test's storage.
  await page.evaluate(
    async ([storageId, id]) => {
      const module = await import("/js/barcode-offer.js");
      window.__offer = module.offerBarcodeCapture(storageId, { productId: id });
    },
    [FIRST_TIMER_HOUSEHOLD, FIRST_TIMER_CONTROL],
  );
  const dialog = page.locator(OFFER_DIALOG);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator("h2")).toHaveText("One scan now, one tap forever");
  await dialog.locator("button").nth(1).click();
  await expect(dialog).toHaveCount(0);

  const afterShown = await page.request.get("/api/auth/barcode-prompt");
  expect(await afterShown.json()).toEqual({ enabled: true, first_time: false });
});

test("turning the offer off stops it everywhere, and settings turns it back on", async ({ page }) => {
  await loginAsDana(page);
  expect((await page.request.patch("/api/auth/barcode-prompt", { data: { enabled: true } })).status()).toBe(200);
  await page.goto(`/products.html?storage=${BARCODE_HOUSEHOLD}`);

  await showOfferFor(page, UNOFFERED);
  const dialog = page.locator(OFFER_DIALOG);
  await expect(dialog).toBeVisible();
  await dialog.locator("button").nth(2).click();
  await expect(dialog).toHaveCount(0);

  // The preference is the user's, not the storage's.
  await expect
    .poll(async () => (await (await page.request.get("/api/auth/barcode-prompt")).json()).enabled)
    .toBe(false);

  // The offer does not appear again, for any product.
  await page.evaluate(
    async ([storageId, id]) => {
      window.__offerSettled = false;
      const module = await import("/js/barcode-offer.js");
      await module.offerBarcodeCapture(storageId, { productId: id });
      window.__offerSettled = true;
    },
    [BARCODE_HOUSEHOLD, HAND_TYPED],
  );
  await expect.poll(() => page.evaluate(() => window.__offerSettled)).toBe(true);
  await expect(page.locator(OFFER_DIALOG)).toHaveCount(0);

  // settings.html shows the state and is the way back on.
  await page.goto("/settings.html");
  const toggle = page.locator("#barcode-prompt-enabled");
  await expect(toggle).toBeVisible();
  await expect(toggle).not.toBeChecked();

  await toggle.check();
  await expect(page.locator("#barcode-prompt-status")).toBeVisible();
  await expect
    .poll(async () => (await (await page.request.get("/api/auth/barcode-prompt")).json()).enabled)
    .toBe(true);
});

// Turning the offer off must never take an inventory feature away: the product
// screen's barcode list is a deliberate action, not an offer.
test("the product page adds and removes a barcode whatever the offer preference is", async ({ page }) => {
  await loginAsDana(page);
  expect((await page.request.patch("/api/auth/barcode-prompt", { data: { enabled: false } })).status()).toBe(200);

  await page.goto(`/products.html?storage=${BARCODE_HOUSEHOLD}`);
  await page.getByRole("button", { name: "Hand-typed Beans", exact: true }).click();

  const card = page.locator(".card", { has: page.getByRole("heading", { name: "Barcodes" }) });
  await expect(card).toBeVisible();
  await expect(card.locator('[data-role="barcode-list"]')).toContainText("No barcode yet.");

  await card.locator("#p-barcode").fill(TYPED_CODE);
  await card.getByRole("button", { name: "Add", exact: true }).click();
  await expect(card.locator('[data-role="barcode-list"]')).toContainText(TYPED_CODE);

  // And it recalls the product straight away, which is the whole point.
  const hit = await page.request.get(`${BASE}/barcodes/${TYPED_CODE}`);
  expect(hit.status()).toBe(200);
  expect((await hit.json()).product.product_id).toBe(HAND_TYPED);

  await card.getByRole("button", { name: "Remove" }).first().click();
  await expect(card.locator('[data-role="barcode-list"]')).toContainText("No barcode yet.");

  await page.request.patch("/api/auth/barcode-prompt", { data: { enabled: true } });
});
