// Location pickers as plain <select> elements, for the upload and review
// screens of docs/specs/06-vision-shelf-ingestion.md.
//
// GET /locations answers with a nested tree; a select needs a flat list. Depth
// is shown by indentation in the option's text, so the picker stays a native
// control — keyboard, screen reader and phone wheel all work — rather than a
// custom widget.

import { get } from "./api.js";

const NBSP = String.fromCharCode(0xa0);

/**
 * @typedef {{id: string, name: string, depth: number, path: string[]}} FlatLocation
 */

/**
 * fetchLocations loads a storage's tree and flattens it depth-first.
 *
 * @param {string} storageId
 * @returns {Promise<FlatLocation[]>}
 */
export async function fetchLocations(storageId) {
  const body = await get(`/api/storages/${storageId}/locations`);
  const out = [];
  const walk = (nodes, depth, path) => {
    for (const node of nodes) {
      const here = [...path, node.name];
      out.push({ id: node.id, name: node.name, depth, path: here });
      walk(node.children || [], depth + 1, here);
    }
  };
  walk(body.items, 0, []);
  return out;
}

/**
 * appendLocationOptions adds one option per location to select. Names go in
 * through textContent — a location name is user text.
 *
 * @param {HTMLSelectElement} select
 * @param {FlatLocation[]} locations
 */
export function appendLocationOptions(select, locations) {
  for (const loc of locations) {
    const option = document.createElement("option");
    option.value = loc.id;
    // Non-breaking spaces survive in an option's text where plain ones
    // collapse, so the indentation actually shows.
    option.textContent = NBSP.repeat(2 * loc.depth) + loc.name;
    select.append(option);
  }
}
