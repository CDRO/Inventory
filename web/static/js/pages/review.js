import "../register-sw.js";

// Page module for review.html — reviewing one ingestion proposal
// (docs/specs/06-vision-shelf-ingestion.md).
//
// The rows, and the accept / correct / reject actions on them, come from the
// shared ReviewList (js/review.js), so this screen behaves like every other
// review screen. What is specific to ingestion lives here: the crop of each
// item from the photo, the product choice (an existing product, the catalog's
// description, or a new one), the location picker including locations the
// photo suggested that do not exist yet, and the expiry.
//
// Nothing is written until Confirm. The body sent then decides every row
// explicitly, because the server refuses a confirm that leaves any row out.
//
// "Analyze again" replaces the whole proposal with a new analysis of the same
// photo (docs/specs/09-consumption-logging.md), and a new product's picture
// can have its background removed when the deployment offers that.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { initGamification } from "../gamification.js";
import { ReviewList } from "../review.js";
import { fetchLocations, appendLocationOptions, refreshLocationOptions } from "../location-options.js";
import { openLocationManager } from "../location-modal.js";
import { fetchCategories, appendCategoryOptions } from "../category-options.js";
import { fetchProducts } from "../product-options.js";
import { get, post, del, ApiError } from "../api.js";
import { pollJob, JobFailedError, reanalyzeJob, reanalyzeFailureMessage } from "../jobs.js";
import { qs, qsa, clearChildren } from "../dom.js";

const statusLine = qs("#status");
const errorBox = qs("#error");
const proposalSection = qs("#proposal");
const rowsContainer = qs("#rows");
const confirmButton = qs("#confirm");
const reanalyzeButton = qs("#reanalyze");
const discardButton = qs("#discard");

let storageId = null;
let jobId = null;
let list = null;
// backgroundRemoval is the job's own word on whether a picture's background
// can be removed right now. Without it no such control is shown at all.
let backgroundRemoval = false;
// locations is this storage's flat tree, refreshed in place by onAddLocation
// below whenever js/location-modal.js resolves — not re-fetched by anything
// else, so every row's location field reads the same list.
let locations = [];
/**
 * cutout is the background-removed picture made for the row, if any: which
 * source it was cut from, and its id for the confirm.
 * @type {Map<string, {row: Object, el: Element, cutout: {source: string, id: string}|null}>}
 */
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
  initGamification(storageId);
  qs("#back").setAttribute("href", inboxHref());

  jobId = new URLSearchParams(location.search).get("job");
  if (!jobId) {
    setStatus("No proposal was named. Open one from your inbox.");
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
      setStatus("This proposal no longer exists. It may have been confirmed or discarded by someone else.");
      return;
    }
    showError(err);
    return;
  }

  switch (job.status) {
    case "pending":
      setStatus("This photo is still being analysed…");
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
      setStatus(job.error || "This photo could not be analysed.");
      proposalSection.hidden = false;
      confirmButton.hidden = true;
      // A failed analysis is exactly what analysing again is for.
      reanalyzeButton.hidden = !job.has_image;
      return;
    case "consumed":
      setStatus("This proposal has already been added to your inventory.");
      return;
    case "done":
      confirmButton.hidden = false;
      reanalyzeButton.hidden = !job.has_image;
      await render(job);
      return;
    default:
      setStatus("This proposal cannot be reviewed.");
  }
}

async function render(job) {
  let categories, products;
  try {
    [locations, categories, products] = await Promise.all([
      fetchLocations(storageId),
      fetchCategories(storageId),
      fetchProducts(storageId),
    ]);
  } catch (err) {
    showError(err);
    return;
  }

  const proposal = job.payload;
  backgroundRemoval = Boolean(job.background_removal);
  list = new ReviewList(rowsContainer, qs("#row-template"));
  list.onCorrect = onCorrect;
  list.setItems(proposal.rows);

  rows.clear();
  for (const row of proposal.rows) {
    const el = rowsContainer.querySelector(`[data-row-id="${CSS.escape(row.row_id)}"]`);
    rows.set(row.row_id, { row, el, cutout: null });
    setupRow(el, row, proposal, locations, categories, products, job.has_image);
  }

  if (proposal.rows.length === 0) {
    setStatus("Nothing was found in this photo. Confirm to clear it from your inbox, or discard it.");
  } else {
    statusLine.hidden = true;
  }
  proposalSection.hidden = false;
}

function setupRow(el, row, proposal, locations, categories, products, hasImage) {
  // The crop: the whole photo as a background, scaled and shifted so only the
  // item's bounding box shows. A product photo has no box and shows whole.
  const crop = qs('[data-role="crop"]', el);
  if (hasImage) {
    paintCrop(crop, row.bounding_box);
    crop.setAttribute("aria-label", `Photo of ${row.label}`);
  }
  // Without a photo the crop stays an empty placeholder, keeping every row's
  // columns aligned.

  setupPictureChoice(el, row, hasImage);

  qs('[data-role="confidence"]', el).textContent = `${Math.round((row.confidence || 0) * 100)}%`;

  setupProduct(el, row, products);
  appendCategoryOptions(qs('[data-role="new-product-category"]', el), categories);
  qs('[data-role="quantity"]', el).value = String(row.quantity);
  setupLocation(el, row, proposal, locations);

  const expiry = qs('[data-role="expiry"]', el);
  const noExpiry = qs('[data-role="no-expiry"]', el);
  noExpiry.addEventListener("change", () => {
    expiry.disabled = noExpiry.checked;
    if (noExpiry.checked) expiry.value = "";
  });
}

// paintCrop draws the job's photo into node as a background, scaled and
// shifted so only box shows. With no usable box the whole photo shows.
function paintCrop(node, box) {
  node.style.backgroundImage = `url("/api/storages/${storageId}/jobs/${jobId}/image")`;
  if (box && box.width > 0 && box.height > 0) {
    node.style.backgroundSize = `${100 / box.width}% ${100 / box.height}%`;
    const px = box.width >= 1 ? 0 : (box.x / (1 - box.width)) * 100;
    const py = box.height >= 1 ? 0 : (box.y / (1 - box.height)) * 100;
    node.style.backgroundPosition = `${px}% ${py}%`;
  } else {
    node.style.backgroundSize = "";
    node.style.backgroundPosition = "";
  }
}

// setupPictureChoice offers a new product a picture taken from this photo —
// its own crop, or the whole photo — with no further AI call
// (docs/specs/05-frontend-pwa-foundations.md, "Shared review component").
// The server cuts the picture; this only names which one. With no photo there
// is nothing to offer, and a row with no usable box has no crop to offer.
function setupPictureChoice(el, row, hasImage) {
  const field = qs('[data-role="new-product-image-field"]', el);
  if (!hasImage) {
    field.hidden = true;
    return;
  }
  const box = row.bounding_box;
  if (!(box && box.width > 0 && box.height > 0)) {
    qs('[data-role="new-product-image"] option[value="crop"]', el).remove();
  }
  if (backgroundRemoval) setupCutout(el, row);
}

// setupCutout offers to remove the background of the picture just chosen
// (docs/specs/09-consumption-logging.md): only once the reviewer has picked a
// picture from their own photo, never on its own, and with the result shown
// next to the original, which stays chosen until the other one is picked. If
// it fails for any reason the original is simply kept — this is a nicety, and
// must never stand in the way of confirming.
function setupCutout(el, row) {
  const wrap = qs('[data-role="cutout"]', el);
  const picture = qs('[data-role="new-product-image"]', el);
  const request = qs('[data-role="cutout-request"]', el);
  const status = qs('[data-role="cutout-status"]', el);
  const compare = qs('[data-role="cutout-compare"]', el);
  const [keepOriginal, keepCutout] = qsa('[data-role="cutout-keep"]', el);
  keepOriginal.name = keepCutout.name = `cutout-${row.row_id}`;
  const entry = rows.get(row.row_id);

  // A cutout belongs to the picture it was cut from: choosing another one
  // starts over.
  function reset() {
    entry.cutout = null;
    keepOriginal.checked = true;
    compare.hidden = true;
    status.hidden = true;
    request.hidden = false;
    request.disabled = false;
    wrap.hidden = picture.value === "";
  }

  picture.addEventListener("change", reset);
  request.addEventListener("click", async () => {
    const source = picture.value;
    if (!source) return;
    request.disabled = true;
    status.textContent = "Removing the background…";
    status.hidden = false;

    let result;
    try {
      result = await post(`/api/storages/${storageId}/ingest/${jobId}/cutouts`, { row_id: row.row_id, source });
    } catch {
      if (picture.value !== source) return;
      status.textContent = "The background could not be removed. The original picture is kept.";
      request.disabled = false;
      return;
    }
    if (picture.value !== source) return; // the reviewer moved on meanwhile

    entry.cutout = { source, id: result.cutout_id };
    paintCrop(qs('[data-role="cutout-original"]', el), source === "crop" ? row.bounding_box : null);
    qs('[data-role="cutout-image"]', el).src = result.url;
    keepOriginal.checked = true;
    status.hidden = true;
    request.hidden = true;
    compare.hidden = false;
  });

  reset();
}

function setupProduct(el, row, products) {
  const select = qs('[data-role="product"]', el);
  const match = row.match || {};
  const add = (value, label) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    select.append(option);
  };

  const alreadyListed = new Set();
  if (match.product) alreadyListed.add(match.product.id);
  for (const candidate of match.candidates || []) alreadyListed.add(candidate.id);

  if (match.product) add(`product:${match.product.id}`, `${match.product.name} (in your inventory)`);
  for (const candidate of match.candidates || []) {
    add(`product:${candidate.id}`, `${candidate.name} (in your inventory)`);
  }
  if (match.catalog) add("catalog", `New product: ${match.catalog.display_name}`);
  if (!match.catalog || match.catalog.display_name !== row.label) add("label", `New product: ${row.label}`);
  add("custom", "Something else…");

  // The rest of the storage's products, alphabetical (as ListProducts
  // returns them) — so a product the model didn't propose, or proposed with
  // low enough confidence to omit, is still one keystroke away. A plain
  // <select> already supports type-ahead by typing the first letters, which
  // is what makes this "autocomplete" without a custom widget.
  const rest = (products || []).filter((p) => !alreadyListed.has(p.id));
  if (rest.length > 0) {
    const group = document.createElement("optgroup");
    group.label = "All products";
    for (const p of rest) {
      const option = document.createElement("option");
      option.value = `product:${p.id}`;
      option.textContent = p.name;
      group.append(option);
    }
    select.append(group);
  }

  if (match.product) {
    select.value = `product:${match.product.id}`;
  } else if (match.candidates?.length) {
    select.value = `product:${match.candidates[0].id}`;
  } else if (match.catalog) {
    select.value = "catalog";
  } else {
    select.value = "label";
  }

  select.addEventListener("change", () => syncNewProduct(el, row));
  syncNewProduct(el, row);
}

// syncNewProduct shows the new-product fields when the choice creates one,
// prefilled from whatever the choice was.
function syncNewProduct(el, row) {
  const choice = qs('[data-role="product"]', el).value;
  const wrap = qs('[data-role="new-product"]', el);
  const name = qs('[data-role="new-product-name"]', el);
  const itemType = qs('[data-role="item-type"]', el);

  wrap.hidden = choice.startsWith("product:");
  if (choice === "catalog") {
    name.value = row.match.catalog.display_name;
    if (row.match.catalog.item_type) itemType.value = row.match.catalog.item_type;
  } else if (choice === "label") {
    name.value = row.label;
  }
}

// onCorrect is the "the photo got this wrong" action: type what it really is.
// Terminal, as spec 09 requires — nothing re-runs the model for this row.
function onCorrect(rowId, el) {
  const select = qs('[data-role="product"]', el);
  select.value = "custom";
  syncNewProduct(el, rows.get(rowId).row);
  const name = qs('[data-role="new-product-name"]', el);
  name.value = "";
  name.focus();
  list.markCorrected(rowId);
}

function setupLocation(el, row, proposal, locations) {
  const select = qs('[data-role="location"]', el);
  const placeholder = document.createElement("option");
  placeholder.value = "";
  placeholder.textContent = "Choose a location…";
  select.append(placeholder);

  const path = row.location?.path || [];
  const proposed = path.filter((segment) => segment.proposed);
  if (proposed.length > 0) {
    const option = document.createElement("option");
    option.value = "new";
    option.textContent = `Create: ${path.map((segment) => segment.name).join(" › ")}`;
    select.append(option);
  }
  appendLocationOptions(select, locations);

  const known = (id) => id && [...select.options].some((o) => o.value === id);
  if (known(row.location?.location_id)) {
    select.value = row.location.location_id;
  } else if (proposed.length > 0) {
    select.value = "new";
  } else if (known(proposal.location_hint_id)) {
    select.value = proposal.location_hint_id;
  }

  qs('[data-role="location-add"]', el).addEventListener("click", () => onAddLocation(select));
}

// onAddLocation opens the shared location editor (js/location-modal.js) and,
// once it closes, refreshes every row's location field from a single GET —
// never one request per row (docs/specs/26-location-quick-create.md). Only
// the field whose trigger was clicked is preselected, and only when exactly
// one location was created; every other field just gets the refreshed
// option list, its own current selection untouched.
async function onAddLocation(openedSelect) {
  const { createdIds } = await openLocationManager(storageId);
  try {
    locations = await fetchLocations(storageId);
  } catch (err) {
    showError(err);
    return;
  }
  const preselect = createdIds.length === 1 ? createdIds[0] : null;
  for (const { el } of rows.values()) {
    const select = qs('[data-role="location"]', el);
    refreshLocationOptions(select, locations, select === openedSelect && preselect ? preselect : select.value);
  }
}

// buildItems turns the screen into the confirm body, or marks what is missing.
function buildItems() {
  const items = [];
  let valid = true;

  for (const [rowId, { row, el, cutout }] of rows) {
    const rowError = qs('[data-role="row-error"]', el);
    rowError.hidden = true;

    if (list.decisionOf(rowId) === "reject") {
      items.push({ row_id: rowId, decision: "reject" });
      continue;
    }

    const problems = [];
    const item = { row_id: rowId, decision: "accept" };

    const choice = qs('[data-role="product"]', el).value;
    if (choice.startsWith("product:")) {
      item.product_id = choice.slice("product:".length);
    } else {
      const name = qs('[data-role="new-product-name"]', el).value.trim();
      if (!name) problems.push("Name the product.");
      item.new_product = { name, item_type: qs('[data-role="item-type"]', el).value };
      const categoryId = qs('[data-role="new-product-category"]', el).value;
      if (categoryId) item.new_product.category_id = categoryId;
      const picture = qs('[data-role="new-product-image"]', el).value;
      const keep = qs('[data-role="cutout-keep"]:checked', el);
      if (picture && cutout && cutout.source === picture && keep?.value === "cutout") {
        item.new_product.image = "cutout";
        item.new_product.cutout_id = cutout.id;
      } else if (picture) {
        item.new_product.image = picture;
      }
    }

    const quantity = Number.parseInt(qs('[data-role="quantity"]', el).value, 10);
    if (!Number.isInteger(quantity) || quantity < 1) problems.push("Quantity must be at least 1.");
    item.quantity = quantity;

    const where = qs('[data-role="location"]', el).value;
    if (where === "") {
      problems.push("Choose a location.");
    } else if (where === "new") {
      item.new_location = newLocationFor(row);
    } else {
      item.location_id = where;
    }

    // Absent: the usual shelf life applies. Present — a date, or null for
    // "does not expire" — is the reviewer's own decision.
    if (qs('[data-role="no-expiry"]', el).checked) {
      item.expiration_date = null;
    } else if (qs('[data-role="expiry"]', el).value) {
      item.expiration_date = qs('[data-role="expiry"]', el).value;
    }

    if (problems.length > 0) {
      rowError.textContent = problems.join(" ");
      rowError.hidden = false;
      valid = false;
    }
    items.push(item);
  }
  return valid ? items : null;
}

// newLocationFor turns a partly-proposed path into what confirm creates: the
// proposed names, below the deepest segment that already exists.
function newLocationFor(row) {
  const path = row.location.path;
  const firstProposed = path.findIndex((segment) => segment.proposed);
  const parent = firstProposed > 0 ? path[firstProposed - 1].location_id : null;
  return { parent_id: parent, names: path.slice(firstProposed).map((segment) => segment.name) };
}

async function onConfirm() {
  clearError();
  const items = buildItems();
  if (items == null) {
    showMessage("Some rows need attention before they can be added.");
    return;
  }

  setBusy(true);
  try {
    const result = await post(`/api/storages/${storageId}/ingest/${jobId}/confirm`, { items });
    // What the server actually wrote, not what the screen asked for: the inbox
    // reports it back so a reviewer sees the outcome of their confirm.
    location.assign(
      inboxHref({
        confirmed: String(result.batch_ids.length),
        products: String(result.products_created),
        locations: String(result.locations_created),
      }),
    );
  } catch (err) {
    setBusy(false);
    if (err instanceof ApiError && err.status === 409) {
      showMessage("This proposal has already been added to your inventory, or is no longer waiting for review.");
    } else if (err instanceof ApiError && err.status === 404) {
      showMessage("Something this review refers to no longer exists — a product or location may have been deleted. Reload and try again.");
    } else {
      showError(err);
    }
  }
}

// onReanalyze is "Analyze again": the same photo, a new proposal. It replaces
// everything on this screen, corrections included, so it asks first — and it
// is the only way the model ever sees this photo twice.
async function onReanalyze() {
  if (!window.confirm("Analyze this photo again? The current proposal, and any changes made to it here, will be replaced.")) {
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
  if (!window.confirm("Discard this photo and its proposal? Nothing will be added to your inventory.")) {
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
  showMessage(err instanceof ApiError ? err.message : "Could not reach the server. Check your connection and try again.");
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
