// Barcode hot cache (docs/specs/24-barcode-hot-cache.md).
//
// Everything here runs as Dana against "E2E Barcode Household"
// (e2e/fixtures/seed.sql), on its own two products so this file cannot
// collide with e2e/specs/barcode-recall.spec.js running in another worker.
//
// The one criterion no Go test can reach is the client-side half: the
// optimistic preview from the local `hotBarcodes` IndexedDB store, and that
// the authoritative per-storage lookup (docs/specs/20-barcode-recall.md)
// always wins when it differs from what was cached — proven here by seeding a
// wrong cached name for a code that is really associated with a different,
// real local product, and asserting the sheet the scan ends on shows the real
// one. Everything about scan_count itself (the increment, the ranking, the
// LIMIT 500, never serializing it) is a Go-level concern already covered in
// internal/store and internal/httpapi — real HTTP, a real Postgres, no
// browser needed for any of that.

import { test, expect } from "@playwright/test";

const BARCODE_HOUSEHOLD = "00000000-0000-7000-8000-000000000014";
const BASE = `/api/storages/${BARCODE_HOUSEHOLD}`;
const LARDER = "00000000-0000-7000-8000-000000000026";

const PREVIEW_PRODUCT = "00000000-0000-7000-8000-000000000051";
const FALLBACK_PRODUCT = "00000000-0000-7000-8000-000000000052";
const PREVIEW_CODE = "1234567890128";
const FALLBACK_CODE = "1234567890135";

const HOT_DB_NAME = "inventory-hot-barcodes";
const HOT_STORE_NAME = "hotBarcodes";

async function loginAsDana(page) {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-dana", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);
}

// seedHotBarcode writes one entry straight into the same IndexedDB store
// js/barcodes.js reads from — a raw open()/put() rather than importing that
// module, so the test does not depend on the shape of its internals, only on
// the store's name and key, which are this feature's actual public contract
// with the client-side cache.
async function seedHotBarcode(page, entry) {
  await page.evaluate(
    ([dbName, storeName, item]) =>
      new Promise((resolve, reject) => {
        const request = indexedDB.open(dbName, 1);
        request.onupgradeneeded = () => {
          const db = request.result;
          if (!db.objectStoreNames.contains(storeName)) {
            db.createObjectStore(storeName, { keyPath: "barcode" });
          }
        };
        request.onsuccess = () => {
          const db = request.result;
          const tx = db.transaction(storeName, "readwrite");
          tx.objectStore(storeName).put(item);
          tx.oncomplete = () => {
            db.close();
            resolve();
          };
          tx.onerror = () => reject(tx.error);
        };
        request.onerror = () => reject(request.error);
      }),
    [HOT_DB_NAME, HOT_STORE_NAME, entry],
  );
}

// selectStockingUpMode reveals the scan button: it is hidden under the
// default "shelf scan" mode, which has no barcode affordance at all.
async function selectStockingUpMode(page) {
  await page.locator('input[name="mode"][value="stocking_up"]').check();
}

// waitForHotBarcodesRefresh waits for the page's own on-load refresh
// (js/barcodes.js's ensureHotBarcodesFresh) to have fully finished writing to
// IndexedDB — the last-refreshed flag it leaves in localStorage is written
// strictly after that write completes, unlike the network response alone,
// which would still race the DB transaction that follows it.
async function waitForHotBarcodesRefresh(page) {
  await page.waitForFunction(() => localStorage.getItem("inventory:hotBarcodesRefreshedAt") !== null);
}

async function scanViaManualEntry(page, code) {
  await page.locator("#scan-barcode").click();
  const sheet = page.locator("dialog[aria-labelledby='barcode-sheet-title']");
  await expect(sheet).toBeVisible();
  await sheet.locator("#barcode-sheet-manual").fill(code);
  await sheet.getByRole("button", { name: "Use this", exact: true }).click();
}

test("GET /api/barcodes/hot answers for any session and never carries scan_count", async ({ page }) => {
  await loginAsDana(page);

  const res = await page.request.get("/api/barcodes/hot");
  expect(res.status()).toBe(200);
  const text = await res.text();
  expect(text).not.toContain("scan_count");
  expect(JSON.parse(text)).toHaveProperty("items");
});

test("an empty hot cache resolves a scan exactly as spec 20 already does, with no preview shown", async ({
  page,
}) => {
  await loginAsDana(page);
  expect(
    (await page.request.post(`${BASE}/products/${FALLBACK_PRODUCT}/barcodes`, { data: { barcode: FALLBACK_CODE } }))
      .status(),
  ).toBe(201);

  await page.goto(`/ingest.html?storage=${BARCODE_HOUSEHOLD}`);
  // Let the page's own on-load hot-cache refresh land before scanning — with
  // nothing seeded, the store it leaves behind is still empty for this code.
  await waitForHotBarcodesRefresh(page);

  await selectStockingUpMode(page);
  await scanViaManualEntry(page, FALLBACK_CODE);

  // No preview dialog for a code the local cache never held — the flow goes
  // straight to the authoritative sheet, same as before this spec existed.
  await expect(page.locator("dialog[aria-labelledby='barcode-preview-title']")).toHaveCount(0);
  const sheet = page.locator("dialog[aria-labelledby='quick-log-title']");
  await expect(sheet).toBeVisible();
  await expect(sheet).toContainText("Hot Cache Fallback Beans");

  await sheet.getByRole("button", { name: "Cancel", exact: true }).click();
});

// The acceptance criterion itself: "verified by a test that seeds a hot-cache
// hit whose authoritative answer differs (a local product exists) and
// asserts the local answer wins."
test("a hot-cache preview shows instantly, but the authoritative in-storage lookup always wins", async ({
  page,
}) => {
  await loginAsDana(page);
  expect(
    (await page.request.post(`${BASE}/products/${PREVIEW_PRODUCT}/barcodes`, { data: { barcode: PREVIEW_CODE } }))
      .status(),
  ).toBe(201);

  await page.goto(`/ingest.html?storage=${BARCODE_HOUSEHOLD}`);
  // Let the page's own on-load refresh finish before seeding, so it cannot
  // wipe out the entry this test is about to write underneath it.
  await waitForHotBarcodesRefresh(page);

  await seedHotBarcode(page, {
    barcode: PREVIEW_CODE,
    display_name: "Stale Cached Name — Not The Real Product",
    category_path: null,
    item_type: "long_shelf_life",
    image_url: null,
    icon_name: null,
    default_shelf_life_days: null,
  });

  // Delayed, not stubbed: the point is that the real answer arrives a moment
  // later and still wins, not that the client never asked for it.
  await page.route(`**${BASE}/barcodes/${PREVIEW_CODE}`, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    await new Promise((resolve) => setTimeout(resolve, 300));
    await route.continue();
  });

  await selectStockingUpMode(page);
  await scanViaManualEntry(page, PREVIEW_CODE);

  // The preview renders immediately, with no loading state, from the stale
  // cached name — while the authoritative call above is still in flight.
  const preview = page.locator("dialog[aria-labelledby='barcode-preview-title']");
  await expect(preview).toBeVisible();
  await expect(preview).toContainText("Stale Cached Name — Not The Real Product");

  // The authoritative answer arrives, the preview closes, and the sheet any
  // write would be built from shows the real local product — never the
  // cached one, and never anything for a person to have acted on in between.
  await expect(preview).toHaveCount(0);
  const sheet = page.locator("dialog[aria-labelledby='quick-log-title']");
  await expect(sheet).toBeVisible();
  await expect(sheet).toContainText("Hot Cache Preview Beans");
  await expect(sheet).not.toContainText("Stale Cached Name");

  await sheet.getByRole("button", { name: "Cancel", exact: true }).click();
});
