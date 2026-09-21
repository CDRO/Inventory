import "../register-sw.js";

// Page module for stocktake.html — the guided walk of one location
// (docs/specs/13-stocktake-and-audit.md), deep-linked as
// stocktake.html?location=…&storage=….
//
// There is no server-side stocktake session, draft or partial state. Like a
// review job, the sheet is either confirmed whole or abandoned by leaving the
// page, so everything below is local until Confirm.
//
// The one interaction worth knowing about is what happens when the shelf
// changed under you: the confirm states a count for every batch the location
// currently holds, and the server refuses the whole thing with a 422 if that
// set no longer matches. This page then re-reads the sheet rather than
// retrying, because the counts you just typed were counts of a different
// shelf.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { initGamification } from "../gamification.js";
import { fetchProducts } from "../product-options.js";
import { formatAudited } from "../audited.js";
import { get, post, ApiError } from "../api.js";
import { clearChildren, el, text, qs } from "../dom.js";

const switcherContainer = qs("#storage-switcher");
const heading = qs("#heading");
const auditedLine = qs("#audited");
const errorBox = qs("#error");
const noticeBox = qs("#notice");
const statusLine = qs("#status");
const sheetSection = qs("#sheet");
const rowsContainer = qs("#rows");
const emptyNote = qs("#empty");
const foundForm = qs("#add-found");
const foundProduct = qs("#found-product");
const foundQuantity = qs("#found-quantity");
const foundExpiry = qs("#found-expiry");
const foundContainer = qs("#found");
const confirmButton = qs("#confirm");
const backLink = qs("#back");
const cancelLink = qs("#cancel");

let storageId = null;
let locationId = null;
/** @type {Map<string, {batch: Object, input: HTMLInputElement}>} */
const counts = new Map();
/** @type {{product_id: string, product_name: string, quantity: number, expiration_date: string|null}[]} */
let found = [];
/** @type {{id: string, name: string}[]} */
let products = [];

init();

async function init() {
  locationId = new URLSearchParams(location.search).get("location");
  if (!locationId) {
    // Without a location there is no shelf to walk, and guessing one would be
    // worse than saying so.
    statusLine.textContent = "No location given. Pick one from the locations tree.";
    return;
  }

  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    // A 401 never reaches here — api.js redirects those to the login page.
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

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });
  initGamification(storageId);

  // Built by hand rather than through withStorageParam, which starts from the
  // current URL and would carry this page's ?location= over to a page that has
  // no use for it.
  const locationsHref = `/locations.html?storage=${encodeURIComponent(storageId)}`;
  backLink.href = locationsHref;
  cancelLink.href = locationsHref;

  foundForm.addEventListener("submit", addFound);
  confirmButton.addEventListener("click", confirm);

  await Promise.all([loadProducts(), reload()]);
}

async function loadProducts() {
  try {
    products = await fetchProducts(storageId);
  } catch (err) {
    showError(err);
    return;
  }
  clearChildren(foundProduct);
  foundProduct.append(
    el("option", { value: "" }, [text("Pick a product…")]),
    ...products.map((product) => el("option", { value: product.id }, [text(product.name)])),
  );
}

async function reload() {
  clearError();
  try {
    const sheet = await get(`/api/storages/${storageId}/locations/${locationId}/stocktake`);
    render(sheet);
  } catch (err) {
    statusLine.hidden = false;
    statusLine.textContent = "This sheet could not be loaded.";
    sheetSection.hidden = true;
    showError(err);
  }
}

function render(sheet) {
  heading.textContent = `Stocktake · ${sheet.location.name}`;
  document.title = `Stocktake · ${sheet.location.name} · Inventory`;
  auditedLine.textContent = formatAudited(sheet.location.last_audited_at);

  counts.clear();
  clearChildren(rowsContainer);
  for (const batch of sheet.batches) {
    rowsContainer.append(renderRow(batch));
  }

  emptyNote.hidden = sheet.batches.length > 0;
  statusLine.hidden = true;
  sheetSection.hidden = false;
}

// renderRow draws one batch with a quantity stepper. Every value the server
// sent goes in through textContent or an input's value, never as markup.
function renderRow(batch) {
  const input = el("input", {
    type: "number",
    min: "0",
    step: "1",
    value: String(batch.quantity),
    "aria-label": `Counted quantity of ${batch.product_name}`,
  });
  counts.set(batch.id, { batch, input });

  const details = [text(batch.product_name)];
  if (batch.expiration_date) {
    details.push(el("span", { class: "badge" }, [text(`expires ${batch.expiration_date}`)]));
  }

  const thumb = batch.image_url
    ? [el("img", { class: "review-row__thumb", src: batch.image_url, alt: "" })]
    : [];

  return el("div", { class: "review-row" }, [
    ...thumb,
    el("div", { class: "review-row__fields" }, [
      el("div", { class: "row row--between" }, [el("strong", {}, details)]),
      el("p", { class: "muted" }, [text(`Recorded: ${batch.quantity}`)]),
    ]),
    el("div", { class: "review-row__actions" }, [
      // The input is wrapped by its label rather than pointed at by a for=,
      // so no generated id has to stay unique across a re-rendered sheet. The
      // aria-label names which product is being counted, which "Counted"
      // alone does not once a screen reader is on the third row.
      el("label", { class: "field" }, [text("Counted"), input]),
      el("button", {
        type: "button",
        class: "btn btn--ghost",
        onclick: () => {
          input.value = "0";
        },
      }, [text("None left")]),
    ]),
  ]);
}

// addFound stages one found item locally. It is written with everything else
// on Confirm, in the server's single transaction — not posted on its own,
// which would leave half a walk behind if the rest were abandoned.
function addFound(event) {
  event.preventDefault();
  clearError();

  const productId = foundProduct.value;
  const quantity = Number.parseInt(foundQuantity.value, 10);
  if (!productId || !Number.isInteger(quantity) || quantity < 1) {
    showMessage("Pick a product and a quantity of at least 1.");
    return;
  }

  const product = products.find((candidate) => candidate.id === productId);
  found.push({
    product_id: productId,
    product_name: product ? product.name : "",
    quantity,
    // An empty date field is an omission, not a statement: the server resolves
    // the shelf-life rules for it. A typed date is kept as typed.
    expiration_date: foundExpiry.value || undefined,
  });

  foundForm.reset();
  renderFound();
}

function renderFound() {
  clearChildren(foundContainer);
  if (found.length === 0) return;

  foundContainer.append(
    el("strong", {}, [text("Found on the shelf")]),
    ...found.map((item, index) =>
      el("div", { class: "row row--between" }, [
        el("span", {}, [
          text(
            `${item.quantity} × ${item.product_name}` +
              (item.expiration_date ? ` · expires ${item.expiration_date}` : ""),
          ),
        ]),
        el("button", {
          type: "button",
          class: "btn btn--ghost",
          onclick: () => {
            found.splice(index, 1);
            renderFound();
          },
        }, [text("Remove")]),
      ]),
    ),
  );
}

async function confirm() {
  clearError();
  confirmButton.disabled = true;

  const batches = [];
  for (const [batchId, { input }] of counts) {
    const quantity = Number.parseInt(input.value, 10);
    if (!Number.isInteger(quantity) || quantity < 0) {
      confirmButton.disabled = false;
      showMessage("Every count has to be a whole number of zero or more.");
      input.focus();
      return;
    }
    batches.push({ batch_id: batchId, quantity });
  }

  const body = {
    batches,
    found: found.map((item) => {
      const entry = { product_id: item.product_id, quantity: item.quantity };
      // The key is sent only when a date was typed: absent and null mean
      // different things to the server, and sending null for an untouched
      // field would freeze a date nobody chose.
      if (item.expiration_date) entry.expiration_date = item.expiration_date;
      return entry;
    }),
  };

  try {
    const result = await post(`/api/storages/${storageId}/locations/${locationId}/stocktake`, body);
    found = [];
    renderFound();
    showNotice(summarize(result));
    await reload();
  } catch (err) {
    showError(err);
    if (err instanceof ApiError && err.status === 422) {
      // The shelf changed since the sheet was fetched. Re-read it: the counts
      // just typed describe a shelf that no longer exists, and retrying them
      // would be the last-write-wins guess this flow refuses to make.
      await reloadKeepingError();
    }
  } finally {
    confirmButton.disabled = false;
  }
}

function summarize(result) {
  const corrected = result.corrected === 1 ? "1 count corrected" : `${result.corrected} counts corrected`;
  const added = result.created_batch_ids.length;
  const foundPart = added === 0 ? "" : `, ${added === 1 ? "1 item" : `${added} items`} added`;
  return `Stocktake recorded — ${corrected}${foundPart}.`;
}

// reloadKeepingError re-reads the sheet without clearing the message that
// explains why it is being re-read.
async function reloadKeepingError() {
  try {
    render(await get(`/api/storages/${storageId}/locations/${locationId}/stocktake`));
  } catch (err) {
    showError(err);
  }
}

function showMessage(message) {
  noticeBox.hidden = true;
  errorBox.textContent = message;
  errorBox.hidden = false;
}

function showError(err) {
  if (!(err instanceof ApiError)) {
    showMessage("Could not reach the server. Check your connection and try again.");
    return;
  }
  // A 422's per-field text is the specific sentence — "this shelf changed
  // since the sheet was fetched" — while the envelope's own message is the
  // generic one every validation failure shares. Prefer the specific one.
  showMessage(firstFieldMessage(err) || err.message);
}

function firstFieldMessage(err) {
  if (!err.fields) return null;
  for (const messages of Object.values(err.fields)) {
    if (Array.isArray(messages) && messages.length > 0) return messages[0];
  }
  return null;
}

function showNotice(message) {
  errorBox.hidden = true;
  noticeBox.textContent = message;
  noticeBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
  noticeBox.hidden = true;
}
