// Category picker as a plain <select>, the same shape location-options.js
// uses for locations — for a new product's category while reviewing an
// ingestion proposal (docs/specs/06-vision-shelf-ingestion.md) or the
// shopping-list "New Item" fill-in (docs/specs/07-shopping-list-reconciliation.md).
// Consumption logging (docs/specs/09-consumption-logging.md) never creates a
// product, so it has no use for this.
//
// GET /categories answers with a nested tree; a select needs a flat list.
// Depth is shown by indentation, the same native-control reasoning
// location-options.js documents.

import { get } from "./api.js";
import { openTreeManager } from "./tree-modal.js";
import { t } from "./i18n.js";

const NBSP = String.fromCharCode(0xa0);

/**
 * @typedef {{id: string, name: string, depth: number}} FlatCategory
 */

/**
 * fetchCategories loads a storage's category tree and flattens it depth-first.
 *
 * @param {string} storageId
 * @returns {Promise<FlatCategory[]>}
 */
export async function fetchCategories(storageId) {
  const body = await get(`/api/storages/${storageId}/categories`);
  const out = [];
  const walk = (nodes, depth) => {
    for (const node of nodes) {
      out.push({ id: node.id, name: node.name, depth });
      walk(node.children || [], depth + 1);
    }
  };
  walk(body.items, 0);
  return out;
}

/**
 * appendCategoryOptions adds one option per category to select, plus a
 * leading "no category" option whose value is the empty string — a new
 * product's category is optional. Each category option carries
 * data-category so refreshCategoryOptions below can tell it apart from the
 * "no category" placeholder, the same way location-options.js's
 * data-location marks its own.
 *
 * @param {HTMLSelectElement} select
 * @param {FlatCategory[]} categories
 */
export function appendCategoryOptions(select, categories) {
  const none = document.createElement("option");
  none.value = "";
  none.textContent = t("categoryOptions.none");
  select.append(none);

  for (const cat of categories) {
    const option = document.createElement("option");
    option.value = cat.id;
    option.textContent = NBSP.repeat(2 * cat.depth) + cat.name;
    option.dataset.category = "1";
    select.append(option);
  }
}

/**
 * refreshCategoryOptions replaces every option appendCategoryOptions
 * previously added to select with a fresh flat list, leaving the leading
 * "no category" option exactly where it was — the same shape
 * location-options.js's refreshLocationOptions gives for locations.
 *
 * @param {HTMLSelectElement} select
 * @param {FlatCategory[]} categories
 * @param {string} [keepValue] - reselected if still a valid option after the
 *   refresh; defaults to the select's own current value.
 */
export function refreshCategoryOptions(select, categories, keepValue = select.value) {
  for (const option of select.querySelectorAll("option[data-category]")) {
    option.remove();
  }
  for (const cat of categories) {
    const option = document.createElement("option");
    option.value = cat.id;
    option.textContent = NBSP.repeat(2 * cat.depth) + cat.name;
    option.dataset.category = "1";
    select.append(option);
  }
  if ([...select.options].some((option) => option.value === keepValue)) {
    select.value = keepValue;
  }
}

/**
 * openCategoryField is what a category field's "+ New category" trigger
 * calls: open js/tree-modal.js scoped to categories, then refresh every
 * currently open category field from a single GET once it resolves — never
 * one request per field, the same rule
 * docs/specs/26-location-quick-create.md sets for locations and
 * docs/specs/27-category-quick-create.md carries over unchanged. Shared by
 * js/pages/review.js and js/pages/shopping-list.js, the two page modules
 * with a category field to attach this trigger to.
 *
 * @param {Object} args
 * @param {string} args.storageId
 * @param {HTMLButtonElement} args.trigger - disabled for the duration of the
 *   modal, for the reason openLocationField's own doc comment gives.
 * @param {HTMLSelectElement} args.openedSelect - the field whose trigger this
 *   is; preselected once resolved, but only when exactly one category was
 *   created.
 * @param {() => HTMLSelectElement[]} args.getOpenSelects - returns every
 *   currently open category field to refresh.
 * @param {(message: string) => void} args.onError - shown only if the
 *   post-close refresh itself fails; a category the user just created can
 *   still exist server-side even though this call never sees it.
 */
export async function openCategoryField({ storageId, trigger, openedSelect, getOpenSelects, onError }) {
  if (trigger.disabled) return; // a dialog for this trigger is already open
  trigger.disabled = true;
  let createdIds;
  try {
    ({ createdIds } = await openTreeManager(storageId, { kind: "categories" }));
  } finally {
    trigger.disabled = false;
  }

  let categories;
  try {
    categories = await fetchCategories(storageId);
  } catch {
    onError(
      createdIds.length > 0
        ? t("categoryOptions.createdButRefreshFailed")
        : t("categoryOptions.networkError"),
    );
    return;
  }

  const preselect = createdIds.length === 1 ? createdIds[0] : null;
  for (const select of getOpenSelects()) {
    refreshCategoryOptions(select, categories, select === openedSelect && preselect ? preselect : select.value);
  }
}
