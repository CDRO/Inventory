import "../register-sw.js";

// Page module for inventory.html: the whole-inventory table
// (docs/specs/33-inventory-overview-table.md), the read counterpart of
// products.html's per-product view. "What do we have, and where?" is
// answered by loading every batch of the storage — the whole inventory
// really does mean the whole inventory, per the spec — and then filtering,
// sorting and grouping it entirely on the client.
//
// There is no inline editing here. A quantity is corrected through the
// stocktake sheet (13), and a product through products.html (16); this page
// only reads and links out to those screens.

import { fetchMe, resolveStorage, rememberStorageId } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderNav, startPageFor } from "../nav.js";
import { initGamification } from "../gamification.js";
import { get } from "../api.js";
import { el, text, clearChildren, qs } from "../dom.js";
import { t, tCount, apiErrorMessage, formatDate } from "../i18n.js";

const NBSP = String.fromCharCode(0xa0);
const PAGE_LIMIT = 200;
const URGENCY_KEYS = ["expired", "critical", "soon", "ok", "none"];

const errorBox = qs("#error");
const loadingLine = qs("#loading");
const incompleteBanner = qs("#incomplete");
const contentSection = qs("#content");
const filterText = qs("#filter-text");
const filterLocation = qs("#filter-location");
const filterCategory = qs("#filter-category");
const sortSelect = qs("#sort-select");
const groupToggle = qs("#group-toggle");
const summaryLine = qs("#summary");
const emptyStorage = qs("#empty-storage");
const emptyFiltered = qs("#empty-filtered");
const clearFiltersButton = qs("#clear-filters");
const table = qs("#table");
const tbody = qs("#rows");
const switcherContainer = qs("#storage-switcher");

let storageId = null;
/** @type {Array<Object>} raw rows as the API returns them. */
let allRows = [];
/** @type {Array} the location tree, nested, as GET /locations returns it. */
let locationTree = [];
let categoryTree = [];

const state = {
  q: "",
  locationId: "",
  categoryId: "",
  sortKey: "expiry",
  sortDir: "asc",
  group: false,
  urgency: new Set(URGENCY_KEYS),
};

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

  readStateFromURL();
  syncURL();

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });
  renderNav(qs("#nav"), {
    storageId,
    current: "inventory",
    startPage: startPageFor(me.storages, storageId),
  });
  initGamification(storageId);

  wireControls();

  let trees;
  try {
    trees = await Promise.all([
      get(`/api/storages/${storageId}/locations`),
      get(`/api/storages/${storageId}/categories`),
    ]);
  } catch (err) {
    showError(err);
    return;
  }
  locationTree = trees[0].items;
  categoryTree = trees[1].items;
  populateSelect(filterLocation, flattenTree(locationTree), t("inventory.filters.allLocations"));
  populateSelect(filterCategory, flattenTree(categoryTree), t("inventory.filters.allCategories"));
  filterLocation.value = state.locationId;
  filterCategory.value = state.categoryId;

  const { rows, incomplete, error } = await loadAllBatches();
  allRows = rows;
  loadingLine.hidden = true;

  if (incomplete && allRows.length === 0) {
    // Nothing at all could be confirmed loaded — this is a failure, not a
    // partial table. Showing "this table is incomplete" here would describe
    // a fetch that never produced a first row as though it had gone some of
    // the way, and showing the empty-storage state would claim a fact the
    // page does not actually know.
    if (error) showError(error);
    return;
  }

  if (incomplete) {
    incompleteBanner.hidden = false;
    if (error) showError(error);
  }

  contentSection.hidden = false;
  render();
}

// loadAllBatches follows next_cursor with limit=200 until the collection is
// exhausted, per docs/specs/33-inventory-overview-table.md. Sorting and
// filtering happen once, entirely on the client, against the full set.
async function loadAllBatches() {
  const rows = [];
  let cursor = null;
  updateLoadingText(0);
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE_LIMIT) });
    if (cursor) params.set("cursor", cursor);
    let page;
    try {
      page = await get(`/api/storages/${storageId}/inventory-batches?${params}`);
    } catch (err) {
      return { rows, incomplete: true, error: err };
    }
    rows.push(...page.items);
    updateLoadingText(rows.length);
    cursor = page.next_cursor;
    if (!cursor) break;
  }
  return { rows, incomplete: false, error: null };
}

function updateLoadingText(count) {
  loadingLine.textContent = t("inventory.loading", { count });
}

function wireControls() {
  filterText.value = state.q;
  sortSelect.value = sortToken();
  groupToggle.checked = state.group;
  for (const key of URGENCY_KEYS) {
    qs(`#urgency-${key}`).checked = state.urgency.has(key);
  }

  filterText.addEventListener("input", () => {
    state.q = filterText.value;
    syncURL();
    render();
  });
  filterLocation.addEventListener("change", () => {
    state.locationId = filterLocation.value;
    syncURL();
    render();
  });
  filterCategory.addEventListener("change", () => {
    state.categoryId = filterCategory.value;
    syncURL();
    render();
  });
  sortSelect.addEventListener("change", () => {
    applySortToken(sortSelect.value);
    syncURL();
    render();
  });
  groupToggle.addEventListener("change", () => {
    state.group = groupToggle.checked;
    syncURL();
    render();
  });
  for (const key of URGENCY_KEYS) {
    qs(`#urgency-${key}`).addEventListener("change", (event) => {
      if (event.target.checked) state.urgency.add(key);
      else state.urgency.delete(key);
      syncURL();
      render();
    });
  }
  clearFiltersButton.addEventListener("click", () => {
    state.q = "";
    state.locationId = "";
    state.categoryId = "";
    state.urgency = new Set(URGENCY_KEYS);
    filterText.value = "";
    filterLocation.value = "";
    filterCategory.value = "";
    for (const key of URGENCY_KEYS) qs(`#urgency-${key}`).checked = true;
    syncURL();
    render();
  });
  for (const button of table.querySelectorAll(".sort-button")) {
    button.addEventListener("click", () => {
      const key = button.dataset.sort;
      if (state.sortKey === key) {
        state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
      } else {
        state.sortKey = key;
        state.sortDir = "asc";
      }
      sortSelect.value = sortToken();
      syncURL();
      render();
    });
  }
}

function applySortToken(token) {
  if (token.startsWith("-")) {
    state.sortKey = token.slice(1);
    state.sortDir = "desc";
  } else {
    state.sortKey = token;
    state.sortDir = "asc";
  }
}

// sortToken is applySortToken's inverse — the single place that decides how
// the current sort is spelled, in the <select> and in the query string.
function sortToken() {
  return (state.sortDir === "desc" ? "-" : "") + state.sortKey;
}

// readStateFromURL restores filter and sort state from the query string, so
// a filtered view survives a reload and can be shared as a link.
function readStateFromURL() {
  const params = new URLSearchParams(location.search);
  state.q = params.get("q") || "";
  state.locationId = params.get("loc") || "";
  state.categoryId = params.get("cat") || "";
  state.group = params.get("group") === "1";
  applySortToken(params.get("sort") || "expiry");

  const urgencyParam = params.get("urgency");
  if (urgencyParam) {
    state.urgency = new Set(urgencyParam.split(",").filter((k) => URGENCY_KEYS.includes(k)));
  }
}

function syncURL() {
  const params = new URLSearchParams();
  params.set("storage", storageId);
  if (state.q) params.set("q", state.q);
  if (state.locationId) params.set("loc", state.locationId);
  if (state.categoryId) params.set("cat", state.categoryId);
  if (state.group) params.set("group", "1");
  if (!(state.sortKey === "expiry" && state.sortDir === "asc")) {
    params.set("sort", sortToken());
  }
  if (state.urgency.size !== URGENCY_KEYS.length) {
    params.set("urgency", URGENCY_KEYS.filter((k) => state.urgency.has(k)).join(","));
  }
  history.replaceState(null, "", `${location.pathname}?${params}`);
}

// --- Filtering, grouping, sorting -----------------------------------------

function classifyUrgency(dateStr) {
  if (!dateStr) return "none";
  const start = todayUTC();
  // A bare DATE ("2027-01-10") parses as UTC midnight per ECMA-262 — the same
  // reasoning js/pages/stocktake.js gives for rendering one.
  const day = Date.parse(dateStr);
  const diffDays = Math.round((day - start) / 86400000);
  if (diffDays < 0) return "expired";
  if (diffDays < 3) return "critical";
  if (diffDays < 14) return "soon";
  return "ok";
}

function todayUTC() {
  const now = new Date();
  return Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
}

function applyFilters(rows) {
  const q = state.q.trim().toLowerCase();
  const locationSet = descendantSetFor(locationTree, state.locationId);
  const categorySet = descendantSetFor(categoryTree, state.categoryId);

  return rows.filter((row) => {
    if (q && !row.product_name.toLowerCase().includes(q)) return false;
    if (locationSet && !locationSet.has(row.location_id)) return false;
    if (categorySet && (!row.category_id || !categorySet.has(row.category_id))) return false;
    if (!state.urgency.has(classifyUrgency(row.expiration_date))) return false;
    return true;
  });
}

// descendantSetFor returns the set of ids matching id-or-descendant within
// tree, or null when id is empty (meaning "no filter" — every row matches).
function descendantSetFor(tree, id) {
  if (!id) return null;
  const node = findNode(tree, id);
  if (!node) return new Set();
  return collectIds(node, new Set());
}

function findNode(nodes, id) {
  for (const node of nodes) {
    if (node.id === id) return node;
    const found = findNode(node.children || [], id);
    if (found) return found;
  }
  return null;
}

function collectIds(node, out) {
  out.add(node.id);
  for (const child of node.children || []) collectIds(child, out);
  return out;
}

function groupByProduct(rows) {
  const groups = new Map();
  for (const row of rows) {
    let group = groups.get(row.product_id);
    if (!group) {
      group = {
        kind: "group",
        productId: row.product_id,
        productName: row.product_name,
        imageUrl: row.image_url,
        categoryName: row.category_name,
        quantity: 0,
        locationIds: new Set(),
        earliestExpiry: null,
        expirationSource: null,
        batches: [],
      };
      groups.set(row.product_id, group);
    }
    group.quantity += row.quantity;
    group.locationIds.add(row.location_id);
    group.batches.push(row);
    if (row.expiration_date && (group.earliestExpiry == null || row.expiration_date < group.earliestExpiry)) {
      group.earliestExpiry = row.expiration_date;
      group.expirationSource = row.expiration_source;
    }
  }
  return [...groups.values()];
}

function sortValue(item) {
  const grouped = item.kind === "group";
  switch (state.sortKey) {
    case "product":
      return (grouped ? item.productName : item.product_name).toLowerCase();
    case "location":
      return grouped ? item.locationIds.size : item.location_path.join(" › ");
    case "quantity":
      return item.quantity;
    case "expiry":
    default:
      return grouped ? item.earliestExpiry : item.expiration_date;
  }
}

function sortRows(items) {
  return [...items].sort((a, b) => {
    const av = sortValue(a);
    const bv = sortValue(b);

    if (state.sortKey === "expiry") {
      // Nulls sort last regardless of direction: "no expiry" is not a date
      // that becomes "soonest" when the direction flips.
      if (av == null && bv == null) return 0;
      if (av == null) return 1;
      if (bv == null) return -1;
      const cmp = av < bv ? -1 : av > bv ? 1 : 0;
      return state.sortDir === "desc" ? -cmp : cmp;
    }

    const cmp = typeof av === "number" && typeof bv === "number" ? av - bv : String(av).localeCompare(String(bv));
    return state.sortDir === "desc" ? -cmp : cmp;
  });
}

// --- Rendering ---------------------------------------------------------

function render() {
  updateSortIndicators();

  const filtered = applyFilters(allRows);
  renderSummary(filtered);

  if (allRows.length === 0) {
    emptyStorage.hidden = false;
    emptyFiltered.hidden = true;
    table.hidden = true;
    return;
  }
  emptyStorage.hidden = true;

  if (filtered.length === 0) {
    emptyFiltered.hidden = false;
    table.hidden = true;
    return;
  }
  emptyFiltered.hidden = true;

  const items = state.group ? groupByProduct(filtered) : filtered.map((row) => ({ kind: "batch", ...row }));
  const sorted = sortRows(items);

  clearChildren(tbody);
  for (const item of sorted) {
    tbody.append(item.kind === "group" ? renderGroupRow(item) : renderBatchRow(item));
  }
  table.hidden = false;
}

function updateSortIndicators() {
  for (const button of table.querySelectorAll(".sort-button")) {
    const th = button.closest("th");
    const active = button.dataset.sort === state.sortKey;
    th.setAttribute("aria-sort", active ? (state.sortDir === "desc" ? "descending" : "ascending") : "none");
  }
}

function renderSummary(filtered) {
  const items = filtered.reduce((sum, row) => sum + row.quantity, 0);
  summaryLine.textContent = t("inventory.summary", {
    items: tCount("inventory.summary.items", items),
    batches: tCount("inventory.summary.batches", filtered.length),
  });
}

function renderBatchRow(row) {
  return el("tr", { "data-batch-id": row.id }, [
    el("td", { "data-label": "" }, [productCell(row.image_url, row.product_name, row.product_id)]),
    el("td", { "data-label": t("inventory.columns.category") }, [text(row.category_name || "—")]),
    el("td", { "data-label": t("inventory.columns.location") }, [text(row.location_path.join(" › "))]),
    el("td", { "data-label": t("inventory.columns.quantity"), class: "inventory-table__qty" }, [text(String(row.quantity))]),
    el("td", { "data-label": t("inventory.columns.expires") }, [expiryCell(row.expiration_date, row.expiration_source)]),
    el("td", { "data-label": "" }, [
      el("a", { class: "btn btn--ghost", href: stocktakeHref(row.location_id) }, [
        text(t("inventory.countThisShelf")),
      ]),
    ]),
  ]);
}

function renderGroupRow(group) {
  const expanded = { open: false };
  const toggle = el("button", {
    type: "button",
    class: "inventory-table__group-toggle",
    "aria-expanded": "false",
    onclick: () => {
      expanded.open = !expanded.open;
      toggle.setAttribute("aria-expanded", String(expanded.open));
      detailRow.hidden = !expanded.open;
    },
  }, [text(expanded.open ? "▾" : "▸")]);

  const row = el("tr", {}, [
    el("td", { "data-label": "" }, [toggle, productCell(group.imageUrl, group.productName, group.productId)]),
    el("td", { "data-label": t("inventory.columns.category") }, [text(group.categoryName || "—")]),
    el("td", { "data-label": t("inventory.columns.location") }, [
      text(tCount("inventory.locationsCount", group.locationIds.size)),
    ]),
    el("td", { "data-label": t("inventory.columns.quantity"), class: "inventory-table__qty" }, [text(String(group.quantity))]),
    el("td", { "data-label": t("inventory.columns.expires") }, [expiryCell(group.earliestExpiry, group.expirationSource)]),
    el("td", { "data-label": "" }, []),
  ]);

  const detailRow = el("tr", { hidden: true }, [
    el("td", { colspan: "6" }, [
      el("div", { class: "stack" }, group.batches.map((batch) => el("div", { class: "row row--between" }, [
        text(t("inventory.groupBatchLine", {
          quantity: batch.quantity,
          location: batch.location_path.join(" › "),
        })),
        el("a", { class: "btn btn--ghost", href: stocktakeHref(batch.location_id) }, [
          text(t("inventory.countThisShelf")),
        ]),
      ]))),
    ]),
  ]);

  const fragment = document.createDocumentFragment();
  fragment.append(row, detailRow);
  return fragment;
}

function productCell(imageUrl, name, productId) {
  const thumb = imageUrl ? [el("img", { class: "inventory-table__thumb", src: imageUrl, alt: "" })] : [];
  // A plain template, not withStorageParam, for the same reason
  // stocktakeHref below gives: it would carry this page's own filter/sort
  // query params into products.html, which has no use for them.
  const href = `/products.html?storage=${encodeURIComponent(storageId)}&product=${encodeURIComponent(productId)}`;
  return el("span", { class: "inventory-table__product" }, [
    ...thumb,
    el("a", { href }, [text(name)]),
  ]);
}

// stocktakeHref links a row's location to its stocktake sheet, the same
// plain-template shape js/pages/locations.js uses — not withStorageParam,
// which would carry this page's own filter/sort query params along into a
// page that has no use for them.
function stocktakeHref(locationId) {
  return `/stocktake.html?location=${encodeURIComponent(locationId)}&storage=${encodeURIComponent(storageId)}`;
}

function expiryCell(dateStr, source) {
  const children = [];
  if (!dateStr) {
    children.push(el("span", { class: "urgency urgency--none" }, [text(t("inventory.urgency.none"))]));
    return el("span", {}, children);
  }
  const band = classifyUrgency(dateStr);
  const formatted = formatDate(new Date(dateStr), { timeZone: "UTC" });
  children.push(el("span", { class: `urgency urgency--${band}` }, [text(formatted)]));
  if (source === "user") {
    children.push(text(" "));
    children.push(el("span", { class: "inventory-table__user-marker" }, [text(t("inventory.userSetExpiry"))]));
  }
  return el("span", {}, children);
}

// --- Filter selects ------------------------------------------------------

function flattenTree(nodes, depth = 0) {
  const out = [];
  for (const node of nodes) {
    out.push({ id: node.id, name: node.name, depth });
    out.push(...flattenTree(node.children || [], depth + 1));
  }
  return out;
}

function populateSelect(select, flatNodes, allLabel) {
  clearChildren(select);
  select.append(el("option", { value: "" }, [text(allLabel)]));
  for (const node of flatNodes) {
    select.append(el("option", { value: node.id }, [text(NBSP.repeat(2 * node.depth) + node.name)]));
  }
}

// --- Errors ----------------------------------------------------------------

function showError(err) {
  errorBox.textContent = err && err.code ? apiErrorMessage(err) : t("inventory.networkError");
  errorBox.hidden = false;
}
