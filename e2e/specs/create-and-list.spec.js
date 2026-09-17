// Required journey 3 of docs/specs/05-frontend-pwa-foundations.md: "Create a
// location and a category; add a product; see it in the list." One test per
// part, each through the page a person would use: locations.html,
// categories.html, and the dashboard's "Add a missing item".
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

test("a category is created from the page, nests a child, takes a shelf life, and survives a reload", async ({ page }) => {
  const parent = unique("Baking");
  const child = unique("Flour");

  // A node is found by its own name, never by text anywhere inside it: a
  // child's shelf-life label names the parent it inherits from ("120 days
  // (from Baking …)"), so filtering on the parent's name as plain text would
  // match the child's node too.
  const nodeNamed = (name) =>
    page.locator(".tree-node").filter({ has: page.locator(".tree-name", { hasText: new RegExp(`^${name}$`) }) });

  await logIn(page);
  await page.goto("/categories.html");
  // The seeded Canned Goods, with its own 730-day rule, proves the tree and
  // its shelf-life labels finished loading.
  await expect(nodeNamed("Canned Goods").locator(".tree-shelf-life")).toHaveText("730 days");

  await page.click("#add-root");
  await page.fill("#add-root-name", parent);
  await page.click('#add-root-form button[type="submit"]');

  const parentNode = nodeNamed(parent);
  await expect(parentNode).toHaveCount(1);
  // Nothing above a new top-level category sets a rule, so the label says
  // what does decide — never an empty field a person could read as "no expiry".
  await expect(parentNode.locator(".tree-shelf-life")).toHaveText("By item type");

  await parentNode.getByRole("button", { name: "Add", exact: true }).click();
  await page.locator('#tree input[placeholder="New name"]').fill(child);
  await page.locator("#tree button.btn--primary", { hasText: "Add" }).click();
  await expect(nodeNamed(child)).toHaveCount(1);

  // Set the parent's shelf life. A new category files no products yet, so the
  // cascade has nothing to move — and the page says so rather than nothing.
  await parentNode.locator(".tree-shelf-life").click();
  await parentNode.locator(".tree-shelf-life-input").fill("120");
  await parentNode.getByRole("button", { name: "Save" }).click();
  await expect(page.locator("#status")).toContainText("No existing expiry dates needed to change.");
  await expect(parentNode.locator(".tree-shelf-life")).toHaveText("120 days");
  await expect(page.locator("#error")).toBeHidden();

  // A reload re-reads from the server, so what follows was persisted.
  await page.reload();
  await expect(parentNode.locator(".tree-shelf-life")).toHaveText("120 days");

  // The child sits under the parent, not beside it, and inherits its rule —
  // the label names where the number comes from.
  await parentNode.locator(".tree-toggle").click();
  const parentItem = page.locator("#tree li").filter({ has: parentNode });
  const childNode = parentItem.locator("ul").locator(".tree-node").filter({ has: page.locator(".tree-name", { hasText: new RegExp(`^${child}$`) }) });
  await expect(childNode).toHaveCount(1);
  await expect(childNode).toBeVisible();
  await expect(childNode.locator(".tree-shelf-life")).toHaveText(`120 days (from ${parent})`);
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
