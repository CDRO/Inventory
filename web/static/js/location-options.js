// Location pickers as plain <select> elements, for the upload and review
// screens of docs/specs/06-vision-shelf-ingestion.md, and for the batch
// split/move picker on products.html (docs/specs/28-batch-move-quick-create.md).
//
// GET /locations answers with a nested tree; a select needs a flat list. Depth
// is shown by indentation in the option's text, so the picker stays a native
// control — keyboard, screen reader and phone wheel all work — rather than a
// custom widget.

import { get } from "./api.js";
import { openTreeManager } from "./tree-modal.js";

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
 * js/pages/shopping-list.js once js/tree-modal.js resolves, so a field's
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

/**
 * openLocationField is what a location field's "+ New location" trigger
 * calls: open js/tree-modal.js, then refresh every currently open
 * location field from a single GET once it resolves — never one request per
 * field (docs/specs/26-location-quick-create.md). Shared by
 * js/pages/review.js and js/pages/shopping-list.js, the two review-style
 * screens 26 was written for (09's consume-review.js has none — see
 * docs/specs/26-location-quick-create.md's "Scope"), and by
 * js/pages/products.js's batch split/move picker
 * (docs/specs/28-batch-move-quick-create.md) — there, a batch row's split
 * and move target fields both count as "open" for the refresh below, since
 * both exist in the DOM regardless of which form is toggled visible.
 *
 * @param {Object} args
 * @param {string} args.storageId
 * @param {HTMLButtonElement} args.trigger - disabled for the duration of the
 *   modal. Without this, a second click before the first dialog closes would
 *   open a second one stacked on top of it — openTreeManager returns a
 *   promise the caller never awaits before the user can click again.
 * @param {HTMLSelectElement} args.openedSelect - the field whose trigger this
 *   is; preselected once resolved, but only when exactly one location was
 *   created (docs/specs/26-location-quick-create.md).
 * @param {() => HTMLSelectElement[]} args.getOpenSelects - returns every
 *   currently open location field to refresh, called after the modal closes
 *   so a page's own state since the trigger was clicked (e.g. a
 *   meanwhile-resolved shopping-list line) is reflected.
 * @param {(message: string) => void} args.onError - shown only if the
 *   post-close refresh itself fails; a location the user just created can
 *   still exist server-side even though this call never sees it.
 */
export async function openLocationField({ storageId, trigger, openedSelect, getOpenSelects, onError }) {
  if (trigger.disabled) return; // a dialog for this trigger is already open
  trigger.disabled = true;
  let createdIds;
  try {
    ({ createdIds } = await openTreeManager(storageId, { kind: "locations" }));
  } finally {
    trigger.disabled = false;
  }

  let locations;
  try {
    locations = await fetchLocations(storageId);
  } catch {
    onError(
      createdIds.length > 0
        ? "A location was created, but the list could not be refreshed. Reload the page to see it."
        : "Could not reach the server. Check your connection and try again.",
    );
    return;
  }

  const preselect = createdIds.length === 1 ? createdIds[0] : null;
  for (const select of getOpenSelects()) {
    refreshLocationOptions(select, locations, select === openedSelect && preselect ? preselect : select.value);
  }
}
