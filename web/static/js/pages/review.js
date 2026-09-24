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

import { t, apiErrorMessage } from "../i18n.js";
import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { initGamification } from "../gamification.js";
import { ReviewList } from "../review.js";
import { fetchLocations, appendLocationOptions, openLocationField } from "../location-options.js";
import { fetchCategories, appendCategoryOptions, openCategoryField } from "../category-options.js";
import { fetchProducts } from "../product-options.js";
import { get, post, del, ApiError } from "../api.js";
import { pollJob, JobFailedError, reanalyzeJob, reanalyzeFailureMessage } from "../jobs.js";
import { qs, qsa, clearChildren } from "../dom.js";
import { offerBarcodeCapture, offerScannedBarcode } from "../barcode-offer.js";

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
// locations is this storage's flat tree as of the initial render, used only
// to seed each row's location field once. A "+ New location" trigger's own
// refresh (location-options.js's openLocationField) fetches its own fresh
// copy directly into the affected <select>s rather than updating this one.
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
    setStatus(t("review.noProposal"));
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
      setStatus(t("review.proposalGone"));
      return;
    }
    showError(err);
    return;
  }

  switch (job.status) {
    case "pending":
      setStatus(t("review.analyzing"));
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
      setStatus(job.error || t("review.analysisFailed"));
      proposalSection.hidden = false;
      confirmButton.hidden = true;
      // A failed analysis is exactly what analysing again is for.
      reanalyzeButton.hidden = !job.has_image;
      return;
    case "consumed":
      setStatus(t("review.alreadyAdded"));
      return;
    case "done":
      confirmButton.hidden = false;
      reanalyzeButton.hidden = !job.has_image;
      await render(job);
      return;
    default:
      setStatus(t("review.cannotReview"));
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
    setStatus(t("review.nothingFound"));
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
    crop.setAttribute("aria-label", t("review.photoOfAriaLabel", { label: row.label }));
  }
  // Without a photo the crop stays an empty placeholder, keeping every row's
  // columns aligned.

  setupPictureChoice(el, row, hasImage);

  qs('[data-role="confidence"]', el).textContent = `${Math.round((row.confidence || 0) * 100)}%`;

  setupProduct(el, row, products);
  setupCategory(el, categories);
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

  // The <img alt> for the without-background picture: static in the
  // template's markup, but img `alt` has no data-i18n-* hook in i18n.js
  // (only textContent/placeholder/title/aria-label do) — set directly here
  // instead of adding one more attribute kind to that shared file.
  qs('[data-role="cutout-image"]', el).alt = t("review.row.withoutBackgroundAlt");

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
    status.textContent = t("review.removingBackground");
    status.hidden = false;

    let result;
    try {
      result = await post(`/api/storages/${storageId}/ingest/${jobId}/cutouts`, { row_id: row.row_id, source });
    } catch {
      if (picture.value !== source) return;
      status.textContent = t("review.backgroundRemovalFailed");
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

  if (match.product) add(`product:${match.product.id}`, t("review.productOption.existing", { name: match.product.name }));
  for (const candidate of match.candidates || []) {
    add(`product:${candidate.id}`, t("review.productOption.existing", { name: candidate.name }));
  }
  if (match.catalog) add("catalog", t("review.productOption.new", { name: match.catalog.display_name }));
  if (!match.catalog || match.catalog.display_name !== row.label) add("label", t("review.productOption.new", { name: row.label }));
  add("custom", t("review.productOption.somethingElse"));

  // The rest of the storage's products, alphabetical (as ListProducts
  // returns them) — so a product the model didn't propose, or proposed with
  // low enough confidence to omit, is still one keystroke away. A plain
  // <select> already supports type-ahead by typing the first letters, which
  // is what makes this "autocomplete" without a custom widget.
  const rest = (products || []).filter((p) => !alreadyListed.has(p.id));
  if (rest.length > 0) {
    const group = document.createElement("optgroup");
    group.label = t("review.productOption.allProductsGroup");
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

// setupCategory wires a row's new-product category field and its "+ New
// category" trigger (docs/specs/27-category-quick-create.md), the
// categories-kind counterpart of setupLocation below.
function setupCategory(el, categories) {
  const select = qs('[data-role="new-product-category"]', el);
  appendCategoryOptions(select, categories);

  const addButton = qs('[data-role="new-product-category-add"]', el);
  addButton.addEventListener("click", () =>
    openCategoryField({
      storageId,
      trigger: addButton,
      openedSelect: select,
      getOpenSelects: () => Array.from(rows.values(), ({ el }) => qs('[data-role="new-product-category"]', el)),
      onError: showMessage,
    }),
  );
}

function setupLocation(el, row, proposal, locations) {
  const select = qs('[data-role="location"]', el);
  const placeholder = document.createElement("option");
  placeholder.value = "";
  placeholder.textContent = t("review.chooseLocation");
  select.append(placeholder);

  const path = row.location?.path || [];
  const proposed = path.filter((segment) => segment.proposed);
  if (proposed.length > 0) {
    const option = document.createElement("option");
    option.value = "new";
    option.textContent = t("review.createLocationPath", { path: path.map((segment) => segment.name).join(" › ") });
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

  const addButton = qs('[data-role="location-add"]', el);
  addButton.addEventListener("click", () =>
    openLocationField({
      storageId,
      trigger: addButton,
      openedSelect: select,
      getOpenSelects: () => Array.from(rows.values(), ({ el }) => qs('[data-role="location"]', el)),
      onError: showMessage,
    }),
  );
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
      if (!name) problems.push(t("review.problems.nameProduct"));
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
    if (!Number.isInteger(quantity) || quantity < 1) problems.push(t("review.problems.quantityMin"));
    item.quantity = quantity;

    const where = qs('[data-role="location"]', el).value;
    if (where === "") {
      problems.push(t("review.problems.chooseLocation"));
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
    showMessage(t("review.someRowsNeedAttention"));
    return;
  }

  setBusy(true);
  try {
    const result = await post(`/api/storages/${storageId}/ingest/${jobId}/confirm`, { items });
    // The capture-time barcode offer, for each product this confirm brought
    // into existence (docs/specs/20-barcode-recall.md). It runs after the
    // write and before the navigation, which is the only moment the item is
    // still in the user's hand; it never fails the confirm, and answering it
    // with "not this time" costs one tap.
    await offerBarcodesFor(result.created_product_ids || []);
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
      showMessage(t("review.alreadyAddedConflict"));
    } else if (err instanceof ApiError && err.status === 404) {
      showMessage(t("review.referencedGone"));
    } else {
      showError(err);
    }
  }
}

// PENDING_BARCODE_PREFIX is where ingest.html parked a code this storage
// could not resolve, keyed by the job the photo created
// (docs/specs/20-barcode-recall.md's miss path: "its value is kept
// client-side through the flow").
const PENDING_BARCODE_PREFIX = "inventory:barcode-for-job:";

// offerBarcodesFor runs the capture-time offer over the products this confirm
// created.
//
// When the photo was taken because a scan came back unknown, and this confirm
// identified exactly one product, that product is the one the code belongs to
// and the offer arrives with the code already in hand. Anything else — several
// new products, or no pending code — is the ordinary offer.
async function offerBarcodesFor(createdIds) {
  const pending = takePendingBarcode();
  if (pending && createdIds.length === 1) {
    await offerScannedBarcode(storageId, { productId: createdIds[0], code: pending });
    return;
  }
  for (const id of createdIds) {
    await offerBarcodeCapture(storageId, { productId: id });
  }
}

// takePendingBarcode reads the parked code and removes it in the same breath:
// it belongs to this one confirm, and a code left behind would be offered
// again for an unrelated product later in the session.
function takePendingBarcode() {
  try {
    const key = PENDING_BARCODE_PREFIX + jobId;
    const code = sessionStorage.getItem(key);
    sessionStorage.removeItem(key);
    return code;
  } catch {
    return null;
  }
}

// onReanalyze is "Analyze again": the same photo, a new proposal. It replaces
// everything on this screen, corrections included, so it asks first — and it
// is the only way the model ever sees this photo twice.
async function onReanalyze() {
  if (!window.confirm(t("review.reanalyzeConfirm"))) {
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
  if (!window.confirm(t("review.discardConfirm"))) {
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
  showMessage(err instanceof ApiError ? apiErrorMessage(err) : t("review.networkError"));
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
