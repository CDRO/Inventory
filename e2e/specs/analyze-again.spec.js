// "Analyze again" on both review screens (docs/specs/09-consumption-logging.md):
// the one explicit, whole-job way a photo is ever analysed twice.
//
// The E2E stack has no working vision model and no seeded job has a photo on
// disk, so no job here can really be analysed again. What the stack does
// answer for real is the refusal of a job with no photo. Everything past that
// — the button, the confirmation, the unavailable-model message, and the new
// proposal replacing the old one — is the review screens' own behaviour, and is
// driven by answering the page's requests here. Requeueing and re-running the
// job is covered by the Go tests (internal/store/jobs_test.go,
// internal/jobs/runner_test.go, internal/httpapi/reanalyze_test.go).

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const LOOK_ONLY_JOB = "00000000-0000-7000-8000-000000000072";
const CONSUME_JOB = "00000000-0000-7000-8000-000000000073";
const YOGURT = "00000000-0000-7000-8000-000000000041";

async function logInAsBob(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

// jobAnswers serves GET /jobs/{id} from a list of job bodies, one per request,
// repeating the last — how a job looks as it moves from done, through pending,
// to done again.
async function jobAnswers(page, jobId, bodies) {
  let served = 0;
  await page.route(`**/jobs/${jobId}`, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    const body = bodies[Math.min(served, bodies.length - 1)];
    served += 1;
    await route.fulfill({ json: body });
  });
}

function shelfJob(status, rows) {
  return {
    id: LOOK_ONLY_JOB,
    kind: "shelf_ingestion",
    status,
    payload: status === "done" ? { mode: "shelf", location_hint_id: null, rows } : null,
    error: null,
    has_image: true,
    background_removal: false,
  };
}

function newItemRow(rowId, label, quantity) {
  return {
    row_id: rowId,
    label,
    confidence: 0.9,
    quantity,
    bounding_box: null,
    match: { status: "new_item", product: null, candidates: [], catalog: null },
    location: { path: [], location_id: null },
  };
}

test("a proposal without a photo offers no Analyze again, and the server refuses one", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  await expect(page.locator("#rows .review-row")).toHaveCount(2);
  await expect(page.locator("#reanalyze")).toBeHidden();

  // Asked anyway, the real server says no: there is nothing to analyse.
  const refused = await page.request.post(`/api/storages/${HOUSEHOLD}/jobs/${LOOK_ONLY_JOB}/reanalyze`);
  expect(refused.status()).toBe(409);
  const job = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${LOOK_ONLY_JOB}`);
  expect((await job.json()).status).toBe("done");
});

test("without a usable model, Analyze again says so and the proposal stays", async ({ page }) => {
  await logInAsBob(page);
  await jobAnswers(page, LOOK_ONLY_JOB, [
    shelfJob("done", [newItemRow("0", "Rice 1kg", 1), newItemRow("1", "Lentils 500g", 2)]),
  ]);
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/reanalyze`, (route) =>
    route.fulfill({
      status: 503,
      json: { error: { code: "model_unavailable", message: "The configured vision model is unavailable." } },
    }),
  );

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);
  await rows.nth(0).locator('[data-role="quantity"]').fill("5");

  const reanalyze = page.getByRole("button", { name: "Analyze again" });
  await expect(reanalyze).toBeVisible();
  page.once("dialog", (dialog) => dialog.accept());
  await reanalyze.click();

  await expect(page.locator("#error")).toContainText("AI model is unavailable");
  // Nothing on screen was lost: the rows, the reviewer's edit, and Confirm.
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0).locator('[data-role="quantity"]')).toHaveValue("5");
  await expect(page.getByRole("button", { name: "Confirm" })).toBeEnabled();
});

test("declining the confirmation analyses nothing", async ({ page }) => {
  await logInAsBob(page);
  await jobAnswers(page, LOOK_ONLY_JOB, [shelfJob("done", [newItemRow("0", "Rice 1kg", 1)])]);
  let asked = false;
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/reanalyze`, (route) => {
    asked = true;
    return route.fulfill({ status: 202, json: { job_id: LOOK_ONLY_JOB } });
  });

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  await expect(page.locator("#rows .review-row")).toHaveCount(1);
  page.once("dialog", (dialog) => dialog.dismiss());
  await page.getByRole("button", { name: "Analyze again" }).click();

  await expect(page.locator("#rows .review-row")).toHaveCount(1);
  expect(asked).toBe(false);
});

test("Analyze again replaces a shelf proposal with the new analysis", async ({ page }) => {
  await logInAsBob(page);
  await jobAnswers(page, LOOK_ONLY_JOB, [
    shelfJob("done", [newItemRow("0", "Rice 1kg", 1), newItemRow("1", "Lentils 500g", 2)]),
    shelfJob("pending"),
    shelfJob("done", [newItemRow("0", "Basmati Rice 1kg", 3)]),
  ]);
  let asked = 0;
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/reanalyze`, (route) => {
    asked += 1;
    expect(route.request().method()).toBe("POST");
    return route.fulfill({ status: 202, json: { job_id: LOOK_ONLY_JOB } });
  });

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Analyze again" }).click();

  // The old rows go at once; the new proposal arrives by polling, as after an
  // upload, and is the only thing left to review.
  await expect(rows).toHaveCount(1, { timeout: 10_000 });
  await expect(rows.nth(0)).toContainText("Basmati Rice 1kg");
  await expect(rows.nth(0).locator('[data-role="quantity"]')).toHaveValue("3");
  await expect(page.getByRole("button", { name: "Confirm" })).toBeVisible();
  expect(asked).toBe(1);
});

test("a failed analysis can be analysed again from its review screen", async ({ page }) => {
  await logInAsBob(page);
  const failed = { ...shelfJob("failed"), error: "The photo could not be analysed. Try uploading it again." };
  await jobAnswers(page, LOOK_ONLY_JOB, [failed, shelfJob("pending"), shelfJob("done", [newItemRow("0", "Rice 1kg", 1)])]);
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/reanalyze`, (route) =>
    route.fulfill({ status: 202, json: { job_id: LOOK_ONLY_JOB } }),
  );

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  await expect(page.locator("#status")).toContainText("could not be analysed");
  await expect(page.getByRole("button", { name: "Confirm" })).toBeHidden();

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Analyze again" }).click();

  await expect(page.locator("#rows .review-row")).toHaveCount(1, { timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Confirm" })).toBeVisible();
});

test("Analyze again replaces a consumption proposal with the new analysis", async ({ page }) => {
  await logInAsBob(page);
  const consumption = (status, rows) => ({
    id: CONSUME_JOB,
    kind: "consumption_photo",
    status,
    payload: status === "done" ? { rows } : null,
    error: null,
    has_image: true,
    background_removal: false,
  });
  await jobAnswers(page, CONSUME_JOB, [
    consumption("done", [
      {
        row_id: "0", label: "Empty Yogurt Pot", confidence: 0.88, quantity: 1, bounding_box: null,
        match: { status: "exact_match", product: { id: YOGURT, name: "Greek Yogurt" }, candidates: [] },
      },
      {
        row_id: "1", label: "Unlabeled Empty Jar", confidence: 0.3, quantity: 1, bounding_box: null,
        match: { status: "new_item", product: null, candidates: [] },
      },
    ]),
    consumption("pending"),
    consumption("done", [
      {
        row_id: "0", label: "Two Empty Yogurt Pots", confidence: 0.93, quantity: 2, bounding_box: null,
        match: { status: "exact_match", product: { id: YOGURT, name: "Greek Yogurt" }, candidates: [] },
      },
    ]),
  ]);
  await page.route(`**/jobs/${CONSUME_JOB}/reanalyze`, (route) =>
    route.fulfill({ status: 202, json: { job_id: CONSUME_JOB } }),
  );

  await page.goto(`/consume-review.html?storage=${HOUSEHOLD}&job=${CONSUME_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Analyze again" }).click();

  await expect(rows).toHaveCount(1, { timeout: 10_000 });
  await expect(rows.nth(0)).toContainText("Two Empty Yogurt Pots");
  await expect(rows.nth(0).locator('[data-role="product"]')).toHaveValue(`product:${YOGURT}`);
});
