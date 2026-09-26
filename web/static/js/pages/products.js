import "../register-sw.js";
import { t, tCount, formatDate, apiErrorMessage } from "../i18n.js";

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
// not own: quantity correction, which stays on the stocktake sheet
// (docs/specs/13-stocktake-and-audit.md).
//
// The "merge instead?" affordance on a rename is a **courtesy, not a server
// rule** (spec 16 says so in as many words): the server never blocks a rename
// over similarity, because two genuinely different products can share close
// names. It is computed here, over the product list this page already holds,
// rather than through a server route nothing else needs.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderNav, startPageFor } from "../nav.js";
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
  ["perishable", "products.itemType.perishable"],
  ["long_shelf_life", "products.itemType.longShelfLife"],
  ["non_perishable", "products.itemType.nonPerishable"],
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
  renderNav(document.querySelector("#nav"), {
    storageId,
    current: "products",
    startPage: startPageFor(me.storages, storageId),
  });
  initGamification(storageId);

  filterInput.addEventListener("input", renderList);

  // A deep link from inventory.html's "Product" column
  // (docs/specs/33-inventory-overview-table.md) names the product to open
  // immediately, the same way stocktake.html?location= does for a location.
  const linkedProduct = new URLSearchParams(location.search).get("product");
  if (linkedProduct) selectedId = linkedProduct;

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
        text(t("products.list.empty")),
      ]),
    );
    return;
  }
  if (shown.length === 0) {
    listContainer.append(el("p", { class: "empty-state" }, [text(t("products.list.noMatch"))]));
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
        ? t("products.barcode.conflict")
        : err instanceof ApiError
          ? apiErrorMessage(err)
          : t("products.error.network");
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
      list.append(el("li", { class: "empty-state" }, [text(t("products.barcode.empty"))]));
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
            [text(t("products.barcode.remove"))],
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
    el("button", { type: "submit", class: "btn" }, [text(t("products.barcode.add"))]),
    el(
      "button",
      {
        type: "button",
        class: "btn",
        onclick: async () => {
          const code = await openScanSheet(storageId, { title: t("products.barcode.scanTitle") });
          if (code) await add(code);
        },
      },
      [text(t("products.barcode.scan"))],
    ),
  ]);
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    const code = typed.value.trim();
    if (code) add(code);
  });

  reload();

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(t("products.barcode.title"))]),
    el("p", { class: "muted" }, [
      text(t("products.barcode.hint")),
    ]),
    errorLine,
    list,
    el("label", { for: "p-barcode" }, [text(t("products.barcode.addLabel"))]),
    form,
  ]);
}

function renderPicture(product) {
  if (product.image_url) {
    return el("img", {
      src: product.image_url,
      alt: t("products.picture.alt", { name: product.name }),
      style: "max-width: 8rem; border-radius: var(--radius, 6px);",
    });
  }
  if (product.icon_name) {
    return el("p", { class: "muted" }, [text(t("products.picture.icon", { icon: product.icon_name }))]);
  }
  return el("p", { class: "empty-state" }, [text(t("products.picture.none"))]);
}

function renderEditForm(product) {
  const name = el("input", { type: "text", id: "p-name", value: product.name, required: true, maxlength: "255" });

  // The same picker the review screens build (js/category-options.js), so the
  // indentation and the "no category" option are one implementation.
  const category = el("select", { id: "p-category" });
  appendCategoryOptions(category, categories);
  category.value = product.category_id ?? "";

  const itemType = el("select", { id: "p-item-type" });
  for (const [value, labelKey] of ITEM_TYPES) {
    const option = el("option", { value }, [text(t(labelKey))]);
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
    placeholder: t("products.edit.shelfLife.placeholder"),
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
      field(t("products.edit.name"), name),
      field(t("products.edit.category"), category),
      field(t("products.edit.itemTypeLabel"), itemType),
      field(t("products.edit.minStock"), minStock),
      field(t("products.edit.shelfLife"), shelfLife, t("products.edit.shelfLife.hint")),
      field(t("products.edit.icon"), icon, t("products.edit.icon.hint")),
      el("div", { class: "row" }, [
        el("button", { type: "submit", class: "btn btn--primary" }, [text(t("common.save"))]),
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
    showStatus(t("products.save.nothingChanged"));
    return;
  }

  // The rename courtesy: a close existing name is offered as a merge, and the
  // save goes ahead either way if the user says no. The server never refuses a
  // rename over similarity (docs/specs/16-product-maintenance.md).
  if (body.name) {
    const twin = closestOtherProduct(body.name, product.id);
    if (twin && !confirm(t("products.save.confirmRenameOverMerge", { name: twin.name }))) {
      await offerMerge(product, twin);
      return;
    }
  }

  try {
    const updated = await patch(`${basePath()}/${product.id}`, body);
    clearError();
    if (typeof updated.recomputed_batches === "number") {
      showStatus(tCount("products.save.shelfLifeRecalculated", updated.recomputed_batches));
    } else {
      showStatus(t("products.save.saved"));
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
  if (!confirm(t("products.merge.confirm", { source: source.name, survivor: survivor.name }))) {
    return;
  }
  try {
    const result = await post(`${basePath()}/${survivor.id}/merge`, { source_product_id: source.id });
    clearError();
    showStatus(
      t("products.merge.result", {
        batches: tCount("products.merge.movedBatches", result.moved_batches),
        dates: tCount("products.merge.recomputedDates", result.recomputed_batches),
      }),
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

  const [hintBefore, hintAfter] = t("products.stock.hint").split("{link}");
  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(t("products.stock.title", { count: product.current_stock }))]),
    rows.length
      ? el("ul", { class: "stack" }, rows)
      : el("p", { class: "empty-state" }, [text(t("products.stock.empty"))]),
    el("p", { class: "muted" }, [
      text(hintBefore),
      el("a", { href: withStorageParam(storageId, "/stocktake.html") }, [text(t("products.stock.linkText"))]),
      text(hintAfter),
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
 * That row lock is not the same guarantee as "one click, one request",
 * though (#161): it serializes two concurrent splits against each other, but
 * each still re-validates against whatever quantity it finds once it gets
 * the lock, so two rapid clicks can each legitimately succeed in turn. The
 * split and move submit handlers below guard against that with their own
 * disabled/`finally` pattern (matching `openLocationField`'s `trigger`
 * guard in js/location-options.js) — a defense against a duplicate
 * *request*, not a validity check the server already owns.
 *
 * @param {Object} batch
 * @param {HTMLSelectElement[]} locationSelects - every batch row's target-
 *   location field on the currently rendered product, shared so the "+ New
 *   location" trigger can refresh all of them from one GET
 *   (docs/specs/28-batch-move-quick-create.md).
 */
function renderBatchRow(batch, locationSelects) {
  const locationPath = locationPathFor(batch.location_id);
  const errorLine = el("div", { class: "alert", role: "alert", hidden: true });

  function showFieldError(message) {
    errorLine.textContent = message;
    errorLine.hidden = false;
  }

  function fail(err) {
    showFieldError(err instanceof ApiError ? apiErrorMessage(err) : t("products.error.network"));
  }

  function locationOptionsWithPlaceholder(select) {
    select.append(el("option", { value: "" }, [text(t("products.batch.chooseLocation"))]));
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
    required: true,
    "aria-label": t("products.batch.splitQuantityAriaLabel"),
    placeholder: t("products.batch.quantityPlaceholder"),
  });
  const splitTarget = el("select", {
    "aria-label": t("products.batch.splitTargetAriaLabel"),
    "data-field": "location",
    required: true,
  });
  locationOptionsWithPlaceholder(splitTarget);
  locationSelects.push(splitTarget);
  const splitLocationAdd = el(
    "button",
    { type: "button", class: "btn btn--ghost", "data-role": "location-add" },
    [text(t("products.batch.newLocation"))],
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
  const splitSubmit = el("button", { type: "submit", class: "btn btn--primary" }, [text(t("products.batch.split"))]);
  const splitForm = el(
    "form",
    { class: "row", hidden: true, "data-role": "split-form" },
    [
      splitQuantity,
      splitTarget,
      splitLocationAdd,
      splitSubmit,
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
        [text(t("common.cancel"))],
      ),
    ],
  );
  splitForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    // Guards a second click racing the in-flight request, same pattern as
    // openLocationField's trigger.disabled in js/location-options.js.
    if (splitSubmit.disabled) return;
    const quantity = Number.parseInt(splitQuantity.value, 10);
    if (!Number.isFinite(quantity) || !splitTarget.value) return;
    errorLine.hidden = true;
    splitSubmit.disabled = true;
    try {
      await post(`${batchesBasePath()}/${batch.id}/split`, {
        quantity,
        target_location_id: splitTarget.value,
      });
      await reload();
    } catch (err) {
      fail(err);
    } finally {
      splitSubmit.disabled = false;
    }
  });

  // Move: the whole batch, same id, new location_id — a different endpoint
  // from split, not a split of the full quantity.
  const moveTarget = el("select", {
    "aria-label": t("products.batch.moveTargetAriaLabel"),
    "data-field": "location",
    required: true,
  });
  locationOptionsWithPlaceholder(moveTarget);
  locationSelects.push(moveTarget);
  const moveLocationAdd = el(
    "button",
    { type: "button", class: "btn btn--ghost", "data-role": "location-add" },
    [text(t("products.batch.newLocation"))],
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
  const moveSubmit = el("button", { type: "submit", class: "btn btn--primary" }, [text(t("products.batch.move"))]);
  const moveForm = el(
    "form",
    { class: "row", hidden: true, "data-role": "move-form" },
    [
      moveTarget,
      moveLocationAdd,
      moveSubmit,
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
        [text(t("common.cancel"))],
      ),
    ],
  );
  moveForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    // Guards a second click racing the in-flight request, same pattern as
    // openLocationField's trigger.disabled in js/location-options.js.
    if (moveSubmit.disabled) return;
    if (!moveTarget.value) return;
    errorLine.hidden = true;
    moveSubmit.disabled = true;
    try {
      await patch(`${batchesBasePath()}/${batch.id}`, { location_id: moveTarget.value });
      await reload();
    } catch (err) {
      fail(err);
    } finally {
      moveSubmit.disabled = false;
    }
  });

  function closeForms() {
    splitForm.hidden = true;
    moveForm.hidden = true;
    errorLine.hidden = true;
  }

  const summary = el("div", { class: "row row--between" }, [
    el("span", {}, [
      text(t("products.batch.summary", { quantity: batch.quantity, location: locationPath })),
      text(
        batch.expiration_date
          ? // expiration_date is a bare DATE (migrations/00002_core_schema.sql),
            // parsed as UTC midnight — timeZone: "UTC" renders the calendar
            // date the server sent, not one day early for a viewer west of UTC.
            t("products.batch.expiresOn", {
              date: formatDate(new Date(batch.expiration_date), { dateStyle: "medium", timeZone: "UTC" }),
            })
          : t("products.batch.noExpiry"),
      ),
      text(batch.expiration_source === "user" ? t("products.batch.userSet") : ""),
    ]),
    el("div", { class: "row" }, [
      el("a", { class: "btn btn--ghost", href: stocktakeHref(batch.location_id) }, [
        text(t("products.batch.countThisShelf")),
      ]),
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
        [text(t("products.batch.split"))],
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
        [text(t("products.batch.move"))],
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
 * locationPathFor looks up a batch's current location by id against the flat
 * tree this page already fetched, root first ("Cellar › Shelf A") — the same
 * shape js/pages/consume-review.js and js/pages/inventory.js render
 * (docs/specs/35-stocktake-entry-points.md). Falls back to "Unknown location"
 * rather than throwing — a batch can briefly point at a location deleted by
 * another member between this page's two loads.
 */
function locationPathFor(locationId) {
  const match = locations.find((loc) => loc.id === locationId);
  return match ? match.path.join(" › ") : t("products.batch.locationUnknown");
}

// stocktakeHref links a batch's location to its stocktake sheet, the same
// plain-template shape js/pages/inventory.js's own stocktakeHref gives — not
// withStorageParam, which would carry this page's own ?product= into a page
// that has no use for it.
function stocktakeHref(locationId) {
  return `/stocktake.html?location=${encodeURIComponent(locationId)}&storage=${encodeURIComponent(storageId)}`;
}

function renderHistoryCard(product) {
  const rows = product.logs.map((entry) => {
    // entry.reason and entry.created_by are server-supplied text
    // (docs/specs/19-localization.md: the API stays English) — only the
    // structure and the date around them is localized.
    let line = t("products.history.entry", {
      date: formatDate(new Date(entry.timestamp), { dateStyle: "medium" }),
      qty: `${entry.change_qty > 0 ? "+" : ""}${entry.change_qty}`,
      reason: entry.reason,
    });
    if (entry.created_by) {
      line += t("products.history.entryCreatedBy", { createdBy: entry.created_by });
    }
    return el("li", {}, [text(line)]);
  });

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(t("products.history.title"))]),
    rows.length
      ? el("ul", {}, rows)
      : el("p", { class: "empty-state" }, [text(t("products.history.empty"))]),
  ]);
}

function renderDangerCard(product) {
  const others = products.filter((p) => p.id !== product.id);
  const picker = el("select", { id: "p-merge-source" }, [
    el("option", { value: "" }, [text(t("products.danger.pickDuplicate"))]),
  ]);
  for (const other of others) {
    picker.append(el("option", { value: other.id }, [text(other.name)]));
  }

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(t("products.danger.title"))]),
    el("p", { class: "muted" }, [
      text(t("products.danger.mergeHint", { name: product.name })),
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
              showStatus(t("products.danger.pickFirst"));
              return;
            }
            offerMerge(product, source);
          },
        },
        [text(t("products.danger.mergeIn"))],
      ),
    ]),
    el(
      "button",
      {
        type: "button",
        class: "btn btn--danger",
        onclick: () => removeProduct(product),
      },
      [text(t("products.danger.delete"))],
    ),
  ]);
}

async function removeProduct(product) {
  // The confirm states the stock and that the history goes with it, which
  // docs/specs/16-product-maintenance.md requires of the frontend: deleting is
  // allowed even with stock on hand, so the person has to be told what they
  // are erasing.
  const warning = t("products.delete.confirm", {
    name: product.name,
    items: tCount("products.delete.items", product.current_stock),
  });
  if (!confirm(warning)) return;

  try {
    await del(`${basePath()}/${product.id}`);
    clearError();
    showStatus(t("products.delete.done", { name: product.name }));
    selectedId = null;
    clearChildren(detailContainer);
    await reload();
  } catch (err) {
    showError(err);
  }
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
  errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("products.error.network.full");
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
