// "Discard all" (docs/specs/32-inbox-discard-all.md): emptying the whole
// inbox in one confirmed step, rather than once per job.
//
// Runs against its own storage and user, "E2E Inbox" / e2e-inbox, seeded in
// e2e/fixtures/seed.sql. The journey deletes jobs outright, so it cannot
// share "E2E Household" with ingestion.spec.js or consumption.spec.js, which
// depend on their own seeded jobs there under playwright.config.js's
// fullyParallel.

import { test, expect } from "@playwright/test";

const E2E_INBOX = "00000000-0000-7000-8000-000000000015";
const PENDING_JOB = "00000000-0000-7000-8000-0000000000a4";
const FAILED_JOB = "00000000-0000-7000-8000-0000000000a5";
const DONE_JOB = "00000000-0000-7000-8000-0000000000a6";
const CONSUMED_PRODUCT = "00000000-0000-7000-8000-0000000000a1";
const CONSUMED_BATCH = "00000000-0000-7000-8000-0000000000a2";
// The done job is seeded with the latest created_at of the three jobs
// "Discard all" is allowed to touch, so it is the first one the inbox lists
// (newest first) and its created_at is the up_to boundary the client must
// send.
const EXPECTED_UP_TO = "2025-06-01T10:00:00Z";

async function logInAsE2eInbox(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-inbox", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

test("Discard all empties the inbox up to the server's own boundary, not the device clock, and leaves consumed data alone", async ({ page }) => {
  await logInAsE2eInbox(page);

  // The device clock is set an hour before every seeded job. If the client
  // used it instead of the server's created_at, the up_to it sends would
  // predate all three jobs and the intercepted value below would not match
  // EXPECTED_UP_TO.
  await page.clock.install({ time: new Date("2025-06-01T07:00:00Z") });

  let listedFirstCreatedAt = null;
  let sentUpTo = null;
  await page.route(`**/api/storages/${E2E_INBOX}/jobs?*`, async (route) => {
    if (route.request().method() === "DELETE") {
      sentUpTo = new URL(route.request().url()).searchParams.get("up_to");
    }
    const response = await route.fetch();
    if (route.request().method() === "GET" && listedFirstCreatedAt == null) {
      const body = await response.json();
      if (body.items.length > 0) listedFirstCreatedAt = body.items[0].created_at;
    }
    await route.fulfill({ response });
  });

  await page.goto(`/inbox.html?storage=${E2E_INBOX}`);
  await expect(page.locator(`[data-job-id="${PENDING_JOB}"]`)).toBeVisible();
  await expect(page.locator(`[data-job-id="${FAILED_JOB}"]`)).toBeVisible();
  await expect(page.locator(`[data-job-id="${DONE_JOB}"]`)).toBeVisible();

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Discard all" }).click();

  await expect(page.locator("#notice")).toContainText("Discarded 3 photos.");
  await expect(page.locator(".empty-state")).toBeVisible();
  await expect(page.locator("#discard-all")).toBeHidden();

  expect(listedFirstCreatedAt).toBe(EXPECTED_UP_TO);
  expect(sentUpTo).toBe(listedFirstCreatedAt);

  // The consumed job's own proposal was applied long before this journey
  // ran; "Discard all" only ever deletes from jobs, so the batch it produced
  // must still be there.
  const batches = await page.request.get(`/api/storages/${E2E_INBOX}/products/${CONSUMED_PRODUCT}/batches`);
  expect(batches.status()).toBe(200);
  const body = await batches.json();
  expect(body.items.map((b) => b.id)).toContain(CONSUMED_BATCH);
});
