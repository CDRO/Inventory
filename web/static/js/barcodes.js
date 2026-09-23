// The instance-wide barcode hot cache (docs/specs/24-barcode-hot-cache.md).
//
// A perceived-latency accelerant only, never a second source of truth. The
// `hotBarcodes` IndexedDB store here is refreshed wholesale from
// GET /api/barcodes/hot and read from when a barcode is scanned — but it is
// never written to from a scan, and the authoritative per-storage lookup
// (docs/specs/20-barcode-recall.md) is still called for every scan, hit or
// miss against this cache. Its answer always wins when it differs from what
// is cached here; this module supplies an optimistic preview only, never the
// data an add/log sheet is built from.

import { get } from "./api.js";

const DB_NAME = "inventory-hot-barcodes";
const DB_VERSION = 1;
const STORE_NAME = "hotBarcodes";
const LAST_REFRESHED_KEY = "inventory:hotBarcodesRefreshedAt";
const REFRESH_AFTER_MS = 24 * 60 * 60 * 1000;

function openDB() {
  return new Promise((resolve, reject) => {
    if (typeof indexedDB === "undefined") {
      reject(new Error("indexedDB unavailable"));
      return;
    }
    const request = indexedDB.open(DB_NAME, DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME, { keyPath: "barcode" });
      }
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

// localStorage can throw (a private window, blocked site data) — the cache
// still has to degrade to "just fetch again", never to a broken scan flow.
function safeGetItem(key) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function safeSetItem(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Worst case: the next page load refreshes again, which costs one extra
    // request and nothing else.
  }
}

function replaceAll(db, items) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readwrite");
    const store = tx.objectStore(STORE_NAME);
    store.clear();
    for (const item of items) store.put(item);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

/**
 * ensureHotBarcodesFresh refreshes the local `hotBarcodes` store when it is
 * missing or older than 24 hours, replacing it wholesale — never merged or
 * diffed, since 500 rows of display-field data is small and this tolerant of
 * staleness (docs/specs/24-barcode-hot-cache.md).
 *
 * A fetch failure (offline, server error) leaves whatever is already cached
 * in place, however old, and is never surfaced as an error: this is a
 * nicety, and its complete absence must degrade to exactly spec 20's
 * existing scan behaviour, never to a broken one.
 *
 * @returns {Promise<void>}
 */
export async function ensureHotBarcodesFresh() {
  const last = Number(safeGetItem(LAST_REFRESHED_KEY) || 0);
  if (Date.now() - last < REFRESH_AFTER_MS) return;

  let items;
  try {
    ({ items } = await get("/api/barcodes/hot"));
  } catch {
    return;
  }

  try {
    const db = await openDB();
    await replaceAll(db, items);
    db.close();
    safeSetItem(LAST_REFRESHED_KEY, String(Date.now()));
  } catch {
    // No IndexedDB (an old browser, a locked-down private window): the
    // instant preview is simply never available, which is the same
    // "behaves exactly as if this spec did not exist" degradation.
  }
}

/**
 * lookupHotBarcode reads one cached entry, or null on a miss, a cache that
 * has never been populated, or an environment with no usable IndexedDB.
 *
 * **Never write to this store from here or from any scan.** The cache is
 * populated only by ensureHotBarcodesFresh from the server's own count; a
 * locally decoded code is looked up against it, never inserted into it
 * (docs/specs/24-barcode-hot-cache.md).
 *
 * @param {string} code
 * @returns {Promise<{barcode: string, display_name: string, category_path: string|null,
 *   item_type: string, image_url: string|null, icon_name: string|null,
 *   default_shelf_life_days: number|null}|null>}
 */
export async function lookupHotBarcode(code) {
  try {
    const db = await openDB();
    const result = await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, "readonly");
      const request = tx.objectStore(STORE_NAME).get(code);
      request.onsuccess = () => resolve(request.result ?? null);
      request.onerror = () => reject(request.error);
    });
    db.close();
    return result;
  } catch {
    return null;
  }
}
