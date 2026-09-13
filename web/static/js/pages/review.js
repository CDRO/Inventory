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

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { ReviewList } from "../review.js";
import { fetchLocations, appendLocationOptions } from "../location-options.js";
import { get, post, del, ApiError } from "../api.js";
import { pollJob, JobFailedError } from "../jobs.js";
import { qs } from "../dom.js";

const statusLine = qs("#status");
const errorBox = qs("#error");
const proposalSection = qs("#proposal");
const rowsContainer = qs("#rows");
const confirmButton = qs("#confirm");
const discardButton = qs("#discard");

let storageId = null;
let jobId = null;
let list = null;
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
  qs("#back").setAttribute("href", inboxHref());

  jobId = new URLSearchParams(location.search).get("job");
  if (!jobId) {
    setStatus("No proposal was named. Open one from your inbox.");
    return;
  }

  confirmButton.addEventListener("click", onConfirm);
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
      return;
    case "consumed":
      setStatus("This proposal has already been added to your inventory.");
      return;
    case "done":
      await render(job);
      return;
    default:
      setStatus("This proposal cannot be reviewed.");
  }
}

async function render(job) {
  let locations;
  try {
    locations = await fetchLocations(storageId);
  } catch (err) {
    showError(err);
    return;
  }

  const proposal = job.payload;
  list = new ReviewList(rowsContainer, qs("#row-template"));
  list.onCorrect = onCorrect;
  list.setItems(proposal.rows);

  for (const row of proposal.rows) {
    const el = rowsContainer.querySelector(`[data-row-id="${CSS.escape(row.row_id)}"]`);
    rows.set(row.row_id, { row, el });
    setupRow(el, row, proposal, locations, job.has_image);
  }

  if (proposal.rows.length === 0) {
    setStatus("Nothing was found in this photo. Confirm to clear it from your inbox, or discard it.");
  } else {
    statusLine.hidden = true;
  }
  proposalSection.hidden = false;
}

function setupRow(el, row, proposal, locations, hasImage) {
  // The crop: the whole photo as a background, scaled and shifted so only the
  // item's bounding box shows. A product photo has no box and shows whole.
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
    crop.setAttribute("aria-label", `Photo of ${row.label}`);
  }
  // Without a photo the crop stays an empty placeholder, keeping every row's
  // columns aligned.

  qs('[data-role="confidence"]', el).textContent = `${Math.round((row.confidence || 0) * 100)}%`;

  setupProduct(el, row);
  qs('[data-role="quantity"]', el).value = String(row.quantity);
  setupLocation(el, row, proposal, locations);

  const expiry = qs('[data-role="expiry"]', el);
  const noExpiry = qs('[data-role="no-expiry"]', el);
  noExpiry.addEventListener("change", () => {
    expiry.disabled = noExpiry.checked;
    if (noExpiry.checked) expiry.value = "";
  });
}

function setupProduct(el, row) {
  const select = qs('[data-role="product"]', el);
  const match = row.match || {};
  const add = (value, label) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    select.append(option);
  };

  if (match.product) add(`product:${match.product.id}`, `${match.product.name} (in your inventory)`);
  for (const candidate of match.candidates || []) {
    add(`product:${candidate.id}`, `${candidate.name} (in your inventory)`);
  }
  if (match.catalog) add("catalog", `New product: ${match.catalog.display_name}`);
  if (!match.catalog || match.catalog.display_name !== row.label) add("label", `New product: ${row.label}`);
  add("custom", "Something else…");

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
}

// buildItems turns the screen into the confirm body, or marks what is missing.
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

    const choice = qs('[data-role="product"]', el).value;
    if (choice.startsWith("product:")) {
      item.product_id = choice.slice("product:".length);
    } else {
      const name = qs('[data-role="new-product-name"]', el).value.trim();
      if (!name) problems.push("Name the product.");
      item.new_product = { name, item_type: qs('[data-role="item-type"]', el).value };
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
    await post(`/api/storages/${storageId}/ingest/${jobId}/confirm`, { items });
    location.assign(inboxHref({ confirmed: "1" }));
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
