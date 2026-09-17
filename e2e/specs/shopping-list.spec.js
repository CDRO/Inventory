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
const PASSWORD = "e2e-fixture-password";
// Fixture product ids in OTHER_HOUSEHOLD (e2e/fixtures/seed.sql).
const SOURDOUGH_BREAD = "00000000-0000-7000-8000-000000000042";
const SOY_MILK = "00000000-0000-7000-8000-000000000044";

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
  // than inventing a product card.
  const newItem = lineFor(page, "smoked paprika");
  await expect(newItem.locator('[data-field="status"]')).toHaveText("New");
  await expect(newItem.locator('[data-field="detail"]')).toContainText("Nothing known about this yet.");

  // Resolve all three. The exact match carries an overridden quantity, so
  // `resolved_quantity` can be shown to record what was applied rather than
  // what was proposed (docs/specs/07-shopping-list-reconciliation.md).
  await exact.locator('[data-field="quantity"]').fill("3");
  await exact.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "sourdough bread").locator('[data-field="status"]')).toHaveText("Done");

  await candidates.filter({ hasText: "Soy Milk" }).click();
  await ambiguous.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "milk").locator('[data-field="status"]')).toHaveText("Done");

  await newItem.locator('[data-action="resolve"]').click();
  await expect(lineFor(page, "smoked paprika").locator('[data-field="status"]')).toHaveText("Done");

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
  // Deliberately NOT asserted: what confirming leaves behind. Spec 07 says
  // confirming an exact match writes a batch and a log, and confirming a new
  // item creates a product — neither is built yet (#74), so today every
  // confirm only marks its line resolved. Asserting that no-op here would
  // make this deployment gate fail the day the spec is implemented; the
  // inventory and product assertions belong in this test once #74 lands.
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
