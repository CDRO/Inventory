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
import { get, patch, post, postForm, del, ApiError } from "../api.js";
import { fetchCategories, appendCategoryOptions } from "../category-options.js";
import { fetchLocations, appendLocationOptions, openLocationField } from "../location-options.js";
import { clearChildren, el, text } from "../dom.js";
import { openScanSheet } from "../barcode.js";
import { renderImagePicker } from "../image-picker.js";
import { renderIconPicker } from "../icon-picker.js";
import { productCell } from "../product-table.js";

// The list search is debounced by this many ms — "as-you-type" per
// docs/specs/16-product-maintenance.md, without a request per keystroke.
const SEARCH_DEBOUNCE_MS = 250;

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
const showAllButton = document.querySelector("#show-all");
const addProductButton = document.querySelector("#add-product");
const switcherContainer = document.querySelector("#storage-switcher");

let storageId = null;
/** @type {{id: string, name: string}[]} */
let products = [];
/** @type {{id: string, name: string, depth: number}[]} */
let categories = [];
/** @type {{id: string, name: string, depth: number, path: string[]}[]} */
let locations = [];
let selectedId = null;

// The list panel's search state. null means the filter-only default — no
// products rendered (docs/specs/16-product-maintenance.md) — and a string is
// the last query a request was actually made for, "" meaning "Show all
// products". Kept so a save, a delete or a create elsewhere on the page can
// refresh whatever is currently shown without guessing what that was.
let currentListQuery = null;
let searchGeneration = 0;
let searchDebounceTimer = null;

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

  filterInput.addEventListener("input", onFilterInput);
  showAllButton.addEventListener("click", onShowAllClick);
  addProductButton.addEventListener("click", () => {
    selectedId = null;
    showCreateForm();
  });

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

  // Whatever the list panel was showing (a search, "show all", or nothing)
  // stays showing, refreshed — a save, a merge or a create elsewhere on the
  // page must not silently revert it to the filter-only default.
  if (currentListQuery === null) renderListDefault();
  else await runSearch(currentListQuery);

  if (selectedId && !products.some((p) => p.id === selectedId)) {
    // The selected product is gone — merged away or deleted.
    selectedId = null;
    clearChildren(detailContainer);
  }
  if (selectedId) await showDetail(selectedId);
}

// onFilterInput debounces the as-you-type search
// (docs/specs/16-product-maintenance.md). Clearing the box returns to the
// filter-only empty state immediately — never back to "show all", which is a
// person's explicit choice each time, not a state the page remembers.
function onFilterInput() {
  clearTimeout(searchDebounceTimer);
  const query = filterInput.value.trim();
  if (query === "") {
    renderListDefault();
    return;
  }
  searchDebounceTimer = setTimeout(() => runSearch(query), SEARCH_DEBOUNCE_MS);
}

// onShowAllClick loads and renders every product in the storage — the same
// request the old default page load made, now reached by an explicit click
// rather than happening on its own.
function onShowAllClick() {
  clearTimeout(searchDebounceTimer);
  filterInput.value = "";
  runSearch("");
}

// renderListDefault is the filter-only default: no products rendered until a
// search or "Show all products" (docs/specs/16-product-maintenance.md).
// `searchGeneration` is bumped so a search already in flight cannot land
// after the box was cleared and overwrite this state with stale rows.
function renderListDefault() {
  currentListQuery = null;
  searchGeneration++;
  clearChildren(listContainer);
  const key = products.length === 0 ? "products.list.empty" : "products.list.prompt";
  listContainer.append(el("p", { class: "empty-state" }, [text(t(key))]));
}

// runSearch is the one path to the list table, for both a typed search and
// "Show all products" (query === ""): a filtered GET request built the same
// way every other filtered-and-reloaded listing on this system already is
// (docs/specs/33-inventory-overview-table.md's pattern), replacing the empty
// state with matching rows.
async function runSearch(query) {
  currentListQuery = query;
  const generation = ++searchGeneration;

  let rows;
  try {
    rows = (await get(`${basePath()}?q=${encodeURIComponent(query)}`)).items;
  } catch (err) {
    if (generation !== searchGeneration) return; // superseded by a newer search
    showError(err);
    return;
  }
  if (generation !== searchGeneration) return; // superseded by a newer search
  clearError();

  clearChildren(listContainer);
  if (rows.length === 0) {
    listContainer.append(el("p", { class: "empty-state" }, [text(t("products.list.noMatch"))]));
    return;
  }
  listContainer.append(renderProductListTable(rows));
}

// renderProductListTable is products.html's list table
// (docs/specs/16-product-maintenance.md): the same row shape and CSS classes
// as inventory.html's own table (docs/specs/33-inventory-overview-table.md),
// not a second table implementation — js/product-table.js's productCell is
// the piece the two pages actually share.
function renderProductListTable(rows) {
  return el("table", { class: "inventory-table" }, [
    el("thead", {}, [
      el("tr", {}, [
        el("th", { scope: "col" }, [text(t("products.list.columns.product"))]),
        el("th", { scope: "col" }, [text(t("products.list.columns.category"))]),
        el("th", { scope: "col", class: "inventory-table__qty" }, [text(t("products.list.columns.stock"))]),
      ]),
    ]),
    el("tbody", {}, rows.map(renderProductListRow)),
  ]);
}

function renderProductListRow(row) {
  return el("tr", { "data-product-id": row.id }, [
    el("td", { "data-label": "" }, [
      productCell(row.image_url, row.name, {
        href: `${location.pathname}?storage=${encodeURIComponent(storageId)}&product=${encodeURIComponent(row.id)}`,
        onclick: (event) => {
          event.preventDefault();
          showDetail(row.id);
        },
      }),
    ]),
    el("td", { "data-label": t("products.list.columns.category") }, [text(row.category_name || "—")]),
    el("td", { "data-label": t("products.list.columns.stock"), class: "inventory-table__qty" }, [
      text(String(row.current_stock)),
    ]),
  ]);
}

async function showDetail(productId) {
  selectedId = productId;
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
  const editForm = renderEditForm(product);
  const iconField = renderIconField(product);
  detailContainer.append(
    el("div", { class: "card stack" }, [
      el("h3", {}, [text(product.name)]),
      renderPicture(product, iconField.refresh),
      iconField.node,
      editForm.form,
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

// renderPicture shows the product's picture and the path to changing it —
// what docs/specs/16-product-maintenance.md means by "image (with the change
// paths from 07 — suggestion picker, custom upload)".
//
// The picker is the shared js/image-picker.js, the same one the shopping-list
// reconciliation screen uses, and the write is the PATCH that already existed
// for it: `{"image": "<hash>"}` sets the picked suggestion,
// `{"image": null, "icon_name": null}` clears the picture. Nothing about
// storage scoping is decided here — the route is storage-scoped server-side,
// and an id belonging to another storage gets the same 404 an unknown one
// does (docs/specs/03-auth-and-multi-tenancy.md).
//
// Spec 07's other change path, a custom photo upload, is the file control
// beside the picker. It posts the photo itself to the same address the PATCH
// writes to — `POST …/products/{id}/image` multipart, the address spec 07
// names — and the server reads it through the one upload path that strips a
// photo's metadata after applying its orientation to the pixels. It is
// deliberately built here rather than inside js/image-picker.js: the picker's
// other caller is the shopping-list reconciliation screen, where the product
// being given a picture does not exist yet and there is no id to post to.
// `onIconChanged` is called whenever a picture write clears icon_name — both
// routes always clear it, a picture and a picked icon being alternatives —
// so the icon field's own "current icon" line, a sibling built separately by
// renderIconField, can resync. Without it, that line would go stale the same
// way the old free-text input did before #251: setPicture/uploadPicture's
// re-render is deliberately narrow (it must not discard unsaved edits) and
// has no other handle onto the icon field's DOM.
function renderPicture(product, onIconChanged) {
  const current = el("div", { "data-role": "product-picture" }, [currentPicture(product)]);
  const pickerBox = el("div", { "data-role": "picture-picker", class: "stack", hidden: true });
  const status = el("p", { class: "muted", "data-role": "picture-status", hidden: true });
  const uploadBox = renderPictureUpload(product, current, status, onIconChanged);

  const change = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost",
      "data-role": "change-picture",
      onclick: async () => {
        // Both change paths appear together, and both before the await: spec
        // 07 offers them as alternatives ("pick one of the 3, upload a custom
        // photo instead"), so the upload must not wait on a provider round
        // trip that may never come back.
        pickerBox.hidden = false;
        uploadBox.hidden = false;
        await renderImagePicker(pickerBox, {
          storageId,
          query: product.name,
          keyPrefix: "products.pictures",
          // This product may already have a picture, and removing it must not
          // depend on an image provider being reachable — without this the
          // only way to clear one would be a suggestion list that happened to
          // come back non-empty.
          clearable: true,
          onPick: (hash) => setPicture(product, hash, current, status, onIconChanged),
        });
      },
    },
    [text(t("products.picture.change"))],
  );

  return el("div", { class: "stack" }, [current, change, pickerBox, uploadBox, status]);
}

// renderPictureUpload builds the custom-upload control: a labelled file input
// that posts the chosen photo to `POST …/products/{id}/image`.
//
// The in-flight guard is a local of this call, not a module variable. A
// module-level one would be shared by every product ever opened in this
// session, so returning to a product while another one's upload was still in
// flight would silently refuse it (the bug class of #249/#252 in the picker).
function renderPictureUpload(product, current, status, onIconChanged) {
  let uploading = false;

  const input = el("input", {
    type: "file",
    id: "p-picture",
    // The server keeps JPEG and PNG only (internal/httpapi/upload.go), so
    // saying "image/*" here would let a phone offer a HEIC the upload then
    // refuses.
    accept: "image/jpeg,image/png",
    "data-role": "upload-picture",
    onchange: async (event) => {
      const chosen = event.currentTarget.files?.[0];
      // Cleared before the write, not after. A browser fires no change event
      // when the same file is picked twice, so a control that keeps its value
      // goes quietly dead on the retry after a failure — and the File already
      // in hand stays readable once the input no longer holds it.
      event.currentTarget.value = "";
      if (!chosen || uploading) return;

      uploading = true;
      input.disabled = true;
      try {
        await uploadPicture(product, chosen, current, status, onIconChanged);
      } finally {
        uploading = false;
        input.disabled = false;
      }
    },
  });

  return el("div", { class: "row", "data-role": "picture-upload", hidden: true }, [
    el("label", { for: "p-picture" }, [text(t("products.picture.upload"))]),
    input,
  ]);
}

function currentPicture(product) {
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

// setPicture writes the choice and re-renders only the picture itself. The
// whole detail view is deliberately not re-rendered: the edit form beside it
// may hold changes somebody has typed and not saved, and a picture change
// must not discard them. icon_name is always cleared alongside the picture
// (the two are alternatives) — `onIconChanged` is how the icon field's own
// "current icon" preview, a sibling built separately by renderIconField,
// finds out (docs/specs/40-icon-picker.md; the same staleness #251 fixed for
// the old free-text input, now on the field that replaced it).
async function setPicture(product, hash, current, status, onIconChanged) {
  status.hidden = false;
  status.textContent = t("products.picture.saving");
  try {
    const updated = await patch(`${basePath()}/${product.id}/image`, {
      image: hash,
      icon_name: null,
    });
    product.image_url = updated.image_url ?? null;
    product.icon_name = updated.icon_name ?? null;
    clearChildren(current);
    current.append(currentPicture(product));
    onIconChanged?.();
    status.textContent = hash ? t("products.picture.saved") : t("products.picture.cleared");
  } catch (err) {
    status.textContent = apiErrorMessage(err, t("products.error.network"));
    // Rethrown so the picker rolls its selection back: the message above says
    // the write failed, and a button still marked as chosen would say it did
    // not. A picked suggestion can legitimately fail — the server refuses a
    // hash whose cache entry has been evicted since the list was drawn.
    throw err;
  }
}

// uploadPicture sends a custom photo to the same address setPicture patches,
// under POST with a multipart body — spec 07's upload change path
// (docs/specs/07-shopping-list-reconciliation.md). The server generates the
// filename and strips the photo's metadata, so nothing here has to: what the
// browser called the file never reaches disk.
//
// Unlike setPicture it does not rethrow. There is no selection state to roll
// back — the file input was cleared the moment the file was read — and the
// status line already carries the server's own words.
async function uploadPicture(product, file, current, status, onIconChanged) {
  status.hidden = false;
  status.textContent = t("products.picture.uploading");

  const body = new FormData();
  body.append("image", file);
  try {
    const updated = await postForm(`${basePath()}/${product.id}/image`, body);
    product.image_url = updated.image_url ?? null;
    product.icon_name = updated.icon_name ?? null;
    clearChildren(current);
    current.append(currentPicture(product));
    onIconChanged?.();
    status.textContent = t("products.picture.saved");
  } catch (err) {
    // Deliberately not rethrown, where setPicture just above does rethrow.
    // There is no selection to roll back here: the file input was cleared the
    // moment the file was read, so the control already shows nothing chosen
    // and the status line carries the server's own words. What the retry
    // needs is the guard released, which the caller's `finally` does.
    status.textContent = apiErrorMessage(err, t("products.error.network"));
  }
}

// renderIconField replaces the free-text icon_name input
// (docs/specs/40-icon-picker.md, "The picker UI") with a "Change icon"
// trigger that opens js/icon-picker.js below it — the same open/toggle shape
// renderPicture uses for the picture change paths above.
//
// A pick writes immediately, through the same PATCH the Save button uses
// (`{"icon_name": "…"}` or `{"icon_name": null}` to clear), independently of
// whatever the rest of the edit form currently holds — exactly like a picture
// change already does, and for the same reason: the edit form beside it may
// carry changes nobody has saved yet, and picking an icon must not discard
// them.
//
// Returns `refresh`, alongside `node`, so renderPicture can resync this
// field's "current icon" line when a picture write clears icon_name out from
// under it — the two fields are siblings built independently, and neither
// has any other handle onto the other's DOM.
function renderIconField(product) {
  const current = el("p", { class: "muted", "data-role": "current-icon" }, [text(currentIconLabel(product))]);
  const status = el("p", { class: "muted", "data-role": "icon-status", hidden: true });
  const pickerBox = el("div", { "data-role": "icon-picker", class: "stack", hidden: true });

  const change = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost",
      "data-role": "change-icon",
      onclick: async () => {
        pickerBox.hidden = false;
        await renderIconPicker(pickerBox, {
          storageId,
          currentIconName: product.icon_name,
          keyPrefix: "products.icons",
          onPick: (iconName) => setIcon(product, iconName, current, status),
        });
      },
    },
    [text(t("products.icons.change"))],
  );

  const node = el("div", { class: "field" }, [
    el("label", {}, [text(t("products.edit.icon"))]),
    current,
    change,
    pickerBox,
    status,
  ]);

  return { node, refresh: () => { current.textContent = currentIconLabel(product); } };
}

function currentIconLabel(product) {
  return product.icon_name
    ? t("products.icons.current", { icon: product.icon_name })
    : t("products.icons.current.none");
}

// setIcon writes the picked icon_name and re-renders only the field's own
// "current icon" line — the same narrow-update shape setPicture uses above,
// for the same reason: a full reload() would discard unsaved edits sitting in
// the rest of the form. Rethrows on failure so the picker rolls its selection
// back, exactly like setPicture does.
async function setIcon(product, iconName, current, status) {
  status.hidden = true;
  try {
    const updated = await patch(`${basePath()}/${product.id}`, { icon_name: iconName });
    product.icon_name = updated.icon_name ?? null;
    current.textContent = currentIconLabel(product);
  } catch (err) {
    status.hidden = false;
    status.textContent = apiErrorMessage(err, t("products.error.network"));
    throw err;
  }
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

  const form = el(
    "form",
    {
      class: "stack",
      onsubmit: (event) => {
        event.preventDefault();
        save(product, { name, category, itemType, minStock, shelfLife });
      },
    },
    [
      field(t("products.edit.name"), name),
      field(t("products.edit.category"), category),
      field(t("products.edit.itemTypeLabel"), itemType),
      field(t("products.edit.minStock"), minStock),
      field(t("products.edit.shelfLife"), shelfLife, t("products.edit.shelfLife.hint")),
      el("div", { class: "row" }, [
        el("button", { type: "submit", class: "btn btn--primary" }, [text(t("common.save"))]),
      ]),
    ],
  );
  return { form };
}

function field(label, input, hint) {
  const children = [el("label", { for: input.id }, [text(label)]), input];
  if (hint) children.push(el("small", { class: "muted" }, [text(hint)]));
  return el("div", { class: "field" }, children);
}

// showCreateForm opens the standalone "+ Add product" entry point
// (docs/specs/16-product-maintenance.md, "Creating a product") in the detail
// panel — reachable without first typing a shopping-list line. It reuses the
// same field set and image-picker call shopping-list.js's describeManually()
// already uses (name, category, item type, min_stock, an optional picture)
// rather than building a second form from scratch.
function showCreateForm() {
  clearStatus();
  clearChildren(detailContainer);
  detailContainer.append(renderCreateForm());
}

function renderCreateForm() {
  const name = el("input", { type: "text", id: "np-name", required: true, maxlength: "255" });

  const category = el("select", { id: "np-category" });
  appendCategoryOptions(category, categories);

  const itemType = el("select", { id: "np-item-type" });
  for (const [value, labelKey] of ITEM_TYPES) {
    itemType.append(el("option", { value }, [text(t(labelKey))]));
  }
  // The server's own default when item_type is omitted (internal/store/products.go).
  itemType.value = "long_shelf_life";

  const minStock = el("input", { type: "number", id: "np-min-stock", min: "0", step: "1", value: "0" });

  // The picker needs something to search for, which a blank "+ Add product"
  // form does not have yet — unlike describeManually(), which always opens
  // with a line's own text already in the name field. Rather than firing it
  // automatically off a blur (which would race the very click that leaves the
  // name field to reach Save), it opens the same way the edit form's own
  // picture change does: an explicit button, using whatever name has been
  // typed by the time it is clicked.
  const pictureBox = el("div", { class: "stack", hidden: true });
  let pickedImage = null;
  const addPictureButton = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost",
      onclick: () => {
        pictureBox.hidden = false;
        renderImagePicker(pictureBox, {
          storageId,
          query: name.value.trim(),
          keyPrefix: "products.pictures",
          onPick: (hash) => {
            pickedImage = hash;
          },
        });
      },
    },
    [text(t("products.create.addPicture"))],
  );

  const errorLine = el("div", { class: "alert", role: "alert", hidden: true });

  const form = el(
    "form",
    {
      class: "stack",
      onsubmit: (event) => {
        event.preventDefault();
        submitCreate({ name, category, itemType, minStock }, () => pickedImage, errorLine);
      },
    },
    [
      field(t("products.edit.name"), name),
      field(t("products.edit.category"), category),
      field(t("products.edit.itemTypeLabel"), itemType),
      field(t("products.edit.minStock"), minStock),
      el("div", { class: "field" }, [
        el("label", {}, [text(t("products.create.picture"))]),
        addPictureButton,
        pictureBox,
      ]),
      errorLine,
      el("div", { class: "row" }, [
        el("button", { type: "submit", class: "btn btn--primary" }, [text(t("common.save"))]),
        el(
          "button",
          { type: "button", class: "btn btn--ghost", onclick: () => clearChildren(detailContainer) },
          [text(t("common.cancel"))],
        ),
      ]),
    ],
  );

  return el("div", { class: "card stack" }, [
    el("h3", {}, [text(t("products.create.title"))]),
    form,
  ]);
}

// submitCreate posts the new product. name is the only field the server
// requires (docs/specs/16-product-maintenance.md); a close match to an
// existing product's name comes back as that existing product instead of a
// new one, which is shown exactly like a freshly created product would be —
// the same "merge instead?" courtesy the rename path gives, applied here by
// the server rather than the frontend.
async function submitCreate(inputs, pickedImage, errorLine) {
  const name = inputs.name.value.trim();
  if (!name) {
    errorLine.textContent = t("products.create.nameRequired");
    errorLine.hidden = false;
    return;
  }

  const body = { name, item_type: inputs.itemType.value };
  if (inputs.category.value) body.category_id = inputs.category.value;
  const minStock = Number.parseInt(inputs.minStock.value, 10);
  body.min_stock = Number.isFinite(minStock) ? minStock : 0;
  const hash = pickedImage();
  if (hash) body.image = hash;

  errorLine.hidden = true;
  try {
    const created = await post(basePath(), body);
    clearError();
    selectedId = created.id;
    // After reload(), not before: reload() opens the new product's own detail
    // view, and showDetail() clears the status line at its own start (#395)
    // — set after it runs, so the confirmation is not wiped before anyone
    // sees it.
    await reload();
    showStatus(t("products.create.saved", { name: created.name }));
  } catch (err) {
    errorLine.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("products.error.network");
    errorLine.hidden = false;
  }
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
  // Destroying a container clears it off every batch that referenced it, not
  // just the one in view (docs/specs/39-batch-containers.md), so the
  // confirmation has to be able to say how many. Counted over this product's
  // own batches, which is where a shared container can come from at all — a
  // split with container_disposition "both".
  const containerUsage = new Map();
  for (const batch of product.batches) {
    if (batch.container_id) {
      containerUsage.set(batch.container_id, (containerUsage.get(batch.container_id) ?? 0) + 1);
    }
  }
  const rows = product.batches.map((batch) =>
    renderBatchRow(batch, locationSelects, containerUsage.get(batch.container_id) ?? 0),
  );

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
 * It also carries the batch's container (docs/specs/39-batch-containers.md):
 * what the stock is physically held in, orthogonal to where it sits. The
 * container form upserts through the same PATCH the move uses — one label
 * creates a container, a second renames that same row, null takes the batch out
 * of it without destroying it — and "destroy" is the one separate endpoint,
 * behind a confirmation because it clears the container off every batch that
 * referenced it and cannot be undone by sending the label again.
 *
 * @param {Object} batch
 * @param {HTMLSelectElement[]} locationSelects - every batch row's target-
 *   location field on the currently rendered product, shared so the "+ New
 *   location" trigger can refresh all of them from one GET
 *   (docs/specs/28-batch-move-quick-create.md).
 * @param {number} containerBatchCount - how many of this product's batches
 *   share this batch's container, so the destroy confirmation can say so.
 */
function renderBatchRow(batch, locationSelects, containerBatchCount) {
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
    "data-field": "quantity",
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

  // Shown only when the source batch has a container: with nothing to dispose
  // of, five radio buttons are five questions about nothing
  // (docs/specs/39-batch-containers.md, "Product detail UI"). "source" is
  // pre-selected because it is the server's default and the common case — the
  // 24-pack that now holds 22 is still the 24-pack.
  const dispositionRadios = [];
  const splitDisposition = batch.container_id
    ? el("fieldset", { class: "stack", "data-role": "container-disposition" }, [
        el("legend", {}, [text(t("products.batch.disposition.legend"))]),
        ...["source", "target", "both", "neither", "destroy"].map((value) => {
          const radio = el("input", {
            type: "radio",
            name: `disposition-${batch.id}`,
            value,
            checked: value === "source",
          });
          dispositionRadios.push(radio);
          return el("label", { class: "row" }, [radio, text(t(`products.batch.disposition.${value}`))]);
        }),
      ])
    : null;

  function chosenDisposition() {
    const picked = dispositionRadios.find((radio) => radio.checked);
    return picked ? picked.value : null;
  }

  const splitForm = el(
    "form",
    { class: "stack", hidden: true, "data-role": "split-form" },
    [
      el("div", { class: "row" }, [
        splitQuantity,
        splitTarget,
        splitLocationAdd,
        splitSubmit,
        el(
          "button",
          { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
          [text(t("common.cancel"))],
        ),
      ]),
      ...(splitDisposition ? [splitDisposition] : []),
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
    const body = { quantity, target_location_id: splitTarget.value };
    // Sent only when there was a container to decide about. The server treats
    // the field as a no-op on a container-less batch either way, so this is
    // about not claiming a decision nobody made.
    const disposition = chosenDisposition();
    if (disposition) body.container_disposition = disposition;
    try {
      await post(`${batchesBasePath()}/${batch.id}/split`, body);
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

  // Container: label and type upsert through the batch PATCH
  // (docs/specs/39-batch-containers.md). One label creates a container, a second
  // renames that same row, and null takes the batch out of it without
  // destroying it — three behaviours of one field, so one form drives all three.
  const containerLabel = el("input", {
    type: "text",
    required: true,
    maxlength: "255",
    "data-field": "container-label",
    "aria-label": t("products.batch.containerLabelAriaLabel"),
    placeholder: t("products.batch.containerLabelPlaceholder"),
    value: batch.container_label ?? "",
  });
  const containerType = el("input", {
    type: "text",
    "data-field": "container-type",
    "aria-label": t("products.batch.containerTypeAriaLabel"),
    placeholder: t("products.batch.containerTypePlaceholder"),
    value: batch.container_type ?? "",
  });
  const containerSubmit = el("button", { type: "submit", class: "btn btn--primary" }, [
    text(batch.container_id ? t("products.batch.containerRename") : t("products.batch.containerSave")),
  ]);

  // container_type is only ever sent alongside a label or on a batch that
  // already has a container: the server answers 422 for a type with nothing to
  // attach it to, and this form always has the label beside it, so that refusal
  // is never reached from here by design rather than by a client-side check.
  async function patchContainer(body, trigger) {
    if (trigger.disabled) return;
    errorLine.hidden = true;
    trigger.disabled = true;
    try {
      await patch(`${batchesBasePath()}/${batch.id}`, body);
      await reload();
    } catch (err) {
      fail(err);
    } finally {
      trigger.disabled = false;
    }
  }

  const containerActions = [containerSubmit];
  if (batch.container_id) {
    const clearButton = el("button", { type: "button", class: "btn btn--ghost", "data-role": "container-clear" }, [
      text(t("products.batch.containerClear")),
    ]);
    clearButton.addEventListener("click", () => patchContainer({ container_label: null }, clearButton));

    const destroyButton = el("button", { type: "button", class: "btn btn--danger", "data-role": "container-destroy" }, [
      text(t("products.batch.containerDestroy")),
    ]);
    destroyButton.addEventListener("click", async () => {
      if (destroyButton.disabled) return;
      // Confirmed because it is not undoable by sending the label again, and
      // because it clears the container off every batch that referenced it —
      // which the count names, since a batch list is exactly the surface where
      // more than the row in view can be affected.
      const warning = t("products.batch.containerDestroyConfirm", {
        label: batch.container_label ?? "",
        batches: tCount("products.batch.containerDestroyBatches", Math.max(containerBatchCount, 1)),
      });
      if (!confirm(warning)) return;
      errorLine.hidden = true;
      destroyButton.disabled = true;
      try {
        await post(`/api/storages/${storageId}/containers/${batch.container_id}/destroy`, {});
        await reload();
      } catch (err) {
        fail(err);
      } finally {
        destroyButton.disabled = false;
      }
    });

    containerActions.push(clearButton, destroyButton);
  }

  const containerForm = el(
    "form",
    { class: "row", hidden: true, "data-role": "container-form" },
    [
      containerLabel,
      containerType,
      ...containerActions,
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => closeForms() },
        [text(t("common.cancel"))],
      ),
    ],
  );
  containerForm.addEventListener("submit", (event) => {
    event.preventDefault();
    const label = containerLabel.value.trim();
    if (!label) return;
    const kind = containerType.value.trim();
    patchContainer({ container_label: label, container_type: kind === "" ? null : kind }, containerSubmit);
  });

  function closeForms() {
    splitForm.hidden = true;
    moveForm.hidden = true;
    containerForm.hidden = true;
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
      // The container's label, or nothing at all when the batch is in nothing —
      // docs/specs/39-batch-containers.md asks for exactly that, not an
      // "in no container" line on every row of every product.
      batch.container_label
        ? el("span", { "data-role": "container-label" }, [
            text(t("products.batch.inContainer", { label: batch.container_label })),
          ])
        : text(""),
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
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "data-role": "container-toggle",
          onclick: () => {
            const opening = containerForm.hidden;
            closeForms();
            containerForm.hidden = !opening;
          },
        },
        [text(batch.container_id ? t("products.batch.containerEdit") : t("products.batch.containerAdd"))],
      ),
    ]),
  ]);

  return el("li", { class: "stack", "data-role": "batch-row", "data-batch-id": batch.id }, [
    summary,
    errorLine,
    splitForm,
    moveForm,
    containerForm,
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
