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
// Two things this page deliberately does not do:
//
//   - It never contacts Iconify, SerpAPI or Google. Image suggestions arrive
//     as URLs on this origin, already fetched and normalized server-side. An
//     <img> pointing at a provider would leak the viewer's IP and user-agent
//     to that host and would not load at all on a LAN-only NAS.
//   - It never sends a catalog id back, because it is never given one. A
//     catalog card is display fields only; the server re-derives which row was
//     shown when it needs to link a variant.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { get, post, ApiError } from "../api.js";
import { el, text, clearChildren, qs } from "../dom.js";

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
  submitButton.addEventListener("click", submitList);
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

function renderItem(item) {
  const node = itemTemplate.content.firstElementChild.cloneNode(true);

  field(node, "raw_text").textContent = item.raw_text;
  field(node, "status").textContent = statusLabel(item.status);

  const quantityInput = field(node, "quantity");
  quantityInput.value = String(item.quantity ?? 1);

  const detail = field(node, "detail");
  renderDetail(detail, item, node);

  const resolveButton = node.querySelector('[data-action="resolve"]');
  if (item.status === "resolved") {
    resolveButton.disabled = true;
    quantityInput.disabled = true;
  } else {
    resolveButton.addEventListener("click", () =>
      resolveItem(node, item, Number(quantityInput.value)),
    );
  }

  return node;
}

// renderDetail draws the per-state choices. `node.dataset.productId` is the
// single place a chosen product is recorded, so every state below agrees on
// how the confirm step reads its decision.
function renderDetail(detail, item, node) {
  clearChildren(detail);

  if (item.status === "resolved") {
    detail.append(el("p", { class: "empty-state" }, [text("Confirmed.")]));
    return;
  }

  if (item.status === "exact_match" && item.matched_product) {
    node.dataset.productId = item.matched_product.id;
    detail.append(
      el("p", {}, [text(`Matches ${item.matched_product.name}.`)]),
    );
    return;
  }

  if (item.status === "ambiguous") {
    detail.append(el("p", {}, [text("Which one did you mean?")]));
    detail.append(
      el(
        "div",
        { class: "stack" },
        item.candidates.map((candidate) =>
          el(
            "button",
            {
              type: "button",
              class: "btn btn--ghost",
              onclick: (event) => {
                node.dataset.productId = candidate.id;
                markChosen(detail, event.currentTarget);
              },
            },
            [text(candidate.name)],
          ),
        ),
      ),
    );
    return;
  }

  // new_item, with or without a catalog description.
  if (item.catalog) {
    detail.append(renderCatalogCard(item.catalog));
    return;
  }

  detail.append(
    el("p", { class: "empty-state" }, [
      text("Nothing known about this yet. Pick a picture to start a new product."),
    ]),
  );
  renderImageSuggestions(detail, item.raw_text);
}

// renderCatalogCard shows what the anonymous catalog knows. Everything here is
// a display field — there is no id to render even if this code wanted one.
function renderCatalogCard(catalog) {
  const lines = [el("strong", {}, [text(catalog.display_name)])];

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
  if (catalog.variants && catalog.variants.length > 0) {
    lines.push(el("p", {}, [text("Or one of these:")]));
    lines.push(
      el(
        "div",
        { class: "row" },
        catalog.variants.map((name) => el("span", { class: "badge" }, [text(name)])),
      ),
    );
  }

  return el("div", { class: "card stack" }, lines);
}

// renderImageSuggestions asks the server for pictures. Every URL it renders is
// on this origin — see this file's header for why that matters.
async function renderImageSuggestions(detail, query) {
  const container = el("div", { class: "row" }, [
    el("span", { class: "empty-state" }, [text("Looking for pictures…")]),
  ]);
  detail.append(container);

  try {
    const body = await get(
      `/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`,
    );
    clearChildren(container);

    if (!body.suggestions || body.suggestions.length === 0) {
      container.append(
        el("span", { class: "empty-state" }, [
          text("No pictures found. You can add one later."),
        ]),
      );
      return;
    }

    for (const suggestion of body.suggestions) {
      container.append(
        el("img", {
          src: suggestion.url,
          alt: `${suggestion.type} suggestion for ${query}`,
          width: 96,
          height: 96,
          loading: "lazy",
        }),
      );
    }
  } catch {
    clearChildren(container);
    // A provider being unreachable must not block the line: the spec's own
    // degradation rule, carried through to the UI.
    container.append(
      el("span", { class: "empty-state" }, [
        text("Picture search is unavailable right now. You can still continue."),
      ]),
    );
  }
}

function markChosen(detail, button) {
  for (const other of detail.querySelectorAll("button")) {
    other.classList.remove("btn--selected");
  }
  button.classList.add("btn--selected");
}

async function resolveItem(node, item, quantity) {
  clearError();
  const productId = node.dataset.productId || null;

  try {
    const updated = await post(
      `${basePath()}/${listId}/items/${item.id}/resolve`,
      { product_id: productId, quantity },
    );
    node.replaceWith(renderItem({ ...item, ...updated, status: "resolved" }));
  } catch (err) {
    showError(err);
  }
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
