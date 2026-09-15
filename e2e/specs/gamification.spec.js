// Gamification's per-user opt-out (docs/specs/50-gamification-overview.md
// principle 4, docs/specs/52-gamification-quests-and-ui.md). The acceptance
// criterion is explicit: "With gamification_enabled = FALSE, no gamification
// request is made, no element is rendered, and every inventory feature
// behaves identically." The one request this module always makes — GET
// .../progress, to learn whether the layer is on at all — is the documented
// mechanism itself (docs/specs/52-gamification-quests-and-ui.md: "When GET
// .../progress returns 204 ..., js/gamification.js renders nothing at all");
// what "no gamification request" rules out is everything *past* that check:
// quests, achievements, the leaderboard, settings.
//
// docs/specs/00-overview.md's e2e journeys, like every other spec file under
// e2e/specs/, run only inside the throwaway Playwright container in
// docker-compose.e2e.yml (see that file's header comment) — never as part of
// `go test ./...` or the `test` GitHub Actions workflow, and not executed in
// the sandbox this spec was authored in (issue #48: no Docker daemon here).

import { test, expect } from "@playwright/test";

// These three tests toggle /api/me/preferences for e2e-bob and e2e-alice —
// there are only three fixture users in total (e2e/fixtures/seed.sql), and
// the first two tests both need bob's global gamification_enabled flag in a
// known state to make an exact-request-count assertion. Run serially so one
// test's toggle can never land mid-assertion in another, which
// playwright.config.js's default fullyParallel would otherwise allow —
// confirmed live: without this, "FALSE: only the one gate check fires" and
// "TRUE: the ring and card render" raced on bob's row and the FALSE test
// observed quest/achievement requests that were actually the TRUE test's.
test.describe.configure({ mode: "serial" });

const GAMIFICATION_PATHS = ["/progress", "/quests", "/achievements", "/gamification/settings"];

function isGamificationRequest(url) {
  return GAMIFICATION_PATHS.some((path) => url.includes(path));
}

test("gamification_enabled = FALSE: only the one gate check fires, and nothing renders", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  const disable = await page.request.put("/api/me/preferences", {
    data: { gamification_enabled: false, holiday_weeks: [] },
  });
  expect(disable.status()).toBe(200);

  const gamificationRequests = [];
  page.on("request", (req) => {
    if (isGamificationRequest(req.url())) gamificationRequests.push(req.url());
  });

  await page.goto("/dashboard.html");
  // The dashboard's own load-time calls (reorder, analytics) settle around
  // the same time as the gamification check; waiting for the reorder list
  // gives the progress fetch time to have resolved either way.
  await expect(page.locator("#out-of-stock-list")).not.toContainText("Loading…");

  const progressRequests = gamificationRequests.filter((url) => url.includes("/progress"));
  const beyondTheGateCheck = gamificationRequests.filter((url) => !url.includes("/progress"));

  expect(progressRequests.length, "the one gate check must still happen").toBe(1);
  expect(beyondTheGateCheck, "no quest, achievement or settings request may follow a disabled gate check").toEqual(
    [],
  );

  await expect(page.locator("#gamification-ring")).toBeHidden();
  await expect(page.locator("#gamification-card")).toBeHidden();
  await expect(page.locator("#gamification-card")).toBeEmpty();
});

test("gamification_enabled = TRUE: the ring and card render from real data", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  const enable = await page.request.put("/api/me/preferences", {
    data: { gamification_enabled: true, holiday_weeks: [] },
  });
  expect(enable.status()).toBe(200);

  await page.goto("/dashboard.html");

  await expect(page.locator("#gamification-ring")).toBeVisible();
  await expect(page.locator("#gamification-card")).toBeVisible();
  await expect(page.locator("#gamification-card")).not.toBeEmpty();
});

test("turning gamification off in settings.html persists across a reload", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-alice", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);
  // Start from a known state regardless of what an earlier test left behind.
  await page.request.put("/api/me/preferences", { data: { gamification_enabled: true, holiday_weeks: [] } });

  await page.goto("/settings.html");
  await expect(page.locator("#gamification-enabled")).toBeChecked();

  await page.locator("#gamification-enabled").uncheck();
  await expect(page.locator("#error")).toBeHidden();

  await page.reload();
  await expect(page.locator("#gamification-enabled")).not.toBeChecked();

  // Leave the fixture user as we found it, for whichever test runs next.
  await page.request.put("/api/me/preferences", { data: { gamification_enabled: true, holiday_weeks: [] } });
});
