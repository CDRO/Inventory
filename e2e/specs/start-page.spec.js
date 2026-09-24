// Navigation and the per-storage start page
// (docs/specs/34-navigation-and-start-page.md).
//
// Two fixture users, and they exist only for this file. `start_page` is
// durable: changing it leaves it changed for every later run and for every
// other spec that logs in as that user. e2e-alice and e2e-bob are shared by
// auth-journeys, storage-switching, categories and ingestion, all of which
// assert on where a login lands, so this file never touches theirs
// (e2e/fixtures/seed.sql).
//
//   e2e-start       — one storage, seeded on the default, and the only user
//                     whose start page any test here writes.
//   e2e-start-multi — two storages, seeded `inventory` in one and `locations`
//                     in the other. Read only, never written, which is what
//                     lets these run under playwright.config.js's
//                     fullyParallel alongside everything else.

import { test, expect } from "@playwright/test";

const PASSWORD = "e2e-fixture-password";
const START_STORAGE = "00000000-0000-7000-8000-000000000017";
const START_ONE = "00000000-0000-7000-8000-000000000018";
const START_TWO = "00000000-0000-7000-8000-000000000019";

async function logIn(page, username) {
  await page.goto("/index.html");
  await page.fill("#username", username);
  await page.fill("#password", PASSWORD);
  await page.click('button[type="submit"]');
}

// The acceptance criterion in full: the default is the dashboard, the choice
// is made on settings.html, and it survives signing out and back in — which
// is the whole reason it is a column and not a localStorage key.
test("a member changes their start page and the next login lands there", async ({ page }) => {
  await logIn(page, "e2e-start");

  await expect(page).toHaveURL(new RegExp(`/dashboard[.]html[?]storage=${START_STORAGE}$`));
  await expect(page.locator("#main h2").first()).toHaveText("Dashboard");

  await page.goto(`/settings.html?storage=${START_STORAGE}`);
  // The label names the storage being changed, so somebody in two households
  // can see which one this select concerns.
  await expect(page.locator("#start-page-label")).toHaveText("Start page for E2E Start");
  await expect(page.locator("#start-page")).toHaveValue("dashboard");

  await page.selectOption("#start-page", "inventory");
  await expect(page.locator("#start-page-status")).toHaveText("Saved.");
  await expect(page.locator("#error")).toBeHidden();

  await page.click("#nav #logout");
  await expect(page).toHaveURL(/\/$/); // index.html, canonicalised to "/" by the file server

  await logIn(page, "e2e-start");
  await expect(page).toHaveURL(new RegExp(`/inventory[.]html[?]storage=${START_STORAGE}$`));
  await expect(page.locator("#main h2").first()).toHaveText("Inventory");

  // Put the fixture back, so a re-run against a database that was not torn
  // down starts from the default again — the same thing seed.sql's DO UPDATE
  // does, done here as well because this test is the one that moved it.
  await page.goto(`/settings.html?storage=${START_STORAGE}`);
  await page.selectOption("#start-page", "dashboard");
  await expect(page.locator("#start-page-status")).toHaveText("Saved.");
});

// The preference is per membership, not per user: one person, two households,
// two different answers. An implementation that stored it on the user would
// pass the test above and fail this one.
test("each of one person's storages opens on its own start page", async ({ page }) => {
  await logIn(page, "e2e-start-multi");

  // Two memberships and nothing remembered, so the picker asks first.
  await expect(page.locator("#main")).toContainText("Choose a storage");
  await page.getByRole("button", { name: "E2E Start One" }).click();

  await expect(page).toHaveURL(new RegExp(`/inventory[.]html[?]storage=${START_ONE}$`));
  await expect(page.locator("#main h2").first()).toHaveText("Inventory");
});

test("the same person's other storage opens somewhere else entirely", async ({ page }) => {
  await logIn(page, "e2e-start-multi");

  await expect(page.locator("#main")).toContainText("Choose a storage");
  await page.getByRole("button", { name: "E2E Start Two" }).click();

  await expect(page).toHaveURL(new RegExp(`/locations[.]html[?]storage=${START_TWO}$`));
  await expect(page.locator("#main h2").first()).toHaveText("Locations");
});

// The bar itself: present on a storage-scoped page, marking where the user is,
// and carrying no route to the admin area. The last of those is also pinned
// server-side by web/embed_test.go, which scans every shipped asset for the
// path; this is the same claim checked against what the browser actually
// rendered.
test("the bar marks the current page and never links to the admin area", async ({ page }) => {
  await logIn(page, "e2e-start-multi");
  // logIn submits the form without awaiting the navigation it causes, so
  // the picker is what says the session exists. Navigating before it has
  // appeared would race the login and land on the login page instead.
  await expect(page.locator("#main")).toContainText("Choose a storage");
  await page.goto(`/locations.html?storage=${START_TWO}`);

  const nav = page.locator("#nav");
  await expect(nav).toBeVisible();
  await expect(nav.getByRole("link", { name: "Locations" })).toHaveAttribute("aria-current", "page");
  await expect(nav.getByRole("link", { name: "Dashboard" })).not.toHaveAttribute("aria-current", "page");

  // Every entry carries the storage, so no destination has to ask again.
  await expect(nav.getByRole("link", { name: "Products" })).toHaveAttribute(
    "href",
    new RegExp(`/products[.]html[?]storage=${START_TWO}$`),
  );

  // The wordmark is the home link and goes wherever this member chose, which
  // for this storage is the locations page.
  await expect(page.locator("#wordmark")).toHaveAttribute(
    "href",
    new RegExp(`/locations[.]html[?]storage=${START_TWO}$`),
  );

  const hrefs = await nav.getByRole("link").evaluateAll((links) => links.map((a) => a.getAttribute("href")));
  expect(hrefs.filter((href) => href && href.includes("/admin"))).toEqual([]);
});

// Every storage-scoped page marks its own entry, in the browser.
//
// web/nav_test.go cross-checks the `current` key each page module passes
// against nav.js's vocabulary, which catches a wrong or missing key in the
// merge gate. This is the other half: that the key actually becomes
// `aria-current="page"` on the rendered bar. Inbox earns its row especially —
// its entry is built by js/inbox-badge.js rather than by nav.js's own
// navLink, so it is the one entry whose marking goes through a second code
// path.
const PAGES = [
  { file: "dashboard.html", label: "Dashboard" },
  { file: "inventory.html", label: "Inventory" },
  { file: "products.html", label: "Products" },
  { file: "locations.html", label: "Locations" },
  { file: "categories.html", label: "Categories" },
  { file: "shopping-list.html", label: "Shopping list" },
  { file: "stocktake.html", label: "Stocktake" },
  { file: "ingest.html", label: "Scan" },
  { file: "inbox.html", label: "Inbox" },
  { file: "settings.html", label: "Settings" },
];

for (const { file, label } of PAGES) {
  test(`${file} marks its own entry in the bar`, async ({ page }) => {
    await logIn(page, "e2e-start-multi");
    await expect(page.locator("#main")).toContainText("Choose a storage");
    await page.goto(`/${file}?storage=${START_TWO}`);

    const nav = page.locator("#nav");
    await expect(nav).toBeVisible();
    await expect(nav.getByRole("link", { name: label, exact: true })).toHaveAttribute(
      "aria-current",
      "page",
    );
    // Exactly one entry is marked: a page that marked two would read as being
    // in two places at once to a screen reader.
    await expect(nav.locator('[aria-current="page"]')).toHaveCount(1);
  });
}

// At phone width the bar scrolls sideways and the page body does not. A bar
// that simply overflowed would look the same in a screenshot and make every
// page horizontally scrollable, which is the failure this pins.
test.describe("on a 375px phone", () => {
  test.use({ viewport: { width: 375, height: 720 } });

  test("the bar scrolls sideways and the page body does not", async ({ page }) => {
    await logIn(page, "e2e-start-multi");
    // logIn submits the form without awaiting the navigation it causes, so
    // the picker is what says the session exists. Navigating before it has
    // appeared would race the login and land on the login page instead.
    await expect(page.locator("#main")).toContainText("Choose a storage");
    await page.goto(`/locations.html?storage=${START_TWO}`);
    await expect(page.locator("#nav")).toBeVisible();

    const bar = await page.locator("#nav").evaluate((el) => ({
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      overflowX: getComputedStyle(el).overflowX,
    }));
    expect(bar.overflowX).toBe("auto");
    expect(bar.scrollWidth, "the bar has more entries than fit, so it must scroll").toBeGreaterThan(
      bar.clientWidth,
    );

    const body = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(body.scrollWidth, "the document must not scroll sideways").toBeLessThanOrEqual(
      body.clientWidth,
    );
  });
});
