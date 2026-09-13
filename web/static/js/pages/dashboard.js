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
import { get, post, ApiError } from "../api.js";
import { el, text, clearChildren, qs, fromTemplate } from "../dom.js";

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

let storageId = null;
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

  await loadDashboard();
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
      `Out of stock — needs at least ${item.min_stock}`,
    );
    renderBucket(lowStockList, lowStockCount, dashboard.low_stock, (item) =>
      `${item.current_stock} of ${item.min_stock}`,
    );
  } catch (err) {
    showError(err);
  }
}

function renderBucket(container, countBadge, items, describe) {
  clearChildren(container);
  countBadge.textContent = String(items.length);

  if (items.length === 0) {
    container.append(el("p", { class: "empty-state" }, [text("Nothing here.")]));
    return;
  }

  for (const item of items) {
    const row = fromTemplate(rowTemplate);
    row.querySelector('[data-field="name"]').textContent = item.name;
    row.querySelector('[data-field="stock"]').textContent = describe(item);
    container.append(row);
  }
}

// checkItem runs the match-preview step: nothing is created or changed yet.
async function checkItem() {
  const name = nameInput.value.trim();
  if (!name) {
    showError(new Error("Type a product name first."));
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
  let prefill = { categoryId: null, itemType: "", imageUrl: null, iconName: null, shelfLifeDays: null };

  if (match.status === "exact_match" && match.matched_product) {
    chosenProductId = match.matched_product.id;
    addItemResult.append(el("p", {}, [text(`Matches ${match.matched_product.name}.`)]));
  } else if (match.status === "ambiguous") {
    addItemResult.append(el("p", {}, [text("Which one did you mean?")]));
    const buttons = match.candidates.map((candidate) =>
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          onclick: (event) => {
            chosenProductId = candidate.id;
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
    prefill = {
      categoryId: null,
      itemType: match.catalog.item_type || "",
      imageUrl: match.catalog.image_url,
      iconName: match.catalog.icon_name,
      shelfLifeDays: match.catalog.default_shelf_life_days,
    };
    addItemResult.append(renderCatalogCard(match.catalog));
  } else {
    addItemResult.append(
      el("p", { class: "empty-state" }, [text("Nothing known about this yet.")]),
    );
    if (match.needs_image_search) {
      renderImageSuggestions(addItemResult, name);
    }
  }

  const minStockInput = el("input", { type: "number", min: "1", step: "1", value: "1" });
  const confirmButton = el(
    "button",
    {
      type: "button",
      class: "btn btn--primary",
      onclick: () => confirmAddItem(name, chosenProductId, Number(minStockInput.value), prefill),
    },
    [text(chosenProductId ? "Update threshold" : "Add to reorder list")],
  );

  addItemResult.append(
    el("div", { class: "row" }, [
      el("label", { class: "field" }, [text("Minimum stock"), minStockInput]),
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
      el("p", { class: "empty-state" }, [text(`Keeps about ${catalog.default_shelf_life_days} days.`)]),
    );
  }
  return el("div", { class: "card stack" }, lines);
}

async function renderImageSuggestions(container, query) {
  const box = el("div", { class: "row" }, [
    el("span", { class: "empty-state" }, [text("Looking for pictures…")]),
  ]);
  container.append(box);

  try {
    const body = await get(`/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`);
    clearChildren(box);
    for (const suggestion of body.suggestions || []) {
      box.append(
        el("img", { src: suggestion.url, alt: `${suggestion.type} suggestion for ${query}`, width: 96, height: 96, loading: "lazy" }),
      );
    }
    if (!body.suggestions || body.suggestions.length === 0) {
      box.append(el("span", { class: "empty-state" }, [text("No pictures found. You can add one later.")]));
    }
  } catch {
    clearChildren(box);
    box.append(el("span", { class: "empty-state" }, [text("Picture search is unavailable right now.")]));
  }
}

async function confirmAddItem(name, productId, minStock, prefill) {
  clearError();
  try {
    await post(`${basePath()}/items`, {
      name,
      product_id: productId,
      min_stock: minStock,
      category_id: prefill.categoryId,
      item_type: prefill.itemType || undefined,
      image_url: prefill.imageUrl,
      icon_name: prefill.iconName,
      default_shelf_life_days: prefill.shelfLifeDays,
    });
    nameInput.value = "";
    addItemResult.hidden = true;
    clearChildren(addItemResult);
    await loadDashboard();
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

  doc.text("Reorder list", 14, 16);
  doc.autoTable({
    startY: 22,
    head: [["Name", "Current stock", "Min stock", "Suggested reorder qty"]],
    body: rows,
  });

  const filename = `reorder-list-${new Date().toISOString().slice(0, 10)}.pdf`;
  doc.save(filename);
}

function suggestedQty(current, min) {
  return Math.max(min - current, 1);
}

function showError(err) {
  errorBox.textContent = err instanceof ApiError ? err.message : err.message || "Something went wrong.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
