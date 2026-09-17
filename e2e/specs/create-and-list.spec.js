// Required journey 3 of docs/specs/05-frontend-pwa-foundations.md: "Create a
// location and a category; add a product; see it in the list."
//
// Two of those three are covered here. **Creating a category has no user
// interface and no route to call** — `POST .../categories` does not exist
// (internal/httpapi/router.go registers only `GET /categories` and
// `PATCH /categories/{id}/shelf-life`), and `store.CreateCategory` has no
// non-test caller at all. docs/specs/06-vision-shelf-ingestion.md anticipates
// the page ("The same component serves the category tree") and
// docs/specs/08-expiration-and-classification.md assumes it ("a user can edit
// them in the category tree without a code change"), but nothing builds it
// yet. That gap is issue #72; journey 3 cannot be completed until it lands,
// and writing a test that reached into the database to create
// a category would assert the journey works while the user-facing half of it
// does not exist.
//
// Everything below runs as Bob, the fixture's single-storage user, so no
// `?storage=` handling is in the way of what is being tested. Names are
// suffixed with a random token because playwright.config.js runs fullyParallel
// and these are the only tests in the suite that add rows to "E2E Household" —
// a fixed name would collide with itself on a re-run against a stack that was
// not torn down.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";

function unique(prefix) {
  return `${prefix} ${Math.random().toString(36).slice(2, 8)}`;
}

async function logIn(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: PASSWORD },
  });
  expect(res.status(), "fixture login").toBe(200);
}

test("a location is created from the page, nests a child, and survives a reload", async ({ page }) => {
  const parent = unique("Cellar");
  const child = unique("Wine rack");

  await logIn(page);
  await page.goto("/locations.html");
  // The seeded Pantry proves the tree finished loading, so the click below
  // cannot land on a half-rendered page.
  await expect(page.locator("#tree")).toContainText("Pantry");

  await page.click("#add-root");
  await page.fill("#add-root-name", parent);
  await page.click('#add-root-form button[type="submit"]');

  const parentNode = page.locator(".tree-node", { hasText: parent });
  await expect(parentNode).toHaveCount(1);

  // Add a child under the location just created — the nesting is the point of
  // a tree, and a flat list would pass an assertion that only checked both
  // names are somewhere on the page.
  await parentNode.getByRole("button", { name: "Add", exact: true }).click();
  const childInput = page.locator('#tree input[placeholder="New name"]');
  await childInput.fill(child);
  await page.locator("#tree button.btn--primary", { hasText: "Add" }).click();

  await expect(page.locator(".tree-node", { hasText: child })).toHaveCount(1);
  await expect(page.locator("#error")).toBeHidden();

  // A reload re-reads from the server, so what is on screen afterwards was
  // persisted rather than merely rendered.
  await page.reload();
  await expect(page.locator("#tree")).toContainText(parent);
  await expect(page.locator("#tree")).toContainText(child);

  // And the child really is under the parent: the parent's <li> owns a nested
  // list containing it, rather than both sitting at the top level.
  const parentItem = page.locator("#tree li", { has: page.locator(`.tree-node:has-text("${parent}")`) }).first();
  await expect(parentItem.locator(`ul .tree-node:has-text("${child}")`)).toHaveCount(1);
});

test("a product added from the dashboard appears in the reorder list", async ({ page }) => {
  // "Add a missing item" (docs/specs/10-reorder-and-shopping-export.md) is the
  // only route by which a person can create a product by name alone today. The
  // one other way a product comes into being is confirming a new item on an
  // ingestion proposal, covered by ingestion.spec.js. Resolving a shopping
  // line does not create one yet, although spec 07 says it should (#74).
  const product = unique("Quinoa Flakes");

  await logIn(page);
  await page.goto("/dashboard.html");
  await expect(page.locator("#out-of-stock-list")).not.toContainText("Loading…");

  await page.fill("#add-item-name", product);
  await page.click("#add-item-check");

  // Nothing in this storage resembles the name and the catalog is empty, so
  // the match lands on the new-item branch rather than offering a product to
  // adjust — which is what makes the confirm below a creation.
  await expect(page.locator("#add-item-result")).toContainText("Nothing known about this yet.");
  const confirm = page.locator("#add-item-result").getByRole("button", { name: "Add to reorder list" });
  await expect(confirm).toBeVisible();
  await confirm.click();

  // min_stock defaults to 1 and the new product has no batches, so current
  // stock is 0: the out-of-stock bucket by definition
  // (internal/httpapi/reorder.go's classifyReorder).
  await expect(page.locator("#out-of-stock-list")).toContainText(product);
  await expect(page.locator("#low-stock-list")).not.toContainText(product);
  await expect(page.locator("#error")).toBeHidden();

  // The name field is cleared on success, so a second item does not silently
  // re-submit the first one's name.
  await expect(page.locator("#add-item-name")).toHaveValue("");

  await page.reload();
  await expect(page.locator("#out-of-stock-list")).toContainText(product);
});
