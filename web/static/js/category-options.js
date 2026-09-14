// Category picker as a plain <select>, the same shape location-options.js
// uses for locations — for a new product's category while reviewing an
// ingestion proposal (docs/specs/06-vision-shelf-ingestion.md). Consumption
// logging (docs/specs/09-consumption-logging.md) never creates a product,
// so it has no use for this.
//
// GET /categories answers with a nested tree; a select needs a flat list.
// Depth is shown by indentation, the same native-control reasoning
// location-options.js documents.

import { get } from "./api.js";

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
 * product's category is optional.
 *
 * @param {HTMLSelectElement} select
 * @param {FlatCategory[]} categories
 */
export function appendCategoryOptions(select, categories) {
  const none = document.createElement("option");
  none.value = "";
  none.textContent = "No category";
  select.append(none);

  for (const cat of categories) {
    const option = document.createElement("option");
    option.value = cat.id;
    option.textContent = NBSP.repeat(2 * cat.depth) + cat.name;
    select.append(option);
  }
}
