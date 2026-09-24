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
import { createShelfLifeDetail, resolveInheritance } from "../category-shelf-life.js";
import { get, post, patch, ApiError } from "../api.js";
import { clearChildren, el, text } from "../dom.js";
import { t, apiErrorMessage } from "../i18n.js";

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
    renderDetail: createShelfLifeDetail({
      basePath,
      runMutation,
      showStatus,
      getInherited: () => inherited,
    }),
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
    "aria-label": t("categories.addRootNameLabel"),
    placeholder: t("categories.addRootNamePlaceholder"),
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
      el("button", { type: "submit", class: "btn" }, [text(t("categories.add"))]),
      el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text(t("common.cancel"))]),
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

function renderEmptyTree() {
  clearChildren(treeContainer);
  treeContainer.append(
    el("p", { class: "empty-state" }, [
      text(t("categories.empty")),
    ]),
  );
}

function showStatus(message) {
  statusLine.textContent = message;
  statusLine.hidden = false;
}

function showError(err) {
  if (!(err instanceof ApiError)) {
    errorBox.textContent = t("categories.networkError");
  } else {
    // apiErrorMessage renders the server's stable code in the active
    // language; a 422's per-field messages come straight from the server and
    // stay in English (docs/specs/19-localization.md's API-stays-English
    // boundary) — an out-of-range shelf life, say — because they are what
    // tell the person what to fix.
    const details = err.fields ? Object.values(err.fields).flat() : [];
    errorBox.textContent = [apiErrorMessage(err), ...details].join(" ");
  }
  errorBox.hidden = false;
}

function clearMessages() {
  errorBox.textContent = "";
  errorBox.hidden = true;
  statusLine.textContent = "";
  statusLine.hidden = true;
}
