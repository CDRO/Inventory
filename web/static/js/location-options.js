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
 * through textContent — a location name is user text. Each option carries
 * data-location so refreshLocationOptions below can tell it apart from an
 * option this module did not add — a leading placeholder, or review.html's
 * synthesized "Create: …" proposed-path option.
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
    option.dataset.location = "1";
    select.append(option);
  }
}

/**
 * refreshLocationOptions replaces every option appendLocationOptions
 * previously added to select with a fresh flat list, leaving every other
 * option exactly where it was. Used by js/pages/review.js and
 * js/pages/shopping-list.js once js/location-modal.js resolves, so a field's
 * placeholder or proposed-path option and its current selection survive a
 * tree that just changed underneath it (docs/specs/26-location-quick-create.md).
 *
 * @param {HTMLSelectElement} select
 * @param {FlatLocation[]} locations
 * @param {string} [keepValue] - reselected if still a valid option after the
 *   refresh; defaults to the select's own current value.
 */
export function refreshLocationOptions(select, locations, keepValue = select.value) {
  for (const option of select.querySelectorAll("option[data-location]")) {
    option.remove();
  }
  appendLocationOptions(select, locations);
  if ([...select.options].some((option) => option.value === keepValue)) {
    select.value = keepValue;
  }
}
