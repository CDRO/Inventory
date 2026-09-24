import "../register-sw.js";

// Page module for consume-review.html — reviewing one consumption proposal
// (docs/specs/09-consumption-logging.md).
//
// The rows, and the accept / correct / reject actions on them, come from the
// shared ReviewList (js/review.js), the same component review.html's page
// module uses for shelf and product ingestion — this screen behaves like
// every other review screen. What is specific to consumption lives here: a
// product picker limited to this storage's own products (consumption never
// creates one), and, once a product is known, the batches a decrement can be
// split across, defaulting to the AI-detected count taken from the
// nearest-expiring batch first.
//
// Nothing is written until Confirm. The body sent then decides every row
// explicitly, because the server refuses a confirm that leaves any row out.
// "Analyze again" replaces the whole proposal with a new analysis of the same
// photo, the same way review.html's does.
//
// Unlike review.js's and shopping-list.js's page modules, this one never
// calls location-options.js's openLocationField: this screen has no
// location field to attach that trigger to. Consumption only ever decrements
// an existing batch (renderBatches below reads each one's location_id only
// to display it), and the confirm body — {row_id, decision, product_id,
// decrements} — has no field a newly created location id could travel
// through. docs/specs/26-location-quick-create.md scopes its escape hatch to
// review.html and shopping-list.html for exactly this reason.

import { t, apiErrorMessage, formatDate } from "../i18n.js";
import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderNav, startPageFor } from "../nav.js";
import { initGamification } from "../gamification.js";
import { ReviewList } from "../review.js";
import { fetchProducts, fetchProductBatches } from "../product-options.js";
import { fetchLocations } from "../location-options.js";
import { get, post, del, ApiError } from "../api.js";
import { pollJob, JobFailedError, reanalyzeJob, reanalyzeFailureMessage } from "../jobs.js";
// el() is imported as buildEl: every function below already uses `el` as the
// parameter name for a row's own DOM element (mirroring review.html's page
// module), so the element-builder import is renamed to avoid shadowing it.
import { el as buildEl, text, clearChildren, qs, qsa } from "../dom.js";

const statusLine = qs("#status");
const errorBox = qs("#error");
const proposalSection = qs("#proposal");
const rowsContainer = qs("#rows");
const confirmButton = qs("#confirm");
const reanalyzeButton = qs("#reanalyze");
const discardButton = qs("#discard");
const productDatalist = qs("#product-datalist");

let storageId = null;
let jobId = null;
let list = null;
let products = [];
/** @type {Map<string, string>} location id to its full path, "Basement › Right Shelf". */
let locationNames = new Map();
/** @type {Map<string, {row: Object, el: Element}>} */
const rows = new Map();

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
  renderStorageSwitcher(qs("#storage-switcher"), { storages: me.storages, currentId: storageId });
  renderNav(qs("#nav"), {
    storageId,
    startPage: startPageFor(me.storages, storageId),
  });
  initGamification(storageId);
  qs("#back").setAttribute("href", inboxHref());

  jobId = new URLSearchParams(location.search).get("job");
  if (!jobId) {
    setStatus(t("consumeReview.noProposal"));
    return;
  }

  confirmButton.addEventListener("click", onConfirm);
  reanalyzeButton.addEventListener("click", onReanalyze);
  discardButton.addEventListener("click", onDiscard);
  await load();
}

async function load() {
  let job;
  try {
    job = await get(`/api/storages/${storageId}/jobs/${jobId}`);
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      setStatus(t("consumeReview.proposalGone"));
      return;
    }
    showError(err);
    return;
  }

  switch (job.status) {
    case "pending":
      setStatus(t("consumeReview.analyzing"));
      try {
        await pollJob(storageId, jobId);
      } catch (err) {
        if (!(err instanceof JobFailedError)) {
          showError(err);
          return;
        }
      }
      await load();
      return;
    case "failed":
      // job.error is server-authored text (docs/specs/19-localization.md:
      // the API stays English) — only the fallback default is translated.
      setStatus(job.error || t("consumeReview.analysisFailed"));
      proposalSection.hidden = false;
      confirmButton.hidden = true;
      // A failed analysis is exactly what analysing again is for.
      reanalyzeButton.hidden = !job.has_image;
      return;
    case "consumed":
      setStatus(t("consumeReview.alreadyApplied"));
      return;
    case "done":
      confirmButton.hidden = false;
      reanalyzeButton.hidden = !job.has_image;
      await render(job);
      return;
    default:
      setStatus(t("consumeReview.cannotReview"));
  }
}

async function render(job) {
  try {
    [products, locationNames] = await Promise.all([
      fetchProducts(storageId),
      fetchLocations(storageId).then((locations) => new Map(locations.map((l) => [l.id, l.path.join(" › ")]))),
    ]);
  } catch (err) {
    showError(err);
    return;
  }
  clearChildren(productDatalist);
  for (const p of products) {
    const option = document.createElement("option");
    option.value = p.name;
    productDatalist.append(option);
  }

  const proposal = job.payload;
  list = new ReviewList(rowsContainer, qs("#row-template"));
  list.onCorrect = onCorrect;
  list.setItems(proposal.rows);

  rows.clear();
  for (const row of proposal.rows) {
    const el = rowsContainer.querySelector(`[data-row-id="${CSS.escape(row.row_id)}"]`);
    rows.set(row.row_id, { row, el });
  }
  await Promise.all(proposal.rows.map((row) => setupRow(rows.get(row.row_id).el, row, job.has_image)));

  if (proposal.rows.length === 0) {
    setStatus(t("consumeReview.nothingFound"));
  } else {
    statusLine.hidden = true;
  }
  proposalSection.hidden = false;
}

async function setupRow(el, row, hasImage) {
  const crop = qs('[data-role="crop"]', el);
  if (hasImage) {
    crop.style.backgroundImage = `url("/api/storages/${storageId}/jobs/${jobId}/image")`;
    const box = row.bounding_box;
    if (box && box.width > 0 && box.height > 0) {
      crop.style.backgroundSize = `${100 / box.width}% ${100 / box.height}%`;
      const px = box.width >= 1 ? 0 : (box.x / (1 - box.width)) * 100;
      const py = box.height >= 1 ? 0 : (box.y / (1 - box.height)) * 100;
      crop.style.backgroundPosition = `${px}% ${py}%`;
    }
    crop.setAttribute("aria-label", t("consumeReview.photoOfAriaLabel", { label: row.label }));
  }

  qs('[data-role="confidence"]', el).textContent = `${Math.round((row.confidence || 0) * 100)}%`;

  setupProductSelect(el, row);
  await onProductChange(el, row);
}

// setupProductSelect offers the AI's match(es) as existing-product options,
// plus a search option — the only way to name a product here, since
// consumption never creates one.
function setupProductSelect(el, row) {
  const select = qs('[data-role="product"]', el);
  const match = row.match || {};
  const add = (value, label) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    select.append(option);
  };

  if (match.product) add(`product:${match.product.id}`, match.product.name);
  for (const candidate of match.candidates || []) {
    if (match.product && candidate.id === match.product.id) continue;
    add(`product:${candidate.id}`, candidate.name);
  }
  add("search", t("consumeReview.searchOption"));

  if (match.product) {
    select.value = `product:${match.product.id}`;
  } else if (match.candidates?.length) {
    select.value = `product:${match.candidates[0].id}`;
  } else {
    select.value = "search";
  }

  select.addEventListener("change", () => onProductChange(el, row));
}

// onProductChange shows or hides the search field and the "unrecognized"
// notice, and (re)loads the batch picker for whichever product is now named —
// or clears it while none is.
async function onProductChange(el, row) {
  const select = qs('[data-role="product"]', el);
  const searchWrap = qs('[data-role="product-search"]', el);
  const searchInput = qs('[data-role="product-input"]', el);
  const unrecognized = qs('[data-role="unrecognized"]', el);
  const match = row.match || {};

  if (select.value === "search") {
    searchWrap.hidden = false;
    const chosen = findProductByName(searchInput.value);
    // "Unrecognized" is about whether a product is currently named at all —
    // by the AI's own match or by this search — not just the AI's original
    // guess, or picking one here would never clear the notice.
    unrecognized.hidden = Boolean(match.product || match.candidates?.length || chosen);
    if (chosen) {
      await renderBatches(el, chosen.id, row.quantity);
    } else {
      clearBatches(el, t("consumeReview.searchHint"));
    }
    if (!searchInput.dataset.wired) {
      searchInput.dataset.wired = "1";
      searchInput.addEventListener("input", () => onProductChange(el, row));
    }
    return;
  }

  searchWrap.hidden = true;
  unrecognized.hidden = true;
  const productId = select.value.slice("product:".length);
  await renderBatches(el, productId, row.quantity);
}

function findProductByName(name) {
  const trimmed = name.trim();
  if (!trimmed) return null;
  return products.find((p) => p.name === trimmed) || null;
}

function clearBatches(el, message) {
  const container = qs('[data-role="batches"]', el);
  clearChildren(container);
  if (message) container.append(buildEl("p", { class: "empty-state" }, [text(message)]));
}

// renderBatches loads a product's batches and pre-allocates the AI-suggested
// count across them, nearest expiration first — the reviewer's starting
// point, not the final word: every input stays editable, and the total is
// whatever the edited inputs sum to.
async function renderBatches(rowEl, productId, suggestedQty) {
  const container = qs('[data-role="batches"]', rowEl);
  clearChildren(container);
  container.append(buildEl("p", { class: "empty-state" }, [text(t("consumeReview.loadingStock"))]));

  let batches;
  try {
    batches = await fetchProductBatches(storageId, productId);
  } catch {
    clearChildren(container);
    container.append(buildEl("p", { class: "empty-state" }, [text(t("consumeReview.stockLoadFailed"))]));
    return;
  }

  clearChildren(container);
  if (batches.length === 0) {
    container.append(buildEl("p", { class: "empty-state" }, [text(t("consumeReview.noStock"))]));
    return;
  }

  const alloc = greedyAllocate(batches, suggestedQty);
  const inputs = [];
  const total = buildEl("p", { class: "muted", "data-role": "batch-total" }, []);

  for (const batch of batches) {
    const input = document.createElement("input");
    input.type = "number";
    input.min = "0";
    input.max = String(batch.quantity);
    input.step = "1";
    input.inputMode = "numeric";
    input.value = String(alloc.get(batch.id) || 0);
    input.dataset.batchId = batch.id;
    input.addEventListener("input", updateTotal);
    inputs.push(input);

    const where = locationNames.get(batch.location_id) || t("consumeReview.unknownLocation");
    const when = batch.expiration_date
      ? t("consumeReview.expiresOn", {
          // expiration_date is a bare DATE (migrations/00002_core_schema.sql),
          // parsed here as UTC midnight — timeZone: "UTC" renders exactly the
          // calendar date the server sent, regardless of the viewer's own
          // timezone. Without it, anyone west of UTC would see every expiry
          // one day early.
          date: formatDate(new Date(batch.expiration_date), { dateStyle: "medium", timeZone: "UTC" }),
        })
      : t("consumeReview.noExpiryDate");
    container.append(
      buildEl("label", { class: "row row--between" }, [
        text(t("consumeReview.batchSummary", { where, when })),
        input,
        text(t("consumeReview.ofQuantity", { quantity: batch.quantity })),
      ]),
    );
  }
  container.append(total);

  function updateTotal() {
    const sum = inputs.reduce((acc, i) => acc + (Number.parseInt(i.value, 10) || 0), 0);
    total.textContent = t("consumeReview.totalToRemove", { sum });
  }
  updateTotal();
}

// greedyAllocate assigns the suggested quantity to the nearest-expiring
// batches first (docs/specs/09-consumption-logging.md's default), spilling
// into the next batch only once the current one is exhausted.
function greedyAllocate(batches, quantity) {
  const alloc = new Map();
  let remaining = Number.isInteger(quantity) && quantity > 0 ? quantity : 0;
  for (const batch of batches) {
    if (remaining <= 0) break;
    const take = Math.min(remaining, batch.quantity);
    alloc.set(batch.id, take);
    remaining -= take;
  }
  return alloc;
}

// onCorrect is the "the AI got this wrong" action: search for the real
// product. Terminal, as the spec requires — nothing here re-runs the model.
function onCorrect(rowId, el) {
  const select = qs('[data-role="product"]', el);
  select.value = "search";
  const input = qs('[data-role="product-input"]', el);
  input.value = "";
  onProductChange(el, rows.get(rowId).row);
  input.focus();
  list.markCorrected(rowId);
}

// buildItems turns the screen into the confirm body, or marks what is
// missing.
function buildItems() {
  const items = [];
  let valid = true;

  for (const [rowId, { row, el }] of rows) {
    const rowError = qs('[data-role="row-error"]', el);
    rowError.hidden = true;

    if (list.decisionOf(rowId) === "reject") {
      items.push({ row_id: rowId, decision: "reject" });
      continue;
    }

    const problems = [];
    const item = { row_id: rowId, decision: "accept" };

    const select = qs('[data-role="product"]', el);
    let productId = null;
    if (select.value === "search") {
      const chosen = findProductByName(qs('[data-role="product-input"]', el).value);
      if (chosen) productId = chosen.id;
      else problems.push(t("consumeReview.problems.pickProduct"));
    } else {
      productId = select.value.slice("product:".length);
    }
    if (productId) item.product_id = productId;

    const decrements = [];
    for (const input of qsa("[data-batch-id]", el)) {
      const quantity = Number.parseInt(input.value, 10);
      if (Number.isInteger(quantity) && quantity > 0) {
        decrements.push({ batch_id: input.dataset.batchId, quantity });
      }
    }
    if (decrements.length === 0) problems.push(t("consumeReview.problems.chooseBatch"));
    item.decrements = decrements;

    if (problems.length > 0) {
      rowError.textContent = problems.join(" ");
      rowError.hidden = false;
      valid = false;
    }
    items.push(item);
  }
  return valid ? items : null;
}

async function onConfirm() {
  clearError();
  const items = buildItems();
  if (items == null) {
    showMessage(t("consumeReview.someRowsNeedAttention"));
    return;
  }

  setBusy(true);
  try {
    const result = await post(`/api/storages/${storageId}/consume/photos/${jobId}/confirm`, { items });
    location.assign(inboxHref({ consumed: String(result.batch_ids.length) }));
  } catch (err) {
    setBusy(false);
    if (err instanceof ApiError && err.status === 409) {
      showMessage(t("consumeReview.alreadyAppliedConflict"));
    } else if (err instanceof ApiError && err.status === 404) {
      showMessage(t("consumeReview.referencedGone"));
    } else {
      showError(err);
    }
  }
}

// onReanalyze is "Analyze again": the same photo, a new proposal. It replaces
// everything on this screen, corrections included, so it asks first — and it
// is the only way the model ever sees this photo twice.
async function onReanalyze() {
  if (!window.confirm(t("consumeReview.reanalyzeConfirm"))) {
    return;
  }
  clearError();
  setBusy(true);
  try {
    await reanalyzeJob(storageId, jobId);
  } catch (err) {
    setBusy(false);
    showMessage(reanalyzeFailureMessage(err));
    return;
  }

  // The old proposal is gone on the server; take it off the screen too, then
  // wait for the new one the way a fresh upload is waited for.
  rows.clear();
  clearChildren(rowsContainer);
  list = null;
  proposalSection.hidden = true;
  setBusy(false);
  await load();
}

async function onDiscard() {
  if (!window.confirm(t("consumeReview.discardConfirm"))) {
    return;
  }
  clearError();
  setBusy(true);
  try {
    await del(`/api/storages/${storageId}/jobs/${jobId}`);
    location.assign(inboxHref());
  } catch (err) {
    setBusy(false);
    showError(err);
  }
}

function inboxHref(extra = {}) {
  const url = new URL(withStorageParam(storageId, "/inbox.html"), location.origin);
  for (const [key, value] of Object.entries(extra)) url.searchParams.set(key, value);
  return url.pathname + url.search;
}

function setBusy(busy) {
  confirmButton.disabled = busy;
  reanalyzeButton.disabled = busy;
  discardButton.disabled = busy;
}

function setStatus(message) {
  statusLine.textContent = message;
  statusLine.hidden = false;
}

function showMessage(message) {
  errorBox.textContent = message;
  errorBox.hidden = false;
}

function showError(err) {
  showMessage(err instanceof ApiError ? apiErrorMessage(err) : t("consumeReview.networkError"));
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
