import "../register-sw.js";

// Page module for ingest.html — the one camera entry point of
// docs/specs/09-consumption-logging.md, shared by shelf ingestion and product
// ingestion (docs/specs/06-vision-shelf-ingestion.md).
//
// Each selected photo is its own request and its own job, fired one after
// another as soon as the form is submitted. The page never waits for analysis:
// a 202 means the photo is safe on the server, and from then on the result
// lives in the inbox whether or not anyone stays here. Waiting for a result on
// this screen is offered per photo, as a convenience, never as the only way to
// get one.
//
// The mode — stocking up, using up, or shelf scan — is chosen before capture
// and persists across photos, pages and sessions via localStorage, defaulting
// to the last one used: a run of many photos in one direction asks zero
// questions.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
import { fetchLocations, appendLocationOptions } from "../location-options.js";
import { get, post, postForm, ApiError } from "../api.js";
import { pollJob, JobFailedError } from "../jobs.js";
import { el, fromTemplate, qs, qsa, text } from "../dom.js";
import { openScanSheet } from "../barcode.js";
import { ensureHotBarcodesFresh, lookupHotBarcode } from "../barcodes.js";
import { t, tCount, apiErrorMessage } from "../i18n.js";

// The endpoint each mode uploads to (docs/specs/09-consumption-logging.md's
// capture-mode table) and, for the ones docs/specs/06-vision-shelf-ingestion.md
// already defines, the location hint they accept. Using-up has neither: a
// consumption photo decrements batches that already have a location.
const MODES = {
  stocking_up: { endpoint: "ingest/product-photos", hasLocation: true },
  using_up: { endpoint: "consume/photos", hasLocation: false },
  shelf_scan: { endpoint: "ingest/shelf-photos", hasLocation: true },
};
const MODE_STORAGE_KEY = "inventory:capture-mode";
const DEFAULT_MODE = "shelf_scan";

const form = qs("#upload-form");
const submitButton = qs("#submit");
const photosInput = qs("#photos");
const locationField = qs("#location-field");
const locationSelect = qs("#location");
const uploadsList = qs("#uploads");
const uploadTemplate = qs("#upload-template");
const errorBox = qs("#error");
const scanField = qs("#scan-field");
const scanBarcodeButton = qs("#scan-barcode");

let storageId = null;

init();

async function init() {
  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    showError(err);
    return;
  }

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    location.assign("/storages.html");
    return;
  }
  storageId = resolved;
  rememberStorageId(storageId);
  if (new URLSearchParams(location.search).get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(qs("#storage-switcher"), { storages: me.storages, currentId: storageId });
  renderInboxLink(qs("#inbox-link"), storageId);
  initGamification(storageId);

  try {
    appendLocationOptions(locationSelect, await fetchLocations(storageId));
    // ?location= preselects a shelf, for a link that is already scoped to one.
    // Nothing links here with it yet; the parameter is what such a link uses.
    const preset = new URLSearchParams(location.search).get("location");
    if (preset && [...locationSelect.options].some((o) => o.value === preset)) {
      locationSelect.value = preset;
    }
  } catch (err) {
    showError(err);
  }

  setUpModeSelector();
  form.addEventListener("submit", onSubmit);
  scanBarcodeButton.addEventListener("click", onScanBarcode);

  // Fire-and-forget: warms the local hot-cache preview
  // (docs/specs/24-barcode-hot-cache.md) so it is ready by the time this
  // page's own scan button is used, without making page load wait on it.
  ensureHotBarcodesFresh();
}

// setUpModeSelector restores the last-used mode (defaulting on a first visit),
// persists a change immediately, and shows the location field only for the
// two modes that place something.
function setUpModeSelector() {
  const radios = qsa('input[name="mode"]', form);
  const stored = safeGetItem(MODE_STORAGE_KEY);
  const initial = MODES[stored] ? stored : DEFAULT_MODE;
  for (const radio of radios) {
    radio.checked = radio.value === initial;
    radio.addEventListener("change", () => {
      if (radio.checked) {
        safeSetItem(MODE_STORAGE_KEY, radio.value);
        syncLocationField(radio.value);
      }
    });
  }
  syncLocationField(initial);
}

function syncLocationField(mode) {
  locationField.hidden = !MODES[mode]?.hasLocation;
  syncScanField(mode);
}

function currentMode() {
  return new FormData(form).get("mode");
}

// localStorage can throw (a private window, blocked site data); the mode
// selector still has to work without it, just without the stickiness.
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
    // A viewer without storage just re-picks the mode next visit.
  }
}

async function onSubmit(event) {
  event.preventDefault();
  clearError();

  const files = [...photosInput.files];
  if (files.length === 0) return;

  const mode = currentMode();
  const { endpoint, hasLocation } = MODES[mode] || MODES[DEFAULT_MODE];
  const hint = hasLocation ? locationSelect.value : "";

  submitButton.disabled = true;
  // One after another rather than all at once: a phone on a weak connection
  // uploading six 8MB photos in parallel finishes none of them.
  for (const file of files) {
    const card = addUploadCard(file.name);
    await upload(card, endpoint, file, hint, mode);
  }
  submitButton.disabled = false;
  form.reset();
  setUpModeSelector();
}

async function upload(card, endpoint, file, hint, mode) {
  setStatus(card, t("ingest.status.uploading"), "");
  const body = new FormData();
  body.append("image", file);
  if (hint) body.append("location_id", hint);

  try {
    const { job_id: jobId } = await postForm(`/api/storages/${storageId}/${endpoint}`, body);
    // The code a scan could not resolve travels with the job it caused, so
    // review.html can offer it once the photo has identified the product
    // (docs/specs/20-barcode-recall.md's miss path). sessionStorage, not
    // localStorage: it belongs to this tab's trip through the flow and should
    // not outlive it.
    if (pendingScanCode) {
      try {
        sessionStorage.setItem(PENDING_BARCODE_PREFIX + jobId, pendingScanCode);
      } catch {
        // No session storage: the offer simply appears without a preset code,
        // which is the ordinary capture-time offer.
      }
      pendingScanCode = null;
    }
    setStatus(card, t("ingest.status.uploaded"), t("ingest.status.uploadedDetail"));
    setActions(card, [
      el("button", { type: "button", class: "btn btn--ghost", onclick: () => waitFor(card, jobId, mode) }, [
        text(t("ingest.actions.waitHere")),
      ]),
    ]);
  } catch (err) {
    setStatus(card, t("ingest.status.notUploaded"), uploadErrorMessage(err));
  }
}

async function waitFor(card, jobId, mode) {
  setActions(card, [el("span", { class: "spinner", "aria-hidden": "true" })]);
  try {
    const payload = await pollJob(storageId, jobId);
    const count = Array.isArray(payload?.rows) ? payload.rows.length : 0;
    setStatus(card, t("ingest.status.ready"), tCount("ingest.itemsFound", count));
    setActions(card, [
      el("a", { class: "btn btn--primary", href: reviewHref(jobId, mode) }, [text(t("ingest.actions.reviewNow"))]),
    ]);
  } catch (err) {
    if (err instanceof JobFailedError) {
      setStatus(card, t("ingest.status.failed"), err.message);
    } else {
      setStatus(card, t("ingest.status.unknown"), t("ingest.status.unknownDetail"));
    }
    setActions(card, []);
  }
}

function uploadErrorMessage(err) {
  if (!(err instanceof ApiError)) {
    return t("ingest.errors.network");
  }
  switch (err.code) {
    case "model_unavailable":
      // A configuration problem, not something wrong with the photo
      // (docs/specs/06-vision-shelf-ingestion.md).
      return t("ingest.errors.modelUnavailable");
    case "payload_too_large":
      return t("ingest.errors.payloadTooLarge");
    default:
      return apiErrorMessage(err);
  }
}

function reviewHref(jobId, mode) {
  const page = mode === "using_up" ? "/consume-review.html" : "/review.html";
  const url = new URL(withStorageParam(storageId, page), location.origin);
  url.searchParams.set("job", jobId);
  return url.pathname + url.search;
}

function addUploadCard(name) {
  const card = fromTemplate(uploadTemplate);
  qs('[data-role="name"]', card).textContent = name;
  uploadsList.prepend(card);
  return card;
}

function setStatus(card, status, detail) {
  qs('[data-role="status"]', card).textContent = status;
  qs('[data-role="detail"]', card).textContent = detail;
}

function setActions(card, nodes) {
  qs('[data-role="actions"]', card).replaceChildren(...nodes);
}

function showError(err) {
  errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("ingest.errors.network");
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}

// --- Scan-and-log (docs/specs/20-barcode-recall.md) -------------------------
//
// The scan respects the sticky capture mode above — that selector's whole
// point is that a run of scans asks no questions — and shelf scan has no
// barcode affordance at all, because a shelf is not a barcode.
//
// **Nothing is written by the scan itself.** Scanning opens a lookup (a GET)
// and then a sheet; only the sheet's confirm tap reaches
// POST …/barcodes/{code}/log, which writes the batch and its paired
// inventory_logs row in one transaction on the server.

// pendingScanCode holds a code whose product this storage does not know yet.
// docs/specs/20-barcode-recall.md: the miss lands on the single-product photo
// path "with the code carried through for post-confirm association". It is
// carried client-side, handed to the job it belongs to at upload time, and
// picked up again by review.html after that job's confirm.
let pendingScanCode = null;

const PENDING_BARCODE_PREFIX = "inventory:barcode-for-job:";

function syncScanField(mode) {
  scanField.hidden = mode === "shelf_scan";
}

async function onScanBarcode() {
  clearError();
  const code = await openScanSheet(storageId, { title: t("ingest.scanBarcode") });
  if (!code) return;

  // The hot-cache hit is an optimistic preview only (docs/specs/24-barcode-
  // hot-cache.md): a missing or stale local copy renders nothing here and
  // the flow below is exactly spec 20's — this call is never skipped, hit or
  // miss, and its answer always replaces whatever the preview showed.
  const cached = await lookupHotBarcode(code);
  const preview = cached ? showHotBarcodePreview(cached) : null;

  let hit;
  try {
    hit = await get(`/api/storages/${storageId}/barcodes/${encodeURIComponent(code)}`);
  } catch (err) {
    preview?.close();
    if (err instanceof ApiError && err.status === 404) {
      onUnknownCode(code);
      return;
    }
    showError(err);
    return;
  }
  preview?.close();

  if (hit.product) {
    await openQuickLog(code, hit.product);
    return;
  }
  if (hit.catalog_suggestion) {
    await onCatalogHit(code, hit.catalog_suggestion);
  }
}

// showHotBarcodePreview renders the cached card immediately, with no loading
// state, while the authoritative lookup above is still in flight
// (docs/specs/24-barcode-hot-cache.md). It offers no action and supplies no
// data any sheet is built from — only the authoritative answer does — so
// there is nothing here for a person to act on before that answer arrives,
// only something other than a blank screen between a scan and an answer.
function showHotBarcodePreview(card) {
  const titleId = "barcode-preview-title";
  const children = [el("h2", { id: titleId }, [text(card.display_name)])];
  if (card.category_path) {
    children.push(el("p", { class: "muted" }, [text(card.category_path)]));
  }
  children.push(el("p", { class: "empty-state", role: "status" }, [text(t("ingest.barcode.checking"))]));

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": titleId }, children);
  document.body.append(dialog);
  dialog.showModal();

  let closed = false;
  return {
    close() {
      if (closed) return;
      closed = true;
      dialog.close();
      dialog.remove();
    },
  };
}

// onUnknownCode is the miss: the system has never seen this product, so
// identification falls back to the one thing that can identify it — a photo
// (docs/specs/06-vision-shelf-ingestion.md). No external barcode database is
// consulted, here or anywhere: that remains a non-goal
// (docs/specs/00-overview.md).
function onUnknownCode(code) {
  pendingScanCode = code;
  errorBox.textContent = t("ingest.barcode.unknownCode");
  errorBox.hidden = false;
  photosInput.focus();
}

async function onCatalogHit(code, card) {
  const accepted = await openCatalogCard(card);
  if (!accepted) return;
  try {
    const product = await post(`/api/storages/${storageId}/barcodes/${encodeURIComponent(code)}/product`, {});
    await openQuickLog(code, product);
  } catch (err) {
    showError(err);
  }
}

// openCatalogCard shows the anonymous catalogue card and asks whether to adopt
// it. The card carries display fields only — no id, no timestamp, nothing
// derived from whoever described this product first
// (docs/specs/02-data-model.md) — and this renderer can only show what is in
// it.
function openCatalogCard(card) {
  const titleId = "barcode-card-title";
  const addButton = el("button", { type: "button", class: "btn btn--primary" }, [text(t("ingest.barcode.addToStorage"))]);
  const cancelButton = el("button", { type: "button", class: "btn btn--ghost" }, [text(t("common.cancel"))]);

  const details = [];
  if (card.category_path) details.push(el("p", { class: "muted" }, [text(card.category_path)]));
  if (Array.isArray(card.variants) && card.variants.length > 0) {
    details.push(el("p", { class: "muted" }, [text(t("ingest.barcode.alsoKnownAs", { variants: card.variants.join(", ") }))]));
  }

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": titleId }, [
    el("h2", { id: titleId }, [text(t("ingest.barcode.catalogTitle"))]),
    el("p", {}, [text(card.display_name)]),
    ...details,
    el("p", { class: "muted" }, [
      text(t("ingest.barcode.catalogHint")),
    ]),
    el("div", { class: "row" }, [addButton, cancelButton]),
  ]);

  return new Promise((resolve) => {
    let settled = false;
    const finish = (accepted) => {
      if (settled) return;
      settled = true;
      dialog.close();
      resolve(accepted);
    };
    addButton.addEventListener("click", () => finish(true));
    cancelButton.addEventListener("click", () => finish(false));
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog) finish(false);
    });
    dialog.addEventListener("close", () => {
      dialog.remove();
      if (!settled) {
        settled = true;
        resolve(false);
      }
    });
    document.body.append(dialog);
    dialog.showModal();
    addButton.focus();
  });
}

// openQuickLog is the review step docs/specs/20-barcode-recall.md requires.
// It is smaller than a job review because there is nothing probabilistic to
// review — but the invariant is the same one specs 06 and 09 hold: no scan
// mutates inventory by itself.
async function openQuickLog(code, product) {
  const mode = currentMode();
  return mode === "using_up" ? openUsingUpSheet(code, product) : openStockingUpSheet(code, product);
}

function sheetFrame(titleId, title, product, fields, confirmLabel, onConfirm) {
  const errorLine = el("div", { class: "alert", role: "alert", hidden: true });
  const confirmButton = el("button", { type: "submit", class: "btn btn--primary" }, [text(confirmLabel)]);
  const cancelButton = el("button", { type: "button", class: "btn btn--ghost" }, [text(t("common.cancel"))]);

  const form = el("form", { class: "stack" }, [
    ...fields,
    errorLine,
    el("div", { class: "row" }, [confirmButton, cancelButton]),
  ]);

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": titleId }, [
    el("h2", { id: titleId }, [text(title)]),
    el("p", { class: "muted" }, [text(t("ingest.barcode.stockLine", { name: product.name, stock: product.current_stock }))]),
    form,
  ]);

  return new Promise((resolve) => {
    let settled = false;
    const finish = (result) => {
      if (settled) return;
      settled = true;
      dialog.close();
      resolve(result);
    };

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      errorLine.hidden = true;
      confirmButton.disabled = true;
      try {
        finish(await onConfirm());
      } catch (err) {
        confirmButton.disabled = false;
        errorLine.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("ingest.errors.formNetwork");
        errorLine.hidden = false;
      }
    });
    cancelButton.addEventListener("click", () => finish(null));
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog) finish(null);
    });
    dialog.addEventListener("close", () => {
      dialog.remove();
      if (!settled) {
        settled = true;
        resolve(null);
      }
    });
    document.body.append(dialog);
    dialog.showModal();
  });
}

// openStockingUpSheet writes one inventory_batches row and one inventory_logs
// row with reason 'purchase' — the same shape a shopping-list line resolution
// writes (docs/specs/07-shopping-list-reconciliation.md).
//
// The expiry field is left empty unless a person types in it: an untouched
// field means "use the rules", which is what keeps the date 'derived' and so
// still reachable by the cascade in
// docs/specs/08-expiration-and-classification.md.
async function openStockingUpSheet(code, product) {
  const quantity = el("input", { type: "number", min: "1", step: "1", inputmode: "numeric", value: "1", id: "quick-qty" });
  const locations = el("select", { id: "quick-location" });
  locations.append(el("option", { value: "" }, [text(t("ingest.barcode.chooseLocation"))]));
  for (const option of locationSelect.options) {
    if (option.value) locations.append(el("option", { value: option.value }, [text(option.textContent)]));
  }
  // The product's most recent batch's location, when it has one — the sheet
  // should not ask a question the previous answer already settled.
  const recent = await mostRecentLocation(product.product_id);
  if (recent && [...locations.options].some((o) => o.value === recent)) locations.value = recent;

  const expiry = el("input", { type: "date", id: "quick-expiry" });

  const fields = [
    el("div", { class: "field" }, [el("label", { for: "quick-qty" }, [text(t("ingest.barcode.howMany"))]), quantity]),
    el("div", { class: "field" }, [el("label", { for: "quick-location" }, [text(t("ingest.barcode.where"))]), locations]),
    el("div", { class: "field" }, [
      el("label", { for: "quick-expiry" }, [text(t("ingest.barcode.expiresOptional"))]),
      expiry,
      el("p", { class: "muted" }, [text(t("ingest.barcode.expiryHint"))]),
    ]),
  ];

  const result = await sheetFrame(
    "quick-log-title",
    t("ingest.barcode.stockingUpTitle"),
    product,
    fields,
    t("ingest.barcode.addToInventory"),
    async () => {
      const body = {
        direction: "in",
        quantity: Number.parseInt(quantity.value, 10) || 0,
        location_id: locations.value || null,
      };
      if (expiry.value) body.expiration_date = expiry.value;
      return post(`/api/storages/${storageId}/barcodes/${encodeURIComponent(code)}/log`, body);
    },
  );
  if (result) announceLogged(product.name, t("ingest.barcode.nowInStock", { stock: result.current_stock }));
}

// openUsingUpSheet applies the decrement under docs/specs/09-consumption-logging.md's
// exact rules. Leaving the batch picker alone sends no `decrements` at all, and
// the server then allocates nearest-expiry first — the same allocation the
// consumption review proposes, computed in one place rather than two.
async function openUsingUpSheet(code, product) {
  const quantity = el("input", { type: "number", min: "1", step: "1", inputmode: "numeric", value: "1", id: "quick-qty" });
  const fields = [
    el("div", { class: "field" }, [el("label", { for: "quick-qty" }, [text(t("ingest.barcode.howManyUsed"))]), quantity]),
    el("p", { class: "muted" }, [
      text(t("ingest.barcode.usingUpHint")),
    ]),
  ];

  const result = await sheetFrame(
    "quick-log-title",
    t("ingest.barcode.usingUpTitle"),
    product,
    fields,
    t("ingest.barcode.removeFromInventory"),
    () =>
      post(`/api/storages/${storageId}/barcodes/${encodeURIComponent(code)}/log`, {
        direction: "out",
        quantity: Number.parseInt(quantity.value, 10) || 0,
      }),
  );
  if (result) announceLogged(product.name, t("ingest.barcode.nowInStock", { stock: result.current_stock }));
}

async function mostRecentLocation(productId) {
  try {
    const batches = await get(`/api/storages/${storageId}/products/${productId}/batches`);
    const items = Array.isArray(batches?.items) ? batches.items : [];
    if (items.length === 0) return null;
    // created_at descending: the last place this product was put is the one to
    // offer, not the one that expires soonest.
    const newest = items.reduce((a, b) => (a.created_at > b.created_at ? a : b));
    return newest.location_id;
  } catch {
    return null;
  }
}

function announceLogged(name, detail) {
  const card = addUploadCard(name);
  setStatus(card, t("ingest.status.logged"), detail);
  setActions(card, []);
}
