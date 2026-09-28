// Mixed-batch photo classification
// (docs/specs/41-mixed-photo-classification.md): a shopping list photographed
// in the middle of a run of shelf photos is spotted by the analysis that
// already runs, surfaced as a banner on the review screen that photo would
// normally get, and acted on — or dismissed — by the person, once.
//
// The E2E stack's GEMINI_API_KEY is a placeholder, so no photo is ever really
// analysed here. The analysis responses are stubbed the way every other
// proposal in this suite is: seeded in e2e/fixtures/seed.sql in exactly the
// shape the analysis job writes, carrying looks_like_shopping_list: true and a
// non-empty shopping_list_lines. That is also why the multipart
// `source: "photo"` upload of docs/specs/07-shopping-list-reconciliation.md is
// not driven from here: the model check refuses it with 503 before the body is
// read, so the only thing this stack could assert about it is the refusal —
// its real coverage is internal/httpapi's handler tests.

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
// Both jobs' analyses flagged a shopping list. KEEP_JOB is only ever
// dismissed, so it survives every run; PROCESS_JOB is DISCARDED by the action
// this suite performs on it, so it belongs to that one test alone.
const KEEP_JOB = "00000000-0000-7000-8000-000000000076";
const PROCESS_JOB = "00000000-0000-7000-8000-000000000077";
// An ordinary shelf proposal, seeded for e2e/specs/ingestion.spec.js: the
// overwhelming majority case, which this spec must leave looking untouched.
const ORDINARY_JOB = "00000000-0000-7000-8000-000000000074";

async function logInAsBob(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

function reviewUrl(job) {
  return `/review.html?storage=${HOUSEHOLD}&job=${job}`;
}

test("a photo the analysis read as a list offers the choice on its normal review screen", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(reviewUrl(KEEP_JOB));

  const banner = page.locator("#shopping-list-banner");
  await expect(banner).toBeVisible();
  // The copy names the mode the photo was actually captured under, so a
  // person mid-run of twenty photos knows which one in the batch this is.
  await expect(banner).toContainText("not a shelf photo");
  await expect(banner.getByRole("button", { name: "Process as shopping list" })).toBeVisible();
  await expect(banner.getByRole("button", { name: "Keep as shelf photo" })).toBeVisible();

  // Nothing is rerouted: this is still the shelf review screen, and it never
  // navigated away on its own.
  expect(new URL(page.url()).pathname).toBe("/review.html");
  await expect(page.locator("#proposal")).toBeVisible();
});

test("an ordinary shelf photo shows no banner at all", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(reviewUrl(ORDINARY_JOB));

  // Wait for the proposal to actually render before concluding the banner is
  // absent, so this cannot pass merely by being early.
  await expect(page.locator("#rows .review-row").first()).toBeVisible();
  await expect(page.locator("#shopping-list-banner")).toBeHidden();
});

test("keeping it as a shelf photo dismisses the banner and leaves the empty review intact", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(reviewUrl(KEEP_JOB));

  const banner = page.locator("#shopping-list-banner");
  await expect(banner).toBeVisible();
  await banner.getByRole("button", { name: "Keep as shelf photo" }).click();

  await expect(banner).toBeHidden();
  // The job proceeds through its ordinary review, unmodified by this spec.
  // Its items came back empty — the common case for a genuine list misread as
  // a shelf — which is simply an empty proposal, exactly as a blurry photo
  // produces today.
  await expect(page.locator("#status")).toContainText("Nothing was found in this photo");
  await expect(page.getByRole("button", { name: "Confirm" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Discard" })).toBeVisible();
  expect(new URL(page.url()).pathname).toBe("/review.html");

  // Dismissing changed nothing on the server: the job is still there, still
  // flagged, and the offer comes back on the next visit.
  const job = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${KEEP_JOB}`);
  expect(job.status()).toBe(200);
  expect((await job.json()).status).toBe("done");
});

test("processing it as a shopping list lands on a real, resolvable list and discards the job", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(reviewUrl(PROCESS_JOB));

  const banner = page.locator("#shopping-list-banner");
  await expect(banner).toBeVisible();
  await banner.getByRole("button", { name: "Process as shopping list" }).click();

  // The same place a person lands after any other list ingestion.
  await page.waitForURL(/\/shopping-list\.html\?.*list=/);
  const listId = new URL(page.url()).searchParams.get("list");
  expect(listId).toBeTruthy();

  // A real list: every extracted line is a row, resolved through the same
  // matching service every other shopping list uses — "canned tomatoes"
  // matches this storage's own Canned Tomatoes.
  const items = page.locator("#items .item");
  await expect(items).toHaveCount(3);
  await expect(items.nth(0)).toContainText("canned tomatoes");
  await expect(items.nth(0).locator('[data-field="status"]')).toHaveText("Match");
  await expect(items.nth(1)).toContainText("oat milk");
  await expect(items.nth(2)).toContainText("sourdough bread");

  // The origin job is discarded, and reclassifying is not idempotent: the
  // same call again is the 404 an id that names nothing gets.
  const gone = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${PROCESS_JOB}`);
  expect(gone.status()).toBe(404);
  const again = await page.request.post(`/api/storages/${HOUSEHOLD}/shopping-lists`, {
    data: { from_job_id: PROCESS_JOB },
  });
  expect(again.status()).toBe(404);

  // Deep-linkable: the resolution screen opens again from its own address,
  // which is what the banner navigated to.
  await page.goto(`/shopping-list.html?storage=${HOUSEHOLD}&list=${listId}`);
  await expect(page.locator("#items .item")).toHaveCount(3);
});
