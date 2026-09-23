import "../register-sw.js";

// Page module for shopping-list.html — the resolution UI of
// docs/specs/07-shopping-list-reconciliation.md.
//
// Each line lands in one of three states and gets a different set of choices:
// a confident local match, a catalog description that can be accepted in one
// click, or nothing known yet. The states come from the server; this module
// never re-derives them, because the classification is made once at ingestion
// and must not shift under someone who is halfway through reviewing a list.
//
// Confirming a line is what writes inventory (spec 07's last acceptance
// criterion): the quantity at the chosen location becomes a batch, and a line
// that names a new product creates it. Nothing is written before Confirm.
//
// Two things this page deliberately does not do:
//
//   - It never contacts Iconify, SerpAPI or Google. Image suggestions and card
//     pictures arrive as URLs on this origin, already fetched server-side. An
//     <img> pointing at a provider would leak the viewer's IP and user-agent
//     to that host and would not load at all on a LAN-only NAS.
//   - It never sends a catalog id back, because it is never given one. "Add
//     this" says only that the card was accepted; the server knows which row
//     that was.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
import { fetchLocations, appendLocationOptions, openLocationField } from "../location-options.js";
import { fetchCategories, appendCategoryOptions, openCategoryField } from "../category-options.js";
import { get, post, ApiError } from "../api.js";
import { el, text, clearChildren, qs } from "../dom.js";
import { offerBarcodeCapture } from "../barcode-offer.js";

const switcherContainer = qs("#storage-switcher");
const errorBox = qs("#error");
const composeSection = qs("#compose");
const rawTextInput = qs("#raw-text");
const submitButton = qs("#submit");
const resultsSection = qs("#results");
const itemsContainer = qs("#items");
const itemTemplate = qs("#item-template");

let storageId = null;
let listId = null;
let locations = [];
let categories = [];

init();

async function init() {
  // A list matched before the storage's locations and categories have loaded
  // would render every line with an empty "Put it in" picker, so matching
  // waits until they have.
  submitButton.disabled = true;

  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    showError(err);
    return;
  }

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    // storages.html owns the picker and the empty state; duplicating either
    // here would mean two places to keep honest.
    location.assign("/storages.html");
    return;
  }

  storageId = resolved;
  rememberStorageId(storageId);
  if (new URLSearchParams(location.search).get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });
  renderInboxLink(document.querySelector("#inbox-link"), storageId);
  initGamification(storageId);
  submitButton.addEventListener("click", submitList);

  // Where things go, and how a new product is filed. Loaded once: a line is
  // resolved against the storage as it was when the list was opened. If that
  // fails the button stays disabled: the error says what went wrong, and a
  // reload is the way to retry.
  try {
    [locations, categories] = await Promise.all([fetchLocations(storageId), fetchCategories(storageId)]);
  } catch (err) {
    showError(err);
    return;
  }
  submitButton.disabled = false;
}

function basePath() {
  return `/api/storages/${storageId}/shopping-lists`;
}

async function submitList() {
  const rawText = rawTextInput.value;
  if (!rawText.trim()) {
    showError(new Error("Add at least one line."));
    return;
  }

  submitButton.disabled = true;
  clearError();
  try {
    const list = await post(basePath(), { source: "text", raw_text: rawText });
    listId = list.id;
    renderList(list);
  } catch (err) {
    showError(err);
  } finally {
    submitButton.disabled = false;
  }
}

function renderList(list) {
  composeSection.hidden = true;
  resultsSection.hidden = false;
  clearChildren(itemsContainer);

  for (const item of list.items) {
    itemsContainer.append(renderItem(item));
  }
}

// A line's decision lives on its node as `choice`, one of:
//   { kind: "product", productId }        an existing product
//   { kind: "catalog", variant }          "Add this" on the card (or a variant)
//   { kind: "manual" }                    a product described in the form
// Every state below sets it, and Confirm reads only it — so what is sent is
// always what is on screen.
function renderItem(item) {
  const node = itemTemplate.content.firstElementChild.cloneNode(true);

  field(node, "raw_text").textContent = item.raw_text;
  field(node, "status").textContent = statusLabel(item.status);

  const quantityInput = field(node, "quantity");
  quantityInput.value = String(item.resolved_quantity ?? item.quantity ?? 1);

  if (item.status === "resolved") {
    renderResolved(node, item);
    return node;
  }

  const locationSelect = field(node, "location");
  const placeholder = el("option", { value: "" }, [text("Choose a location…")]);
  locationSelect.append(placeholder);
  appendLocationOptions(locationSelect, locations);
  if (locations.length === 1) locationSelect.value = locations[0].id;

  const addLocationButton = field(node, "location-add");
  addLocationButton.addEventListener("click", () =>
    openLocationField({
      storageId,
      trigger: addLocationButton,
      openedSelect: locationSelect,
      getOpenSelects: () =>
        Array.from(itemsContainer.querySelectorAll('[data-field="location"]')).filter((s) => !s.disabled),
      onError: (message) => showError(new Error(message)),
    }),
  );

  const categorySelect = field(node, "category");
  appendCategoryOptions(categorySelect, categories);

  const addCategoryButton = field(node, "category-add");
  addCategoryButton.addEventListener("click", () =>
    openCategoryField({
      storageId,
      trigger: addCategoryButton,
      openedSelect: categorySelect,
      getOpenSelects: () =>
        Array.from(itemsContainer.querySelectorAll('[data-field="category"]')).filter((s) => !s.disabled),
      onError: (message) => showError(new Error(message)),
    }),
  );

  renderDetail(node, item);

  node.querySelector('[data-action="resolve"]').addEventListener("click", () => resolveItem(node, item));
  node.querySelector('[data-action="dismiss"]').addEventListener("click", () => dismissItem(node, item));
  return node;
}

function renderResolved(node, item) {
  const detail = field(node, "detail");
  clearChildren(detail);
  detail.append(
    el("p", { class: "empty-state" }, [
      text(item.matched_product || item.resolved_quantity > 0 ? "Confirmed." : "Not bought."),
    ]),
  );
  for (const control of node.querySelectorAll("input, select, button")) {
    control.disabled = true;
  }
  field(node, "new-product").hidden = true;
  clearChildren(field(node, "escape"));
}

// renderDetail draws the per-state choices of spec 07's "Resolution UI per
// state".
function renderDetail(node, item) {
  const detail = field(node, "detail");
  const escape = field(node, "escape");
  clearChildren(detail);
  clearChildren(escape);

  if (item.status === "exact_match" && item.matched_product) {
    node.choice = { kind: "product", productId: item.matched_product.id };
    detail.append(el("p", {}, [text(`Matches ${item.matched_product.name}.`)]));
    return;
  }

  if (item.status === "ambiguous") {
    node.choice = null;
    detail.append(el("p", {}, [text("Which one did you mean?")]));
    const buttons = item.candidates.map((candidate) =>
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "aria-pressed": "false",
          onclick: (event) => {
            node.choice = { kind: "product", productId: candidate.id };
            markChosen(detail, event.currentTarget);
          },
        },
        [text(candidate.name)],
      ),
    );
    detail.append(el("div", { class: "stack" }, buttons));
    escape.append(
      escapeButton("Treat as new item", () => describeManually(node, item, lineName(item))),
      escapeButton("Enter manually", () => describeManually(node, item, "")),
    );
    return;
  }

  // new_item, with or without a catalog description.
  if (item.catalog) {
    node.choice = { kind: "catalog", variant: null };
    detail.append(renderCatalogCard(node, item.catalog));
    escape.append(escapeButton("It's something else", () => describeManually(node, item, "")));
    return;
  }

  detail.append(
    el("p", { class: "empty-state" }, [text("Nothing known about this yet. Describe it to add it.")]),
  );
  describeManually(node, item, lineName(item));
}

// describeManually opens the product form. Declining a catalog card this way
// and naming the product differently is what links it to that card as a
// variant — the server records that, from the name typed here.
function describeManually(node, item, name) {
  node.choice = { kind: "manual" };
  node.picture = null;

  const form = field(node, "new-product");
  form.hidden = false;
  field(node, "name").value = name;

  for (const button of field(node, "detail").querySelectorAll("button")) {
    button.classList.remove("btn--selected");
    button.setAttribute("aria-pressed", "false");
    button.disabled = true;
  }
  clearChildren(field(node, "escape"));

  renderPictureChoices(node, item);
  field(node, "name").focus();
}

// renderCatalogCard shows what the anonymous catalog knows. Everything here is
// a display field — there is no id to render even if this code wanted one.
// The card itself is the default choice; a variant replaces it when clicked.
function renderCatalogCard(node, catalog) {
  const lines = [];
  if (catalog.image_url) {
    lines.push(el("img", { src: catalog.image_url, alt: catalog.display_name, width: 96, height: 96, loading: "lazy" }));
  }
  lines.push(el("strong", {}, [text(catalog.display_name)]));

  if (catalog.category_path) {
    lines.push(el("p", { class: "empty-state" }, [text(catalog.category_path)]));
  }
  if (catalog.item_type) {
    lines.push(el("p", { class: "empty-state" }, [text(catalog.item_type)]));
  }
  if (catalog.default_shelf_life_days != null) {
    lines.push(
      el("p", { class: "empty-state" }, [
        text(`Keeps about ${catalog.default_shelf_life_days} days.`),
      ]),
    );
  }

  const choices = el("div", { class: "row" });
  const pick = (variant, button) => {
    node.choice = { kind: "catalog", variant };
    markChosen(choices, button);
  };
  const addThis = el(
    "button",
    { type: "button", class: "btn btn--ghost btn--selected", "aria-pressed": "true", onclick: (e) => pick(null, e.currentTarget) },
    [text(`Add this: ${catalog.display_name}`)],
  );
  choices.append(addThis);
  for (const name of catalog.variants || []) {
    choices.append(
      el(
        "button",
        { type: "button", class: "btn btn--ghost", "aria-pressed": "false", onclick: (e) => pick(name, e.currentTarget) },
        [text(name)],
      ),
    );
  }
  lines.push(choices);

  return el("div", { class: "card stack" }, lines);
}

// renderPictureChoices asks the server for pictures and lets the reviewer pick
// one. Every URL rendered is on this origin; a picked picture is sent back by
// its cache hash, and the server copies it into permanent storage.
async function renderPictureChoices(node, item) {
  const container = field(node, "pictures");
  clearChildren(container);
  node.picture = null;

  const status = el("span", { class: "empty-state" }, [text("Looking for pictures…")]);
  container.append(status);

  const query = field(node, "name").value.trim() || lineName(item);
  let suggestions = [];
  try {
    const body = await get(`/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`);
    suggestions = body.suggestions || [];
  } catch {
    // A provider being unreachable must not block the line: the spec's own
    // degradation rule, carried through to the UI.
    status.textContent = "Picture search is unavailable right now. You can still continue without one.";
    return;
  }

  if (suggestions.length === 0) {
    status.textContent = "No pictures found. You can continue without one.";
    return;
  }

  status.textContent = "Pick a picture, or continue without one.";
  const row = el("div", { class: "row" });
  const none = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost btn--selected",
      "aria-pressed": "true",
      onclick: (event) => {
        node.picture = null;
        markChosen(row, event.currentTarget);
      },
    },
    [text("No picture")],
  );
  row.append(none);

  for (const suggestion of suggestions) {
    const hash = suggestion.url.split("/").pop();
    row.append(
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "aria-pressed": "false",
          "aria-label": `Use this ${suggestion.type}`,
          onclick: (event) => {
            node.picture = hash;
            markChosen(row, event.currentTarget);
          },
        },
        [el("img", { src: suggestion.url, alt: "", width: 72, height: 72, loading: "lazy" })],
      ),
    );
  }
  container.append(row);
}

function escapeButton(label, onclick) {
  return el("button", { type: "button", class: "btn btn--ghost", onclick }, [text(label)]);
}

function markChosen(container, button) {
  for (const other of container.querySelectorAll("button")) {
    other.classList.remove("btn--selected");
    other.setAttribute("aria-pressed", "false");
  }
  button.classList.add("btn--selected");
  button.setAttribute("aria-pressed", "true");
}

// buildResolve turns the line's choice and fields into the resolve body, or
// names what is still missing.
function buildResolve(node) {
  const choice = node.choice;
  if (!choice) return { problem: "Pick which one you meant, or describe it as a new item." };

  const quantity = Number.parseInt(field(node, "quantity").value, 10);
  if (!Number.isInteger(quantity) || quantity < 0) return { problem: "Enter how many you bought." };

  const body = { quantity };
  const locationId = field(node, "location").value;
  if (quantity > 0) {
    if (!locationId) return { problem: "Choose where it goes." };
    body.location_id = locationId;
  }

  switch (choice.kind) {
    case "product":
      body.product_id = choice.productId;
      break;
    case "catalog":
      body.new_product = { from: "catalog" };
      if (choice.variant) body.new_product.variant = choice.variant;
      break;
    case "manual": {
      const name = field(node, "name").value.trim();
      if (!name) return { problem: "Name the product." };
      body.new_product = {
        from: "manual",
        name,
        item_type: field(node, "item_type").value,
        min_stock: Math.max(0, Number.parseInt(field(node, "min_stock").value, 10) || 0),
      };
      const categoryId = field(node, "category").value;
      if (categoryId) body.new_product.category_id = categoryId;
      if (node.picture) body.new_product.image = node.picture;
      break;
    }
  }
  return { body };
}

async function resolveItem(node, item) {
  clearError();
  const { body, problem } = buildResolve(node);
  if (problem) {
    showError(new Error(`${item.raw_text}: ${problem}`));
    return;
  }
  await send(node, item, body);
}

async function dismissItem(node, item) {
  clearError();
  await send(node, item, { quantity: 0 });
}

async function send(node, item, body) {
  const buttons = node.querySelectorAll('[data-action="resolve"], [data-action="dismiss"]');
  for (const button of buttons) button.disabled = true;
  try {
    const updated = await post(`${basePath()}/${listId}/items/${item.id}/resolve`, body);
    node.replaceWith(renderItem({ ...item, ...updated, status: "resolved" }));
    // A New Item that created a product is one of the three capture points
    // docs/specs/20-barcode-recall.md's offer attaches to. A line that only
    // matched an existing product created nothing, so nothing is offered.
    if (updated.product_created && updated.matched_product?.id) {
      await offerBarcodeCapture(storageId, {
        productId: updated.matched_product.id,
        productName: updated.matched_product.name || "",
      });
    }
  } catch (err) {
    for (const button of buttons) button.disabled = false;
    showError(err);
  }
}

// lineName is the line without its multiplier: "eggs x2" names eggs.
function lineName(item) {
  return item.raw_text
    .replace(/^\s*\d+\s*[xX*]?\s+/, "")
    .replace(/\s+[xX*]\s*\d+\s*$/, "")
    .replace(/\s+\d+\s*[xX]\s*$/, "")
    .trim();
}

function statusLabel(status) {
  switch (status) {
    case "exact_match":
      return "Match";
    case "ambiguous":
      return "Which one?";
    case "new_item":
      return "New";
    case "resolved":
      return "Done";
    default:
      return status;
  }
}

function field(node, name) {
  return node.querySelector(`[data-field="${name}"]`);
}

function showError(err) {
  errorBox.textContent =
    err instanceof ApiError ? err.message : err.message || "Something went wrong.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
