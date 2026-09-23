import "../register-sw.js";

// Page module for products.html — the product list plus the detail/edit view
// docs/specs/16-product-maintenance.md defines, and the screen
// docs/specs/08-expiration-and-classification.md refers to as "a product edit
// screen" without ever specifying.
//
// What it owns: the edit surface (name, category, item type, min_stock, the
// storage-local shelf-life override, the icon), the duplicate merge, the
// delete, and the batch list's split/move picker
// (docs/specs/06-vision-shelf-ingestion.md, "One batch, one location — and
// how to split one"). The target-location field of both actions is built
// through js/location-options.js so docs/specs/28-batch-move-quick-create.md
// only has to attach its "+ New location" trigger. What it deliberately does
// not own: quantity correction and expiry edits, which stay on the stocktake
// screen (docs/specs/13-stocktake-and-audit.md).
//
// The "merge instead?" affordance on a rename is a **courtesy, not a server
// rule** (spec 16 says so in as many words): the server never blocks a rename
// over similarity, because two genuinely different products can share close
// names. It is computed here, over the product list this page already holds,
// rather than through a server route nothing else needs.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
import { get, patch, post, del, ApiError } from "../api.js";
import { fetchCategories, appendCategoryOptions } from "../category-options.js";
import { fetchLocations, appendLocationOptions, openLocationField } from "../location-options.js";
import { clearChildren, el, text } from "../dom.js";
import { openScanSheet } from "../barcode.js";

// The server's bound (maxShelfLifeDays in internal/httpapi/expiry.go). The
// input carries it so a browser flags an out-of-range number before a round
// trip; the server still decides.
const MAX_SHELF_LIFE_DAYS = 36500;

// The three values of docs/specs/02-data-model.md, in the order spec 08 walks
// them when no rule applies.
const ITEM_TYPES = [
  ["perishable", "Perishable"],
  ["long_shelf_life", "Long shelf life"],
  ["non_perishable", "Non-perishable"],
];

const listContainer = document.querySelector("#list");
const detailContainer = document.querySelector("#detail");
const errorBox = document.querySelector("#error");
const statusLine = document.querySelector("#status");
const filterInput = document.querySelector("#filter");
const switcherContainer = document.querySelector("#storage-switcher");

let storageId = null;
/** @type {{id: string, name: string}[]} */
let products = [];
/** @type {{id: string, name: string, depth: number}[]} */
let categories = [];
/** @type {{id: string, name: string, depth: number, path: string[]}[]} */
let locations = [];
let selectedId = null;

init();

async function init() {
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
    // storages.html owns the picker and the empty state, as every other page
    // module does.
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

  filterInput.addEventListener("input", renderList);

  await reload();
}

function basePath() {
  return `/api/storages/${storageId}/products`;
}

function batchesBasePath() {
  return `/api/storages/${storageId}/inventory-batches`;
}

async function reload() {
  try {
    const [productBody, flatCategories] = await Promise.all([
      get(basePath()),
      fetchCategories(storageId),
    ]);
    products = productBody.items;
    categories = flatCategories;
  } catch (err) {
    showError(err);
    return;
  }
  clearError();
  renderList();

  if (selectedId && !products.some((p) => p.id === selectedId)) {
    // The selected product is gone — merged away or deleted.
    selectedId = null;
    clearChildren(detailContainer);
  }
  if (selectedId) await showDetail(selectedId);
}

function renderList() {
  const needle = normalize(filterInput.value);
  const shown = needle ? products.filter((p) => normalize(p.name).includes(needle)) : products;

  clearChildren(listContainer);
  if (products.length === 0) {
    listContainer.append(
      el("p", { class: "empty-state" }, [
        text("No products yet. Scan a shelf or a receipt to add some."),
      ]),
    );
    return;
  }
  if (shown.length === 0) {
    listContainer.append(el("p", { class: "empty-state" }, [text("No product matches that.")]));
    return;
  }

  for (const product of shown) {
    listContainer.append(
      el(
        "button",
        {
          type: "button",
          class: product.id === selectedId ? "btn btn--block btn--primary" : "btn btn--block btn--ghost",
          onclick: () => showDetail(product.id),
        },
        // Names go in through text(), never innerHTML: a product name is user
        // text and may contain anything.
        [text(product.name)],
      ),
    );
  }
}

async function showDetail(productId) {
  selectedId = productId;
  renderList();
  clearStatus();

  let product;
  try {
    // Fetched together so the target-location field always reflects the
    // current tree — including a location another tab or #107's quick-create
    // trigger just added.
    [product, locations] = await Promise.all([get(`${basePath()}/${productId}`), fetchLocations(storageId)]);
  } catch (err) {
    showError(err);
    return;
  }
  clearError();
  renderDetail(product);
}

function renderDetail(product) {
  clearChildren(detailContainer);
  detailContainer.append(
    el("div", { class: "card stack" }, [
      el("h3", {}, [text(product.name)]),
      renderPicture(product),
      renderEditForm(product),
    ]),
    renderStockCard(product),
    renderBarcodeCard(product),
    renderHistoryCard(product),
    renderDangerCard(product),
  );
}

// renderBarcodeCard is the always-available association surface of
// docs/specs/20-barcode-recall.md: add by scanning, add by typing, and remove.
//
// It is unaffected by the capture-time offer's on/off preference. That
// preference governs being *offered* a scan at capture; this is a deliberate
// action somebody came to the product screen to perform, and turning the offer
// off must never take a feature away.
function renderBarcodeCard(product) {
  const list = el("ul", { "data-role": "barcode-list" }, []);
  const errorLine = el("div", { class: "alert", role: "alert", hidden: true });
  const typed = el("input", {
    type: "text",
    id: "p-barcode",
    inputmode: "numeric",
    autocomplete: "off",
    placeholder: "4006381333931",
  });

  function fail(err) {
    errorLine.textContent =
      err instanceof ApiError && err.code === "conflict"
        ? "That barcode already belongs to another product here."
        : err instanceof ApiError
          ? err.message
          : "Could not reach the server. Try again.";
    errorLine.hidden = false;
  }

  async function reload() {
    clearChildren(list);
    let items = [];
    try {
      const body = await get(`${basePath()}/${product.id}/barcodes`);
      items = Array.isArray(body?.items) ? body.items : [];
    } catch (err) {
      fail(err);
      return;
    }
    if (items.length === 0) {
      list.append(el("li", { class: "empty-state" }, [text("No barcode yet.")]));
      return;
    }
    for (const item of items) {
      list.append(
        el("li", { class: "row row--between" }, [
          text(item.barcode),
          el(
            "button",
            {
              type: "button",
              class: "btn btn--ghost",
              onclick: () => remove(item.barcode),
            },
            [text("Remove")],
          ),
        ]),
      );
    }
  }

  async function add(code) {
    errorLine.hidden = true;
    try {
      await post(`${basePath()}/${product.id}/barcodes`, { barcode: code });
      typed.value = "";
      await reload();
    } catch (err) {
      fail(err);
    }
  }

  async function remove(code) {
    errorLine.hidden = true;
    try {
      await del(`${basePath()}/${product.id}/barcodes/${encodeURIComponent(code)}`);
      await reload();
    } catch (err) {
      fail(err);
    }
  }

  const form = el("form", { class: "row" }, [
    typed,
    el("button", { type: "submit", class: "btn" }, [text("Add")]),
    el(
      "button",
      {
        type: "button",
        class: "btn",
        onclick: async () => {
          const code = await openScanSheet(storageId, { title: "Scan a barcode" });
          if (code) await add(code);
        },
      },
      [text("Scan")],
    ),
  ]);
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const code = typed.value.trim();
    if (code) add(code);
  });

  reload();

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text("Barcodes")]),
    el("p", { class: "muted" }, [
      text("Scanning one of these finds this product instantly, with no photo and no AI call."),
    ]),
    errorLine,
    list,
    el("label", { for: "p-barcode" }, [text("Add a barcode")]),
    form,
  ]);
}

function renderPicture(product) {
  if (product.image_url) {
    return el("img", {
      src: product.image_url,
      alt: `Picture of ${product.name}`,
      style: "max-width: 8rem; border-radius: var(--radius, 6px);",
    });
  }
  if (product.icon_name) {
    return el("p", { class: "muted" }, [text(`Icon: ${product.icon_name}`)]);
  }
  return el("p", { class: "empty-state" }, [text("No picture yet.")]);
}

function renderEditForm(product) {
  const name = el("input", { type: "text", id: "p-name", value: product.name, required: true, maxlength: "255" });

  // The same picker the review screens build (js/category-options.js), so the
  // indentation and the "no category" option are one implementation.
  const category = el("select", { id: "p-category" });
  appendCategoryOptions(category, categories);
  category.value = product.category_id ?? "";

  const itemType = el("select", { id: "p-item-type" });
  for (const [value, label] of ITEM_TYPES) {
    const option = el("option", { value }, [text(label)]);
    if (value === product.item_type) option.selected = true;
    itemType.append(option);
  }

  const minStock = el("input", {
    type: "number", id: "p-min-stock", min: "0", step: "1", value: String(product.min_stock),
  });

  const shelfLife = el("input", {
    type: "number",
    id: "p-shelf-life",
    min: "0",
    max: String(MAX_SHELF_LIFE_DAYS),
    step: "1",
    placeholder: "Inherit",
    value: product.default_shelf_life_days == null ? "" : String(product.default_shelf_life_days),
  });

  const icon = el("input", {
    type: "text", id: "p-icon", maxlength: "100", placeholder: "noto:cheese-wedge",
    value: product.icon_name ?? "",
  });

  const form = el(
    "form",
    {
      class: "stack",
      onsubmit: (event) => {
        event.preventDefault();
        save(product, { name, category, itemType, minStock, shelfLife, icon });
      },
    },
    [
      field("Name", name),
      field("Category", category),
      field("Item type", itemType),
      field("Minimum stock", minStock),
      field("Shelf life (days)", shelfLife, "Leave empty to inherit from the category, the catalog, or the item type."),
      field("Icon name", icon, "Leave empty for no icon."),
      el("div", { class: "row" }, [
        el("button", { type: "submit", class: "btn btn--primary" }, [text("Save")]),
      ]),
    ],
  );
  return form;
}

function field(label, input, hint) {
  const children = [el("label", { for: input.id }, [text(label)]), input];
  if (hint) children.push(el("small", { class: "muted" }, [text(hint)]));
  return el("div", { class: "field" }, children);
}

async function save(product, inputs) {
  const body = {};
  const trimmedName = inputs.name.value.trim();
  if (trimmedName !== product.name) body.name = trimmedName;

  const categoryId = inputs.category.value || null;
  if (categoryId !== (product.category_id ?? null)) body.category_id = categoryId;

  if (inputs.itemType.value !== product.item_type) body.item_type = inputs.itemType.value;

  const minStock = Number.parseInt(inputs.minStock.value, 10);
  if (Number.isFinite(minStock) && minStock !== product.min_stock) body.min_stock = minStock;

  // An empty field is an explicit null — "resolve it down the chain" — not an
  // absent one, which is the difference the server's PATCH is built around.
  const shelfLifeRaw = inputs.shelfLife.value.trim();
  const shelfLife = shelfLifeRaw === "" ? null : Number.parseInt(shelfLifeRaw, 10);
  if (shelfLife !== (product.default_shelf_life_days ?? null)) body.default_shelf_life_days = shelfLife;

  const iconRaw = inputs.icon.value.trim();
  const icon = iconRaw === "" ? null : iconRaw;
  if (icon !== (product.icon_name ?? null)) body.icon_name = icon;

  if (Object.keys(body).length === 0) {
    showStatus("Nothing changed.");
    return;
  }

  // The rename courtesy: a close existing name is offered as a merge, and the
  // save goes ahead either way if the user says no. The server never refuses a
  // rename over similarity (docs/specs/16-product-maintenance.md).
  if (body.name) {
    const twin = closestOtherProduct(body.name, product.id);
    if (twin && !confirm(`“${twin.name}” already exists. Save this rename anyway?\n\nCancel to merge them instead.`)) {
      await offerMerge(product, twin);
      return;
    }
  }

  try {
    const updated = await patch(`${basePath()}/${product.id}`, body);
    clearError();
    if (typeof updated.recomputed_batches === "number") {
      showStatus(
        updated.recomputed_batches === 1
          ? "Shelf life saved; 1 expiry date was recalculated."
          : `Shelf life saved; ${updated.recomputed_batches} expiry dates were recalculated.`,
      );
    } else {
      showStatus("Saved.");
    }
    await reload();
  } catch (err) {
    showError(err);
  }
}

/**
 * closestOtherProduct is the courtesy check behind a rename: an existing
 * product whose normalized name contains the new one, or is contained by it.
 * That is what catches the case the spec names — renaming "Penne" to "Penne
 * Barilla 500g" while "Barilla Penne" already exists.
 *
 * Containment rather than a similarity score, deliberately: this is an offer,
 * not a rule, the dialog is cancellable, and a rule nobody can predict is
 * worse than one that occasionally stays quiet. The server never blocks a
 * rename over similarity either way
 * (docs/specs/16-product-maintenance.md).
 */
function closestOtherProduct(name, ownId) {
  const needle = normalize(name);
  if (!needle) return null;
  return (
    products.find((p) => {
      if (p.id === ownId) return false;
      const other = normalize(p.name);
      return other.includes(needle) || needle.includes(other);
    }) ?? null
  );
}

async function offerMerge(survivor, source) {
  if (!confirm(`Merge “${source.name}” into “${survivor.name}”?\n\nAll of its stock and history moves across, and “${source.name}” disappears.`)) {
    return;
  }
  try {
    const result = await post(`${basePath()}/${survivor.id}/merge`, { source_product_id: source.id });
    clearError();
    showStatus(
      `Merged. ${countLabel(result.moved_batches, "batch", "batches")} moved, ` +
        `${countLabel(result.recomputed_batches, "expiry date", "expiry dates")} recalculated.`,
    );
    selectedId = survivor.id;
    await reload();
  } catch (err) {
    showError(err);
  }
}

function renderStockCard(product) {
  // Shared across every batch row's split/move target field, so a single
  // "+ New location" trigger refreshes all of them from one GET
  // (docs/specs/28-batch-move-quick-create.md, matching 26's refresh
  // contract) rather than one request per open field.
  const locationSelects = [];
  const rows = product.batches.map((batch) => renderBatchRow(batch, locationSelects));

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(`In stock: ${product.current_stock}`)]),
    rows.length
      ? el("ul", { class: "stack" }, rows)
      : el("p", { class: "empty-state" }, [text("Nothing on the shelf.")]),
    el("p", { class: "muted" }, [
      text("Quantity corrections and expiry edits happen on the "),
      el("a", { href: withStorageParam(storageId, "/stocktake.html") }, [text("stocktake screen")]),
      text("."),
    ]),
  ]);
}

/**
 * renderBatchRow is one batch's line plus its split/move picker
 * (docs/specs/06-vision-shelf-ingestion.md, "One batch, one location — and
 * how to split one"). Both forms call the endpoints directly — no
 * client-side check of whether the split quantity or the target location is
 * valid, because the server is the only thing holding a lock on the row and
 * therefore the only thing that actually knows (docs/specs/28 supersedes the
 * "linking to 06/08/13" reading of docs/specs/16).
 *
 * @param {Object} batch
 * @param {HTMLSelectElement[]} locationSelects - every batch row's target-
 *   location field on the currently rendered product, shared so the "+ New
 *   location" trigger can refresh all of them from one GET
 *   (docs/specs/28-batch-move-quick-create.md).
 */
function renderBatchRow(batch, locationSelects) {
  const locationName = locationNameFor(batch.location_id);
  const errorLine = el("div", { class: "alert", role: "alert", hidden: true });

  function showFieldError(message) {
    errorLine.textContent = message;
    errorLine.hidden = false;
  }

  function fail(err) {
    showFieldError(err instanceof ApiError ? err.message : "Could not reach the server. Try again.");
  }

  function locationOptionsWithPlaceholder(select) {
    select.append(el("option", { value: "" }, [text("Choose a location…")]));
    appendLocationOptions(select, locations);
  }

  // Split: a new batch at the target, carrying the same quantity, off the
  // current one. The server rejects a quantity at or above the source's
  // current quantity — that request is splitting the whole batch, which is
  // the move action below, not this one.
  const splitQuantity = el("input", {
    type: "number",
    min: "1",
    step: "1",
    "aria-label": "Quantity to split off",
    placeholder: "Quantity",
  });
  const splitTarget = el("select", { "aria-label": "Split target location", "data-field": "location" });
  locationOptionsWithPlaceholder(splitTarget);
  locationSelects.push(splitTarget);
  const splitLocationAdd = el(
    "button",
    { type: "button", class: "btn btn--ghost", "data-role": "location-add" },
    [text("+ New location")],
  );
  splitLocationAdd.addEventListener("click", () =>
    openLocationField({
      storageId,
      trigger: splitLocationAdd,
      openedSelect: splitTarget,
      getOpenSelects: () => locationSelects,
      onError: showFieldError,
    }),
  );
  const splitForm = el(
    "form",
    { class: "row", hidden: true, "data-role": "split-form" },
    [
      splitQuantity,
      splitTarget,
      splitLocationAdd,
      el("button", { type: "submit", class: "btn btn--primary" }, [text("Split")]),
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
        [text("Cancel")],
      ),
    ],
  );
  splitForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const quantity = Number.parseInt(splitQuantity.value, 10);
    if (!Number.isFinite(quantity) || !splitTarget.value) return;
    errorLine.hidden = true;
    try {
      await post(`${batchesBasePath()}/${batch.id}/split`, {
        quantity,
        target_location_id: splitTarget.value,
      });
      await reload();
    } catch (err) {
      fail(err);
    }
  });

  // Move: the whole batch, same id, new location_id — a different endpoint
  // from split, not a split of the full quantity.
  const moveTarget = el("select", { "aria-label": "Move target location", "data-field": "location" });
  locationOptionsWithPlaceholder(moveTarget);
  locationSelects.push(moveTarget);
  const moveLocationAdd = el(
    "button",
    { type: "button", class: "btn btn--ghost", "data-role": "location-add" },
    [text("+ New location")],
  );
  moveLocationAdd.addEventListener("click", () =>
    openLocationField({
      storageId,
      trigger: moveLocationAdd,
      openedSelect: moveTarget,
      getOpenSelects: () => locationSelects,
      onError: showFieldError,
    }),
  );
  const moveForm = el(
    "form",
    { class: "row", hidden: true, "data-role": "move-form" },
    [
      moveTarget,
      moveLocationAdd,
      el("button", { type: "submit", class: "btn btn--primary" }, [text("Move")]),
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
        [text("Cancel")],
      ),
    ],
  );
  moveForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!moveTarget.value) return;
    errorLine.hidden = true;
    try {
      await patch(`${batchesBasePath()}/${batch.id}`, { location_id: moveTarget.value });
      await reload();
    } catch (err) {
      fail(err);
    }
  });

  function closeForms() {
    splitForm.hidden = true;
    moveForm.hidden = true;
    errorLine.hidden = true;
  }

  const summary = el("div", { class: "row row--between" }, [
    el("span", {}, [
      text(`${batch.quantity} × ${locationName}`),
      text(batch.expiration_date ? ` — expires ${batch.expiration_date}` : " — no expiry"),
      text(batch.expiration_source === "user" ? " (you set this)" : ""),
    ]),
    el("div", { class: "row" }, [
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "data-role": "split-toggle",
          onclick: () => {
            const opening = splitForm.hidden;
            closeForms();
            splitForm.hidden = !opening;
          },
        },
        [text("Split")],
      ),
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "data-role": "move-toggle",
          onclick: () => {
            const opening = moveForm.hidden;
            closeForms();
            moveForm.hidden = !opening;
          },
        },
        [text("Move")],
      ),
    ]),
  ]);

  return el("li", { class: "stack", "data-role": "batch-row", "data-batch-id": batch.id }, [
    summary,
    errorLine,
    splitForm,
    moveForm,
  ]);
}

/**
 * locationNameFor looks up a batch's current location by id against the
 * flat tree this page already fetched. Falls back to "Unknown location"
 * rather than throwing — a batch can briefly point at a location deleted by
 * another member between this page's two loads.
 */
function locationNameFor(locationId) {
  const match = locations.find((loc) => loc.id === locationId);
  return match ? match.name : "Unknown location";
}

function renderHistoryCard(product) {
  const rows = product.logs.map((entry) =>
    el("li", {}, [
      text(
        `${entry.timestamp.slice(0, 10)} · ${entry.change_qty > 0 ? "+" : ""}${entry.change_qty} · ${entry.reason}` +
          (entry.created_by ? ` · ${entry.created_by}` : ""),
      ),
    ]),
  );

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text("Recent history")]),
    rows.length
      ? el("ul", {}, rows)
      : el("p", { class: "empty-state" }, [text("Nothing recorded yet.")]),
  ]);
}

function renderDangerCard(product) {
  const others = products.filter((p) => p.id !== product.id);
  const picker = el("select", { id: "p-merge-source" }, [
    el("option", { value: "" }, [text("— Pick the duplicate —")]),
  ]);
  for (const other of others) {
    picker.append(el("option", { value: other.id }, [text(other.name)]));
  }

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text("Clean-up")]),
    el("p", { class: "muted" }, [
      text(`Merging keeps “${product.name}” exactly as it is and folds the other product's stock and history into it.`),
    ]),
    el("div", { class: "row" }, [
      picker,
      el(
        "button",
        {
          type: "button",
          class: "btn",
          onclick: () => {
            const source = others.find((p) => p.id === picker.value);
            if (!source) {
              showStatus("Pick the duplicate to merge in first.");
              return;
            }
            offerMerge(product, source);
          },
        },
        [text("Merge in")],
      ),
    ]),
    el(
      "button",
      {
        type: "button",
        class: "btn btn--danger",
        onclick: () => removeProduct(product),
      },
      [text("Delete this product")],
    ),
  ]);
}

async function removeProduct(product) {
  // The confirm states the stock and that the history goes with it, which
  // docs/specs/16-product-maintenance.md requires of the frontend: deleting is
  // allowed even with stock on hand, so the person has to be told what they
  // are erasing.
  const warning =
    `Delete “${product.name}”?\n\n` +
    `${countLabel(product.current_stock, "item", "items")} currently on the shelf and ` +
    `its entire history will be erased. This cannot be undone.`;
  if (!confirm(warning)) return;

  try {
    await del(`${basePath()}/${product.id}`);
    clearError();
    showStatus(`“${product.name}” deleted.`);
    selectedId = null;
    clearChildren(detailContainer);
    await reload();
  } catch (err) {
    showError(err);
  }
}

function countLabel(n, singular, plural) {
  return `${n} ${n === 1 ? singular : plural}`;
}

/** normalize lowercases and collapses whitespace, for the filter and the
 * rename courtesy. It is not the server's matching service — this is a local
 * convenience over a list the page already has. */
function normalize(value) {
  return value.trim().toLowerCase().replace(/\s+/g, " ");
}

function showStatus(message) {
  statusLine.textContent = message;
  statusLine.hidden = false;
}

function clearStatus() {
  statusLine.textContent = "";
  statusLine.hidden = true;
}

function showError(err) {
  errorBox.textContent =
    err instanceof ApiError ? err.message : "Something went wrong. Check your connection and try again.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
