import "../register-sw.js";

// Page module for dashboard.html — the reorder dashboard and shopping export
// of docs/specs/10-reorder-and-shopping-export.md.
//
// Two things this page deliberately does not do, for the same reasons
// shopping-list.js does not:
//
//   - It never contacts Iconify, SerpAPI or Google. Image suggestions arrive
//     as URLs on this origin, already fetched and normalized server-side.
//   - It never renders a picture unless the server's own classification says
//     an external search is worth it (`needs_image_search`); a catalog hit
//     is shown as a card instead, at no provider cost.
//
// The "Add item" flow is a match-then-confirm pair, not a single call: the
// server never creates or updates anything until the confirm step, mirroring
// the shopping-list resolution flow this page's backend reuses
// (internal/matching, docs/specs/07-shopping-list-reconciliation.md).

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
import { get, post, ApiError } from "../api.js";
import { el, text, clearChildren, qs, fromTemplate } from "../dom.js";
import { offerBarcodeCapture } from "../barcode-offer.js";
import { t, apiErrorMessage } from "../i18n.js";

const switcherContainer = qs("#storage-switcher");
const errorBox = qs("#error");
const nameInput = qs("#add-item-name");
const checkButton = qs("#add-item-check");
const addItemResult = qs("#add-item-result");
const outOfStockList = qs("#out-of-stock-list");
const outOfStockCount = qs("#out-of-stock-count");
const lowStockList = qs("#low-stock-list");
const lowStockCount = qs("#low-stock-count");
const rowTemplate = qs("#reorder-row-template");
const exportCsvLink = qs("#export-csv");
const exportPdfButton = qs("#export-pdf");
const totalItemsValue = qs("#analytics-total-items");
const locationDistribution = qs("#location-distribution");
const turnoverChartContainer = qs("#turnover-chart");
const turnoverEmpty = qs("#turnover-empty");
const turnoverGranularity = qs("#turnover-granularity");

let storageId = null;
// The uPlot instance backing the turnover chart, torn down and rebuilt on
// every load — uPlot has no "replace the data and keep the instance" path
// that also handles a granularity change resizing the x-axis labels, and
// this chart is small enough that a rebuild is not worth optimizing away.
let turnoverChart = null;
// The dashboard's last fetch, kept only so the PDF export can render exactly
// what is on screen without a second round trip.
let lastDashboard = { out_of_stock: [], low_stock: [] };

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
  if (new URLSearchParams(location.search).get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });
  renderInboxLink(document.querySelector("#inbox-link"), storageId);
  exportCsvLink.href = `${basePath()}/export?format=csv`;

  checkButton.addEventListener("click", checkItem);
  exportPdfButton.addEventListener("click", downloadPdf);
  turnoverGranularity.addEventListener("change", () => loadTurnover(turnoverGranularity.value));

  await Promise.all([loadDashboard(), loadAnalytics(), initGamification(storageId)]);
}

function basePath() {
  return `/api/storages/${storageId}/dashboard/reorder`;
}

async function loadDashboard() {
  clearError();
  try {
    const dashboard = await get(basePath());
    lastDashboard = dashboard;
    renderBucket(outOfStockList, outOfStockCount, dashboard.out_of_stock, (item) =>
      t("dashboard.outOfStock.needsAtLeast", { min: item.min_stock }),
    );
    renderBucket(lowStockList, lowStockCount, dashboard.low_stock, (item) =>
      t("dashboard.lowStock.ofMin", { current: item.current_stock, min: item.min_stock }),
    );
  } catch (err) {
    showError(err);
  }
}

function renderBucket(container, countBadge, items, describe) {
  clearChildren(container);
  countBadge.textContent = String(items.length);

  if (items.length === 0) {
    container.append(el("p", { class: "empty-state" }, [text(t("dashboard.nothingHere"))]));
    return;
  }

  for (const item of items) {
    const row = fromTemplate(rowTemplate);
    row.querySelector('[data-field="name"]').textContent = item.name;
    row.querySelector('[data-field="stock"]').textContent = describe(item);
    container.append(row);
  }
}

function analyticsBasePath() {
  return `/api/storages/${storageId}/dashboard/analytics`;
}

// loadAnalytics fetches the stat tile and location-distribution chart, and
// kicks off the turnover chart at whatever granularity the select is
// currently showing (docs/specs/11-reporting-and-analytics.md).
async function loadAnalytics() {
  try {
    const dashboard = await get(`${analyticsBasePath()}?granularity=${turnoverGranularity.value}`);
    totalItemsValue.textContent = String(dashboard.total_items);
    renderLocationDistribution(dashboard.location_distribution);
    renderTurnoverChart(dashboard.turnover);
  } catch (err) {
    showError(err);
  }
}

// loadTurnover re-fetches just the granularity-dependent chart when the
// select changes, rather than re-running the whole analytics load — the stat
// tile and location distribution do not depend on granularity at all.
async function loadTurnover(granularity) {
  try {
    const dashboard = await get(`${analyticsBasePath()}?granularity=${granularity}`);
    renderTurnoverChart(dashboard.turnover);
  } catch (err) {
    showError(err);
  }
}

// renderLocationDistribution draws the per-location bars in plain CSS,
// sorted descending by item_count (already the order the API returns them
// in) and magnitude-colored via bar width — a list satisfies "heatmap" per
// the spec without a literal geographic rendering.
function renderLocationDistribution(rows) {
  clearChildren(locationDistribution);

  if (rows.length === 0) {
    locationDistribution.append(el("p", { class: "empty-state" }, [text(t("dashboard.noStockRecorded"))]));
    return;
  }

  const max = Math.max(...rows.map((row) => row.item_count));
  for (const row of rows) {
    const pct = max > 0 ? Math.round((row.item_count / max) * 100) : 0;
    locationDistribution.append(
      el("div", { class: "bar-row" }, [
        el("span", { class: "bar-row__label", title: row.location_name }, [text(row.location_name)]),
        el("span", { class: "bar-row__track" }, [
          el("span", { class: "bar-row__fill", style: `width: ${pct}%` }),
        ]),
        el("span", { class: "bar-row__value" }, [text(String(row.item_count))]),
      ]),
    );
  }
}

// renderTurnoverChart (re)builds the vendored uPlot instance showing
// purchased vs. consumed per period (web/static/vendor/README.md records
// where uPlot came from and why). uPlot plots numeric x values, so each
// period's label is kept alongside its index and shown via a custom axis
// formatter rather than a real timestamp — the API already buckets by
// period, so the chart does not need to re-derive one.
function renderTurnoverChart(rows) {
  if (turnoverChart) {
    turnoverChart.destroy();
    turnoverChart = null;
  }
  clearChildren(turnoverChartContainer);

  turnoverEmpty.hidden = rows.length > 0;
  if (rows.length === 0) {
    return;
  }

  const labels = rows.map((row) => row.period);
  const data = [
    rows.map((_, i) => i),
    rows.map((row) => row.purchased),
    rows.map((row) => row.consumed),
  ];

  turnoverChart = new uPlot(
    {
      width: turnoverChartContainer.clientWidth || 600,
      height: 256,
      series: [
        {},
        { label: t("dashboard.analytics.purchased"), stroke: "#2563eb", fill: "rgba(37, 99, 235, 0.15)" },
        { label: t("dashboard.analytics.consumed"), stroke: "#b91c1c", fill: "rgba(185, 28, 28, 0.15)" },
      ],
      axes: [
        // x is a period index, not a timestamp, so ticks are forced to whole
        // numbers (incrs: [1]) and rendered back through `labels` — otherwise
        // uPlot's default linear-axis tick chooser can land on fractional
        // positions that have no corresponding period label.
        { incrs: [1], values: (_u, splits) => splits.map((i) => labels[i] ?? "") },
        {},
      ],
      scales: {
        x: { time: false, range: rows.length > 1 ? [0, rows.length - 1] : [-0.5, 0.5] },
      },
    },
    data,
    turnoverChartContainer,
  );
}

// checkItem runs the match-preview step: nothing is created or changed yet.
async function checkItem() {
  const name = nameInput.value.trim();
  if (!name) {
    showError(new Error(t("dashboard.addItem.typeNameFirst")));
    return;
  }

  clearError();
  checkButton.disabled = true;
  try {
    const match = await post(`${basePath()}/items/match`, { name });
    renderMatch(name, match);
  } catch (err) {
    showError(err);
  } finally {
    checkButton.disabled = false;
  }
}

// renderMatch draws the per-status choices, then wires a single "Confirm"
// action whose payload depends entirely on what was picked — mirroring
// shopping-list.js's renderDetail, cut down to the one line this page ever
// has open at a time.
function renderMatch(name, match) {
  clearChildren(addItemResult);
  addItemResult.hidden = false;

  // `chosenProductId` is null for "create a new product"; set for "adjust
  // this existing product's min_stock instead" — the single flag the confirm
  // handler below branches on, the same role node.dataset.productId plays in
  // shopping-list.js.
  let chosenProductId = null;
  let prefill = { categoryId: null, itemType: "", iconName: null, shelfLifeDays: null };

  // The confirm step's min-stock field starts at 1 for a new product, but at
  // the matched product's own current threshold for an existing one —
  // defaulting an existing product to 1 would mean confirming without
  // touching the field silently overwrites a real threshold. A product can be
  // a confident local match while sitting outside both dashboard buckets
  // entirely (already well-stocked, so its current min_stock is otherwise
  // invisible here), which is exactly why the match response carries it per
  // product.
  const minStockInput = el("input", { type: "number", min: "1", step: "1", value: "1" });

  if (match.status === "exact_match" && match.matched_product) {
    chosenProductId = match.matched_product.id;
    minStockInput.value = String(match.matched_product.min_stock || 1);
    addItemResult.append(el("p", {}, [text(t("dashboard.addItem.matches", { name: match.matched_product.name }))]));
  } else if (match.status === "ambiguous") {
    addItemResult.append(el("p", {}, [text(t("dashboard.addItem.whichOne"))]));
    const buttons = match.candidates.map((candidate) =>
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          onclick: (event) => {
            chosenProductId = candidate.id;
            minStockInput.value = String(candidate.min_stock || 1);
            for (const other of event.currentTarget.parentElement.querySelectorAll("button")) {
              other.classList.remove("btn--selected");
            }
            event.currentTarget.classList.add("btn--selected");
          },
        },
        [text(candidate.name)],
      ),
    );
    addItemResult.append(el("div", { class: "stack" }, buttons));
  } else if (match.catalog) {
    // new_item with a catalog hit: one-click acceptance, no external call.
    // The card's picture is shown, but not sent: a picture reaches a product
    // only as a picked suggestion the server copies into permanent storage,
    // never by an address (docs/specs/07-shopping-list-reconciliation.md).
    prefill = {
      categoryId: null,
      itemType: match.catalog.item_type || "",
      iconName: match.catalog.icon_name,
      shelfLifeDays: match.catalog.default_shelf_life_days,
    };
    addItemResult.append(renderCatalogCard(match.catalog));
  } else {
    addItemResult.append(
      el("p", { class: "empty-state" }, [text(t("dashboard.addItem.nothingKnown"))]),
    );
    if (match.needs_image_search) {
      renderImageSuggestions(addItemResult, name);
    }
  }

  const confirmButton = el(
    "button",
    {
      type: "button",
      class: "btn btn--primary",
      onclick: () => confirmAddItem(name, chosenProductId, Number(minStockInput.value), prefill),
    },
    [text(chosenProductId ? t("dashboard.addItem.updateThreshold") : t("dashboard.addItem.addToReorderList"))],
  );

  addItemResult.append(
    el("div", { class: "row" }, [
      el("label", { class: "field" }, [text(t("dashboard.addItem.minimumStock")), minStockInput]),
      confirmButton,
    ]),
  );
}

function renderCatalogCard(catalog) {
  const lines = [el("strong", {}, [text(catalog.display_name)])];
  if (catalog.category_path) {
    lines.push(el("p", { class: "empty-state" }, [text(catalog.category_path)]));
  }
  if (catalog.default_shelf_life_days != null) {
    lines.push(
      el("p", { class: "empty-state" }, [text(t("dashboard.addItem.keepsAbout", { days: catalog.default_shelf_life_days }))]),
    );
  }
  return el("div", { class: "card stack" }, lines);
}

async function renderImageSuggestions(container, query) {
  const box = el("div", { class: "row" }, [
    el("span", { class: "empty-state" }, [text(t("dashboard.addItem.lookingForPictures"))]),
  ]);
  container.append(box);

  try {
    const body = await get(`/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`);
    clearChildren(box);
    for (const suggestion of body.suggestions || []) {
      box.append(
        el("img", { src: suggestion.url, alt: t("dashboard.addItem.suggestionAlt", { type: suggestion.type, query }), width: 96, height: 96, loading: "lazy" }),
      );
    }
    if (!body.suggestions || body.suggestions.length === 0) {
      box.append(el("span", { class: "empty-state" }, [text(t("dashboard.addItem.noPicturesFound"))]));
    }
  } catch {
    clearChildren(box);
    box.append(el("span", { class: "empty-state" }, [text(t("dashboard.addItem.pictureSearchUnavailable"))]));
  }
}

async function confirmAddItem(name, productId, minStock, prefill) {
  clearError();
  try {
    const added = await post(`${basePath()}/items`, {
      name,
      product_id: productId,
      min_stock: minStock,
      category_id: prefill.categoryId,
      item_type: prefill.itemType || undefined,
      icon_name: prefill.iconName,
      default_shelf_life_days: prefill.shelfLifeDays,
    });
    nameInput.value = "";
    addItemResult.hidden = true;
    clearChildren(addItemResult);
    await loadDashboard();
    // The third capture point of docs/specs/20-barcode-recall.md's offer.
    // Only a product this call created: adding a threshold to one that
    // already existed creates nothing to attach a code to.
    if (added?.created && added.product_id) {
      await offerBarcodeCapture(storageId, {
        productId: added.product_id,
        productName: added.name || name,
      });
    }
  } catch (err) {
    showError(err);
  }
}

// downloadPdf renders the currently displayed lists, client-side, from the
// vendored jsPDF + autotable build (web/static/vendor/README.md) — never a
// CDN, and never a second server-side implementation of the same document.
function downloadPdf() {
  const { jsPDF } = window.jspdf;
  const doc = new jsPDF();
  const rows = [
    ...lastDashboard.out_of_stock.map((item) => [item.name, "0", String(item.min_stock), String(suggestedQty(0, item.min_stock))]),
    ...lastDashboard.low_stock.map((item) => [item.name, String(item.current_stock), String(item.min_stock), String(suggestedQty(item.current_stock, item.min_stock))]),
  ];

  doc.text(t("dashboard.pdf.title"), 14, 16);
  doc.autoTable({
    startY: 22,
    head: [[t("dashboard.pdf.name"), t("dashboard.pdf.currentStock"), t("dashboard.pdf.minStock"), t("dashboard.pdf.suggestedQty")]],
    body: rows,
  });

  const filename = `reorder-list-${new Date().toISOString().slice(0, 10)}.pdf`;
  doc.save(filename);
}

function suggestedQty(current, min) {
  return Math.max(min - current, 1);
}

function showError(err) {
  errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : err.message || t("common.unexpectedError");
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
