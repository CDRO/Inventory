import "../register-sw.js";

// Page module for categories.html — a storage's category tree
// (docs/specs/02-data-model.md) and the shelf-life rules that live on it
// (docs/specs/08-expiration-and-classification.md: "a user can edit them in
// the category tree without a code change or redeploy").
//
// The tree interaction — expand/collapse, inline add-child, rename,
// drag-and-drop re-parenting and the "Move to…" picker — is the shared
// TreeView (js/tree.js), exactly as locations.js uses it. What this page adds
// is each node's shelf-life rule, drawn through TreeView's renderDetail hook
// and saved through its own route, which recomputes existing dates and says
// how many moved.
//
// Deleting a category is absent for the reason locations.js gives for
// locations: no spec asks the tree UI for it, and when one does it belongs in
// the shared component so both trees get it together.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
import { TreeView } from "../tree.js";
import { get, post, patch, ApiError } from "../api.js";
import { clearChildren, el, text } from "../dom.js";

// The server's bound (maxShelfLifeDays in internal/httpapi/expiry.go). The
// input carries it so a browser flags an out-of-range number before a round
// trip; the server still decides.
const MAX_SHELF_LIFE_DAYS = 36500;

const switcherContainer = document.querySelector("#storage-switcher");
const treeContainer = document.querySelector("#tree");
const errorBox = document.querySelector("#error");
const statusLine = document.querySelector("#status");
const hint = document.querySelector("#hint");
const addRootButton = document.querySelector("#add-root");

let storageId = null;
let view = null;

/**
 * inherited maps a category id to the nearest ancestor rule it falls back on
 * when it sets none of its own, rebuilt from every server response. TreeView
 * hands renderDetail one node at a time, with no parent chain, so the chain is
 * resolved here once per load.
 *
 * @type {Map<string, {days: number, from: string}|null>}
 */
let inherited = new Map();

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
    // storages.html owns the picker and the empty state, as locations.js
    // explains.
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
  initGamification(storageId);

  view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) => runMutation(() => createCategory(parentId, name)),
    onRename: (id, name) => runMutation(() => patch(`${basePath()}/${id}`, { name })),
    // parent_id is sent explicitly, including as null — "make this a root" —
    // for the reason locations.js's moveLocation gives.
    onMove: (id, newParentId) => runMutation(() => patch(`${basePath()}/${id}`, { parent_id: newParentId })),
    renderDetail: renderShelfLife,
  });

  addRootButton.addEventListener("click", showAddRootForm);

  await reload();
}

// showAddRootForm adds a top-level category, the one case TreeView's inline
// add-child cannot cover — the same inline form locations.js uses.
function showAddRootForm() {
  if (document.querySelector("#add-root-form")) return;

  const input = el("input", {
    type: "text",
    id: "add-root-name",
    "aria-label": "Name of the new top-level category",
    placeholder: "Food",
    required: true,
  });

  const form = el(
    "form",
    {
      id: "add-root-form",
      class: "row",
      onsubmit: (event) => {
        event.preventDefault();
        const name = input.value.trim();
        if (!name) return;
        form.remove();
        runMutation(() => createCategory(null, name));
      },
    },
    [
      input,
      el("button", { type: "submit", class: "btn" }, [text("Add")]),
      el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text("Cancel")]),
    ],
  );

  treeContainer.before(form);
  input.focus();
}

function basePath() {
  return `/api/storages/${storageId}/categories`;
}

function createCategory(parentId, name) {
  return post(basePath(), { name, parent_id: parentId });
}

/**
 * renderShelfLife draws one node's rule as a button that opens its editor.
 * The label always says which rule is in force, so an inheriting node is
 * never mistaken for one with no expiry at all.
 *
 * @param {import("../tree.js").TreeNode & {default_shelf_life_days: number|null}} node
 */
function renderShelfLife(node) {
  const wrapper = el("span", { class: "tree-detail" });
  const button = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost tree-shelf-life",
      title: `Edit the shelf life of ${node.name}`,
      onclick: () => openShelfLifeEditor(node, wrapper),
    },
    [text(shelfLifeLabel(node))],
  );
  wrapper.append(button);
  return wrapper;
}

function shelfLifeLabel(node) {
  if (node.default_shelf_life_days != null) {
    return days(node.default_shelf_life_days);
  }
  const rule = inherited.get(node.id);
  if (rule) {
    return `${days(rule.days)} (from ${rule.from})`;
  }
  // Nothing above sets a rule: the product's item type decides
  // (docs/specs/08-expiration-and-classification.md, step 5).
  return "By item type";
}

function days(n) {
  return n === 1 ? "1 day" : `${n} days`;
}

// openShelfLifeEditor swaps the label for a small inline form. An empty field
// means "inherit", which is sent as null — a different statement from 0,
// which would mean "expires the day it arrives".
function openShelfLifeEditor(node, wrapper) {
  const input = el("input", {
    type: "number",
    min: "0",
    max: String(MAX_SHELF_LIFE_DAYS),
    step: "1",
    inputmode: "numeric",
    class: "tree-shelf-life-input",
    "aria-label": `Shelf life of ${node.name} in days; leave empty to inherit`,
    placeholder: "Inherit",
    value: node.default_shelf_life_days == null ? "" : String(node.default_shelf_life_days),
  });

  const form = el(
    "form",
    {
      class: "row",
      onsubmit: (event) => {
        event.preventDefault();
        if (!input.reportValidity()) return;
        const raw = input.value.trim();
        const value = raw === "" ? null : Number(raw);
        runMutation(() => saveShelfLife(node, value));
      },
    },
    [
      input,
      el("button", { type: "submit", class: "btn btn--primary" }, [text("Save")]),
      el("button", { type: "button", class: "btn btn--ghost", onclick: () => view.render(view.nodes) }, [
        text("Cancel"),
      ]),
    ],
  );

  clearChildren(wrapper);
  wrapper.append(form);
  input.focus();
}

async function saveShelfLife(node, value) {
  const body = await patch(`${basePath()}/${node.id}/shelf-life`, { default_shelf_life_days: value });
  // The count is the point of this route
  // (docs/specs/08-expiration-and-classification.md): a person changing a
  // rule is doing it to change dates, so say how many actually moved.
  const moved = body.recomputed_batches;
  showStatus(
    moved === 0
      ? `Saved the shelf life for ${node.name}. No existing expiry dates needed to change.`
      : `Saved the shelf life for ${node.name}. ${moved} existing expiry ${moved === 1 ? "date was" : "dates were"} updated.`,
  );
}

// runMutation applies one change and then re-reads the tree, redrawing from
// the server's answer for the reasons locations.js's runMutation gives.
async function runMutation(mutate) {
  clearMessages();
  try {
    await mutate();
  } catch (err) {
    showError(err);
  }
  await reload();
}

async function reload() {
  try {
    const body = await get(basePath());
    const empty = body.items.length === 0;
    hint.hidden = empty;
    if (empty) {
      renderEmptyTree();
      return;
    }
    inherited = resolveInheritance(body.items);
    view.render(body.items);
  } catch (err) {
    showError(err);
  }
}

// resolveInheritance walks the tree once, carrying the nearest rule set above
// each node, so every node's label can name the rule it falls back on.
function resolveInheritance(roots) {
  const out = new Map();
  const walk = (nodes, fromAbove) => {
    for (const node of nodes) {
      out.set(node.id, fromAbove);
      const own = node.default_shelf_life_days;
      walk(node.children || [], own != null ? { days: own, from: node.name } : fromAbove);
    }
  };
  walk(roots, null);
  return out;
}

function renderEmptyTree() {
  clearChildren(treeContainer);
  treeContainer.append(
    el("p", { class: "empty-state" }, [
      text("No categories yet. Add a top-level one — Food, Household, Collectibles — to sort products and set how long they keep."),
    ]),
  );
}

function showStatus(message) {
  statusLine.textContent = message;
  statusLine.hidden = false;
}

function showError(err) {
  if (!(err instanceof ApiError)) {
    errorBox.textContent = "Could not reach the server. Check your connection and try again.";
  } else {
    // ApiError.message is the server's human-readable text: a cycle and a
    // category products still use each say what went wrong. A 422's message
    // is generic, so its per-field messages — an out-of-range shelf life,
    // say — are what tell the person what to fix.
    const details = err.fields ? Object.values(err.fields).flat() : [];
    errorBox.textContent = [err.message, ...details].join(" ");
  }
  errorBox.hidden = false;
}

function clearMessages() {
  errorBox.textContent = "";
  errorBox.hidden = true;
  statusLine.textContent = "";
  statusLine.hidden = true;
}
