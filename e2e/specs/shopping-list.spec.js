// Shopping list reconciliation (docs/specs/07-shopping-list-reconciliation.md):
// the page load, the gates, and required journey 6 of
// docs/specs/05-frontend-pwa-foundations.md — "Reconcile a shopping list
// across all three match states."
//
// The journey runs as Alice against "E2E Other Household", not against the
// household every other suite uses. Two reasons: pasting a list is the one
// flow whose result depends on *which* products a storage already has, so it
// needs a product set nobody else mutates; and resolving lines writes rows
// that would otherwise land in the storage the ingestion and consumption
// suites assert against, under playwright.config.js's fullyParallel.
//
// The three lines are chosen against internal/matching's published constants
// (ExactThreshold 0.6, AmbiguousThreshold 0.35, AmbiguousMargin 0.05) and
// pg_trgm's trigram similarity, so each lands in a different state by
// arithmetic rather than by luck:
//
//   "sourdough bread" — identical to the seeded Sourdough Bread, so
//       similarity 1.0. The two milks share almost no trigrams with it and
//       fall under AmbiguousThreshold, so it is the *only* candidate, which
//       makes it a clear winner: exact_match.
//   "milk" — "oat milk" and "soy milk" have the same word count and the same
//       word lengths, so pg_trgm builds nine trigrams for each and five of
//       them are the query's five. Both score exactly 5/9. A difference of
//       zero can never exceed AmbiguousMargin, so this is ambiguous however
//       the absolute thresholds are later tuned — the assertion does not rest
//       on 5/9 happening to sit below ExactThreshold.
//   "smoked paprika" — nothing in the storage comes near AmbiguousThreshold
//       and catalog_products is empty in this fixture, so stages 1 and 2 both
//       miss: new_item with no catalog card.

import { test, expect } from "@playwright/test";

const STORAGE_ID = "00000000-0000-4000-8000-000000000000";
const BASE = `/api/storages/${STORAGE_ID}`;
const HASH = "a".repeat(64);
const OTHER_HOUSEHOLD = "00000000-0000-7000-8000-000000000011";
// "E2E Zero-Locations Household": zero locations, zero products, one
// dedicated member (Casey) who belongs to nothing else — its own storage so
// this test's location-quick-create writes never race
// e2e/specs/ingestion.spec.js's own zero-locations test, which uses "E2E
// Admin Household" for the same acceptance criterion on the review screen.
const ZERO_LOCATIONS_HOUSEHOLD = "00000000-0000-7000-8000-000000000013";
const PASSWORD = "e2e-fixture-password";
// Fixture ids in OTHER_HOUSEHOLD (e2e/fixtures/seed.sql). Garage is its only
// location, so it is where every confirmed line in this suite lands.
const SOURDOUGH_BREAD = "00000000-0000-7000-8000-000000000042";
const SOY_MILK = "00000000-0000-7000-8000-000000000044";
const GARAGE = "00000000-0000-7000-8000-000000000022";

test("shopping-list.html loads for a member with no console errors", async ({ page }) => {
  const consoleErrors = [];
  page.on("pageerror", (err) => consoleErrors.push(String(err)));
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(`${msg.text()} (${msg.location()?.url ?? ""})`);
  });

  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto("/shopping-list.html");
  await expect(page).toHaveTitle(/Shopping list/);
  // The page writes the resolved storage into the URL once /api/auth/me has
  // answered, which is the signal that its load-time work is done.
  await expect(page).toHaveURL(/storage=00000000-0000-7000-8000-000000000010/);
  await expect(page.locator("#error")).toBeHidden();

  expect(consoleErrors, `unexpected console errors: ${consoleErrors.join("; ")}`).toEqual([]);
});

// A list matched before the storage's locations and categories arrive would
// render every line with empty "Put it in" and category pickers, so the button
// waits for both, and a load that fails leaves it disabled for good.
test("Match waits for the storage's locations and categories, and stays disabled if they fail", async ({ page }) => {
  const isCategories = (url) => new URL(url).pathname.endsWith("/categories");

  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: PASSWORD },
  });
  expect(login.status(), "fixture login").toBe(200);

  // Hold the categories response until the button has been seen disabled.
  let release;
  const held = new Promise((resolve) => (release = resolve));
  await page.route(isCategories, async (route) => {
    await held;
    await route.continue();
  });
  await page.goto("/shopping-list.html");
  await expect(page).toHaveURL(/storage=/);
  await expect(page.locator("#submit")).toBeDisabled();
  release();
  await expect(page.locator("#submit")).toBeEnabled();
  await page.unroute(isCategories);

  // Now the same load failing: the error is shown and Match never enables.
  await page.route(isCategories, (route) =>
    route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({ error: { code: "internal", message: "Something went wrong on our side." } }),
    }),
  );
  const failed = page.waitForResponse((res) => isCategories(res.url()) && res.status() === 500);
  await page.reload();
  await failed;
  await expect(page.locator("#error")).toBeVisible();
  await expect(page.locator("#submit")).toBeDisabled();
});

test("shopping-list.html without a session redirects to the login page", async ({ page }) => {
  await page.goto("/shopping-list.html");

  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server
  await expect(page.locator("#login-form")).toBeVisible();
});

test("every shopping list and image route is behind the session gate", async ({ request }) => {
  const routes = [
    ["post", `${BASE}/shopping-lists`],
    ["get", `${BASE}/shopping-lists/${STORAGE_ID}`],
    ["post", `${BASE}/shopping-lists/${STORAGE_ID}/items/${STORAGE_ID}/rematch`],
    ["post", `${BASE}/shopping-lists/${STORAGE_ID}/items/${STORAGE_ID}/resolve`],
    ["get", `${BASE}/image-suggestions?query=milk`],
    ["get", `${BASE}/images/${HASH}`],
  ];

  for (const [method, path] of routes) {
    const response = await request[method](path, { data: {} });

    expect(response.status(), `${method.toUpperCase()} ${path}`).toBe(401);
    const body = await response.json();
    expect(body.error.code).toBe("unauthorized");
    // APP_ENV=prod in this stack: the internal reason must not be disclosed.
    expect(body.error.debug_reason).toBeUndefined();
  }
});

test("the image endpoints are never reachable without a session, so no provider URL can leak", async ({
  request,
}) => {
  // The strongest statement this suite can make about the "browser never talks
  // to SerpAPI/Iconify" rule without a login: nothing an unauthenticated
  // client can reach returns an off-origin image URL.
  const response = await request.get(`${BASE}/image-suggestions?query=tomatoes`);
  expect(response.status()).toBe(401);

  const text = await response.text();
  expect(text).not.toContain("serpapi.com");
  expect(text).not.toContain("api.iconify.design");
  expect(text).not.toContain("googleusercontent");
});

// --- Required journey 6 -----------------------------------------------------

async function pasteList(page, lines) {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-alice", password: PASSWORD },
  });
  expect(login.status(), "fixture login").toBe(200);

  await page.goto(`/shopping-list.html?storage=${OTHER_HOUSEHOLD}`);
  await expect(page.locator("#compose")).toBeVisible();
  await page.fill("#raw-text", lines.join("\n"));

  // Capture the server's own classification alongside the rendered result:
  // the page is not allowed to re-derive these states, so asserting on the
  // response as well as the DOM is what would catch it starting to.
  const responsePromise = page.waitForResponse(
    (res) => res.url().endsWith("/shopping-lists") && res.request().method() === "POST",
  );
  await page.click("#submit");
  const created = await (await responsePromise).json();

  await expect(page.locator("#results")).toBeVisible();
  return created;
}

function lineFor(page, rawText) {
  return page.locator("#items .item", { hasText: rawText });
}

test("a pasted list lands in all three match states, and each resolves", async ({ page }) => {
  const created = await pasteList(page, ["sourdough bread", "milk", "smoked paprika"]);

  const byText = Object.fromEntries(created.items.map((item) => [item.raw_text, item]));
  expect(byText["sourdough bread"].status).toBe("exact_match");
  expect(byText["sourdough bread"].matched_product.name).toBe("Sourdough Bread");
  expect(byText["milk"].status).toBe("ambiguous");
  expect(byText["smoked paprika"].status).toBe("new_item");
  expect(byText["smoked paprika"].catalog ?? null, "the fixture catalog is empty").toBeNull();

  // Exact match: the product is named, and there is nothing to choose.
  const exact = lineFor(page, "sourdough bread");
  await expect(exact.locator('[data-field="status"]')).toHaveText("Match");
  await expect(exact.locator('[data-field="detail"]')).toContainText("Matches Sourdough Bread.");

  // Ambiguous: both candidates are offered, because neither outscores the
  // other by more than AmbiguousMargin.
  const ambiguous = lineFor(page, "milk");
  await expect(ambiguous.locator('[data-field="status"]')).toHaveText("Which one?");
  await expect(ambiguous.locator('[data-field="detail"]')).toContainText("Which one did you mean?");
  const candidates = ambiguous.locator('[data-field="detail"] button');
  await expect(candidates).toHaveCount(2);
  await expect(candidates).toHaveText(["Oat Milk", "Soy Milk"]);

  // New item: no local match and no catalog hit, so the page says so rather
  // than inventing a product card, and opens the form to describe it — named
  // from the line itself.
  const newItem = lineFor(page, "smoked paprika");
  await expect(newItem.locator('[data-field="status"]')).toHaveText("New");
  await expect(newItem.locator('[data-field="detail"]')).toContainText("Nothing known about this yet.");
  await expect(newItem.locator('[data-field="new-product"]')).toBeVisible();
  await expect(newItem.locator('[data-field="name"]')).toHaveValue("smoked paprika");

  // Resolve all three, each into Garage. The exact match carries an
  // overridden quantity, so `resolved_quantity` can be shown to record what
  // was applied rather than what was proposed
  // (docs/specs/07-shopping-list-reconciliation.md).
  await exact.locator('[data-field="quantity"]').fill("3");
  await exact.locator('[data-field="location"]').selectOption(GARAGE);
  await exact.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "sourdough bread").locator('[data-field="status"]')).toHaveText("Done");

  await candidates.filter({ hasText: "Soy Milk" }).click();
  await ambiguous.locator('[data-field="location"]').selectOption(GARAGE);
  await ambiguous.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "milk").locator('[data-field="status"]')).toHaveText("Done");

  await newItem.locator('[data-field="location"]').selectOption(GARAGE);
  await newItem.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "smoked paprika").locator('[data-field="status"]')).toHaveText("Done");

  // This line creates a product, so the capture-time offer of
  // docs/specs/20-barcode-recall.md follows it. Dismissed here — one tap, and
  // it writes nothing — so the rest of this journey runs against the page
  // rather than a modal over it. That offer's own contract is asserted in
  // e2e/specs/barcode-recall.spec.js.
  const barcodeOffer = page.locator("dialog[aria-labelledby='barcode-offer-title']");
  await expect(barcodeOffer).toBeVisible();
  await barcodeOffer.getByRole("button", { name: "Not this time" }).click();
  await expect(barcodeOffer).toHaveCount(0);

  for (const raw of ["sourdough bread", "milk", "smoked paprika"]) {
    const line = lineFor(page, raw);
    await expect(line.locator('[data-field="detail"]')).toContainText("Confirmed.");
    // A resolved line cannot be resolved twice: the server refuses it with a
    // conflict, so the UI must not offer the button again.
    await expect(line.locator('[data-action="resolve"]')).toBeDisabled();
    await expect(line.locator('[data-field="quantity"]')).toBeDisabled();
  }
  await expect(page.locator("#error")).toBeHidden();

  // What the server stored, read back independently of the page that wrote
  // it: every line resolved, the chosen product recorded for the two that had
  // one, and the overridden quantity kept.
  const reread = await page.request.get(
    `/api/storages/${OTHER_HOUSEHOLD}/shopping-lists/${created.id}`,
  );
  expect(reread.status()).toBe(200);
  const stored = Object.fromEntries((await reread.json()).items.map((i) => [i.raw_text, i]));

  for (const raw of ["sourdough bread", "milk", "smoked paprika"]) {
    expect(stored[raw].status, raw).toBe("resolved");
  }
  // By id, not by name: a resolved line is never re-matched for display
  // (internal/httpapi/shoppinglists.go's displayMatch skips it), so the
  // server returns the stored product id with no name attached. Asserting on
  // a name here would be asserting on a field that is empty by design.
  expect(stored["sourdough bread"].resolved_quantity).toBe(3);
  expect(stored["sourdough bread"].matched_product.id).toBe(SOURDOUGH_BREAD);
  expect(stored["milk"].matched_product.id, "the candidate the user picked").toBe(SOY_MILK);
  expect(stored["milk"].resolved_quantity).toBe(1);

  // And what confirming wrote to inventory — the point of reconciling a
  // list. Each line's quantity is now a batch in Garage: on the matched
  // product, on the candidate picked for the ambiguous line, and on the
  // product the new item created.
  const batchesOf = async (productId) => {
    const res = await page.request.get(`/api/storages/${OTHER_HOUSEHOLD}/products/${productId}/batches`);
    expect(res.status()).toBe(200);
    return (await res.json()).items;
  };
  const inGarage = (batches) =>
    batches.filter((b) => b.location_id === GARAGE).reduce((sum, b) => sum + b.quantity, 0);

  expect(inGarage(await batchesOf(SOURDOUGH_BREAD)), "the exact match").toBe(3);
  expect(inGarage(await batchesOf(SOY_MILK)), "the candidate picked for milk").toBe(1);

  const paprika = stored["smoked paprika"].matched_product;
  expect(paprika, "confirming the new item created a product").not.toBeNull();
  const products = (await (await page.request.get(`/api/storages/${OTHER_HOUSEHOLD}/products`)).json()).items;
  expect(products.find((p) => p.id === paprika.id)?.name).toBe("smoked paprika");
  expect(inGarage(await batchesOf(paprika.id)), "the new product's first batch").toBe(1);
});

test("an explicit multiplier in the pasted text becomes the proposed quantity", async ({ page }) => {
  // docs/specs/07-shopping-list-reconciliation.md: "auto-filled '+1 unit' (or
  // parsed quantity if present in raw_text, e.g. 'eggs x2')". The parser is
  // unit-tested in internal/matching; what this checks is that the parsed
  // number actually reaches the quantity field the user confirms.
  const created = await pasteList(page, ["sourdough bread x2"]);
  expect(created.items[0].status).toBe("exact_match");

  const line = lineFor(page, "sourdough bread x2");
  await expect(line.locator('[data-field="quantity"]')).toHaveValue("2");
  await expect(line.locator('[data-field="detail"]')).toContainText("Matches Sourdough Bread.");
});

// --- The resolution choices the fixture cannot reach on its own ------------
//
// Journey 6 above reaches all three match states through the real server. Two
// sets of choices it does not click are covered here: a catalog card's "Add
// this", its variants and "It's something else" — the fixture's catalog is
// empty on purpose, so no line gets a card — and an ambiguous line's two
// escape hatches.
//
// Both tests capture what the page sends rather than letting the server apply
// it. Creating products in "E2E Other Household" would change what journey 6's
// lines match, and fullyParallel runs these beside it. What confirming writes
// on each of these paths is covered against PostgreSQL in
// internal/store/shoppinglists_resolve_test.go, and the server's handling of
// each body in internal/httpapi/shoppinglists_resolve_test.go. The claim here
// is narrower: each button produces exactly the resolve body that path needs.

const CARD = {
  display_name: "Tomatoes",
  category_path: "Food > Vegetables",
  item_type: "perishable",
  image_url: null,
  icon_name: null,
  default_shelf_life_days: 7,
  variants: ["Cherry Tomatoes", "Yellow Tomatoes"],
};

// captureResolves answers every resolve on the page itself and records the
// body each one sent, by the line's raw text.
async function captureResolves(page, rawTextById) {
  const sent = {};
  await page.route("**/items/*/resolve", async (route) => {
    const id = route.request().url().split("/items/")[1].split("/")[0];
    const body = route.request().postDataJSON();
    sent[rawTextById()[id]] = body;
    const named = Boolean(body.product_id || body.new_product);
    await route.fulfill({
      json: {
        id,
        raw_text: rawTextById()[id],
        status: "resolved",
        quantity: body.quantity,
        matched_product: named ? { id: "00000000-0000-7000-8000-0000000000ff", name: "" } : null,
        candidates: [],
        catalog: null,
        needs_image_search: false,
        resolved_quantity: body.quantity,
        product_created: Boolean(body.new_product),
        batch_id: null,
      },
    });
  });
  return sent;
}

// showCatalogCard makes the server's answer to the pasted list describe every
// line with CARD, the shape a stage-2 catalog hit arrives in.
async function showCatalogCard(page) {
  await page.route(`**/api/storages/${OTHER_HOUSEHOLD}/shopping-lists`, async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    const response = await route.fetch();
    const list = await response.json();
    for (const item of list.items) {
      Object.assign(item, { status: "new_item", matched_product: null, candidates: [], catalog: CARD, needs_image_search: false });
    }
    await route.fulfill({ response, json: list });
  });
}

test("a catalog card is accepted with one click, or one of its variants is", async ({ page }) => {
  let rawTextById = {};
  const sent = await captureResolves(page, () => rawTextById);
  await showCatalogCard(page);

  const created = await pasteList(page, ["tomatoes", "tomatoes, c."]);
  rawTextById = Object.fromEntries(created.items.map((item) => [item.id, item.raw_text]));
  const rows = page.locator("#items .item");
  await expect(rows).toHaveCount(2);

  // The card is shown, and accepting it as described is the choice already
  // made: nothing needs clicking to "Add this", and no form is open.
  const accept = rows.nth(0);
  await expect(accept.locator('[data-field="detail"]')).toContainText("Food > Vegetables");
  await expect(accept.getByRole("button", { name: "Add this: Tomatoes" })).toHaveAttribute("aria-pressed", "true");
  await expect(accept.getByRole("button", { name: "Cherry Tomatoes" })).toHaveAttribute("aria-pressed", "false");
  await expect(accept.locator('[data-field="new-product"]')).toBeHidden();
  await accept.locator('[data-action="resolve"]').click();
  await expect(rows.nth(0).locator('[data-field="status"]')).toHaveText("Done");

  // A variant replaces the card as the choice.
  const variant = rows.nth(1);
  await variant.getByRole("button", { name: "Cherry Tomatoes" }).click();
  await expect(variant.getByRole("button", { name: "Cherry Tomatoes" })).toHaveAttribute("aria-pressed", "true");
  await expect(variant.getByRole("button", { name: "Add this: Tomatoes" })).toHaveAttribute("aria-pressed", "false");
  await variant.locator('[data-action="resolve"]').click();
  await expect(rows.nth(1).locator('[data-field="status"]')).toHaveText("Done");

  // No catalog id in either body — the page was never given one.
  expect(sent["tomatoes"]).toEqual({ quantity: 1, location_id: GARAGE, new_product: { from: "catalog" } });
  expect(sent["tomatoes, c."]).toEqual({
    quantity: 1,
    location_id: GARAGE,
    new_product: { from: "catalog", variant: "Cherry Tomatoes" },
  });
  await expect(page.locator("#error")).toBeHidden();
});

test("declining a catalog card opens an empty form for what it really is", async ({ page }) => {
  let rawTextById = {};
  const sent = await captureResolves(page, () => rawTextById);
  await showCatalogCard(page);

  const created = await pasteList(page, ["tomatoes"]);
  rawTextById = Object.fromEntries(created.items.map((item) => [item.id, item.raw_text]));
  const line = page.locator("#items .item").first();

  await line.getByRole("button", { name: "It's something else" }).click();

  // The card's choices are withdrawn and the name starts empty: prefilling
  // "Tomatoes" would invite confirming the very card that was just declined.
  await expect(line.locator('[data-field="new-product"]')).toBeVisible();
  await expect(line.locator('[data-field="name"]')).toHaveValue("");
  await expect(line.getByRole("button", { name: "Add this: Tomatoes" })).toBeDisabled();
  await expect(line.getByRole("button", { name: "It's something else" })).toHaveCount(0);

  await line.locator('[data-field="name"]').fill("Heirloom Tomatoes");
  await line.locator('[data-field="item_type"]').selectOption("perishable");
  await line.locator('[data-field="min_stock"]').fill("2");
  await line.locator('[data-action="resolve"]').click();
  await expect(line.locator('[data-field="status"]')).toHaveText("Done");

  // "manual" with the new name: the server compares it with the card it
  // showed, and links the two as variants because they differ.
  expect(sent["tomatoes"]).toEqual({
    quantity: 1,
    location_id: GARAGE,
    new_product: { from: "manual", name: "Heirloom Tomatoes", item_type: "perishable", min_stock: 2 },
  });
});

test("an ambiguous line can be treated as a new item, or entered by hand", async ({ page }) => {
  let rawTextById = {};
  const sent = await captureResolves(page, () => rawTextById);

  // Both lines are genuinely ambiguous on the real server: "milk" sits exactly
  // between Oat Milk and Soy Milk (see this file's header), with or without
  // the multiplier.
  const created = await pasteList(page, ["milk", "milk x2"]);
  expect(created.items.map((item) => item.status)).toEqual(["ambiguous", "ambiguous"]);
  rawTextById = Object.fromEntries(created.items.map((item) => [item.id, item.raw_text]));
  const rows = page.locator("#items .item");

  // "Treat as new item" keeps the line's own name, without its multiplier.
  const treat = rows.nth(0);
  await treat.getByRole("button", { name: "Treat as new item" }).click();
  await expect(treat.locator('[data-field="new-product"]')).toBeVisible();
  await expect(treat.locator('[data-field="name"]')).toHaveValue("milk");
  await expect(treat.getByRole("button", { name: "Oat Milk" })).toBeDisabled();
  await expect(treat.getByRole("button", { name: "Enter manually" })).toHaveCount(0);
  await treat.locator('[data-action="resolve"]').click();
  await expect(rows.nth(0).locator('[data-field="status"]')).toHaveText("Done");

  // "Enter manually" starts from nothing.
  const manual = rows.nth(1);
  await manual.getByRole("button", { name: "Enter manually" }).click();
  await expect(manual.locator('[data-field="name"]')).toHaveValue("");
  await manual.locator('[data-field="name"]').fill("Almond Milk");
  await manual.locator('[data-action="resolve"]').click();
  await expect(rows.nth(1).locator('[data-field="status"]')).toHaveText("Done");

  expect(sent["milk"]).toEqual({
    quantity: 1,
    location_id: GARAGE,
    new_product: { from: "manual", name: "milk", item_type: "long_shelf_life", min_stock: 0 },
  });
  expect(sent["milk x2"]).toEqual({
    quantity: 2,
    location_id: GARAGE,
    new_product: { from: "manual", name: "Almond Milk", item_type: "long_shelf_life", min_stock: 0 },
  });
  await expect(page.locator("#error")).toBeHidden();
});

// docs/specs/27-category-quick-create.md: js/tree-modal.js generalized to a
// "categories" kind, with the same "+ New category" escape hatch beside the
// New Item form's category field. "E2E Other Household" has no categories
// seeded, so this also exercises the modal's empty-tree message. The resolve
// itself is mocked (captureResolves), for the reason this file's own comment
// above CARD gives — only the category creation and its GET/POST are real.
//
// A line of its own ("dried oregano leaves"), distinct from every other
// line this file pastes: the real journey-6 test above resolves "smoked
// paprika" for real, and fullyParallel gives no guarantee that runs before
// or after this one — reusing its text could turn this line into an
// exact_match against that just-created product depending on run order.
test("a category can be created from the new-product form, without leaving the screen", async ({ page }) => {
  let rawTextById = {};
  const sent = await captureResolves(page, () => rawTextById);

  const created = await pasteList(page, ["dried oregano leaves"]);
  expect(created.items[0].status).toBe("new_item");
  rawTextById = Object.fromEntries(created.items.map((item) => [item.id, item.raw_text]));
  const line = lineFor(page, "dried oregano leaves");

  // No catalog and no local match, so the new-product form is already open.
  await expect(line.locator('[data-field="new-product"]')).toBeVisible();
  await expect(line.locator('[data-field="category"] option')).toHaveCount(1); // "No category" alone

  // Filled in BEFORE the modal opens: closing it must leave every other field
  // on this row exactly as the user left it (spec 27, "no loss of any other
  // field already filled in"), which only shows if there is something to lose.
  await line.locator('[data-field="location"]').selectOption(GARAGE);
  await line.locator('[data-field="name"]').fill("Dried Oregano");

  await line.locator('[data-field="category-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Categories" });
  await expect(dialog).toContainText("No categories yet");

  await dialog.getByRole("button", { name: "Add top-level category" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level category"]').fill("Spices");
  await dialog.locator("#category-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Spices");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Immediately selectable — no reload — and it is what the resolve body
  // carries once the line is completed against it.
  await expect(line.locator('[data-field="category"]')).not.toHaveValue("");
  const categoryId = await line.locator('[data-field="category"]').inputValue();
  await expect(line.locator('[data-field="location"]')).toHaveValue(GARAGE);
  await expect(line.locator('[data-field="name"]')).toHaveValue("Dried Oregano");

  await line.locator('[data-action="resolve"]').click();
  await expect(line.locator('[data-field="status"]')).toHaveText("Done");

  expect(sent["dried oregano leaves"]).toEqual({
    quantity: 1,
    location_id: GARAGE,
    new_product: {
      from: "manual",
      name: "Dried Oregano",
      item_type: "long_shelf_life",
      min_stock: 0,
      category_id: categoryId,
    },
  });
});

// docs/specs/26-location-quick-create.md: in a storage with zero locations,
// the "+ New location" escape hatch is what makes a line resolvable at all —
// without it "Choose a location…" would be the only option, forever.
test("a location can be created from the resolution screen, in a storage seeded with zero locations", async ({
  page,
}) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-casey", password: PASSWORD },
  });
  expect(login.status(), "fixture login").toBe(200);

  await page.goto(`/shopping-list.html?storage=${ZERO_LOCATIONS_HOUSEHOLD}`);
  await expect(page.locator("#compose")).toBeVisible();
  await page.fill("#raw-text", "birthday candles");
  await page.click("#submit");
  await expect(page.locator("#results")).toBeVisible();

  const line = lineFor(page, "birthday candles");
  // No catalog and no local product in this storage, so the new-product form
  // is already open, named from the line itself — nothing to click before
  // the location field matters.
  await expect(line.locator('[data-field="new-product"]')).toBeVisible();
  await expect(line.locator('[data-field="location"] option')).toHaveCount(1); // the placeholder alone

  await line.locator('[data-field="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toContainText("No locations yet");

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Balcony Box");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Balcony Box");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // The location just created is immediately selectable — no reload — and is
  // what the resolve endpoint (07's own, not a second creation path) is then
  // called against, completing the line.
  await expect(line.locator('[data-field="location"]')).not.toHaveValue("");
  await line.locator('[data-action="resolve"]').click();
  await expect(line.locator('[data-field="status"]')).toHaveText("Done");

  const reread = await page.request.get(`/api/storages/${ZERO_LOCATIONS_HOUSEHOLD}/locations`);
  const tree = await reread.json();
  expect(tree.items.map((n) => n.name)).toContain("Balcony Box");
});
