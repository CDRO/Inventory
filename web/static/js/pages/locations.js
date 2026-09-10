import "../register-sw.js";

// Page module for locations.html — Feature 1 of
// docs/specs/06-vision-shelf-ingestion.md: the storage's location tree, with
// expand/collapse, inline add-child, rename, drag-and-drop re-parenting and the
// keyboard-reachable "move to…" picker.
//
// None of that interaction lives here. It is all in the shared TreeView
// (js/tree.js, docs/specs/05-frontend-pwa-foundations.md), which the category
// tree will reuse unchanged; this module only resolves the storage, calls the
// API, and re-renders from the server's answer.
//
// Deleting a location is deliberately absent. The endpoint exists and refuses
// with 409 while the subtree still holds stock, but spec 06's frontend bullet
// lists expand/collapse, add-child, rename and re-parenting only — and the
// affordance belongs in the shared component when a spec asks for it, so that
// categories get it in the same change rather than growing a second tree UI.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { TreeView } from "../tree.js";
import { get, post, patch, ApiError } from "../api.js";
import { clearChildren, el, text } from "../dom.js";

const switcherContainer = document.querySelector("#storage-switcher");
const treeContainer = document.querySelector("#tree");
const errorBox = document.querySelector("#error");
const hint = document.querySelector("#hint");
const addRootButton = document.querySelector("#add-root");

let storageId = null;
let view = null;

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
    // Zero memberships, or more than one with nothing chosen yet. Both are
    // storages.html's job (docs/specs/05-frontend-pwa-foundations.md): it owns
    // the picker and the empty state, and duplicating either here would mean
    // two places to keep honest.
    location.assign("/storages.html");
    return;
  }

  storageId = resolved;
  rememberStorageId(storageId);
  if (new URLSearchParams(location.search).get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });

  view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) => runMutation(() => createLocation(parentId, name)),
    onRename: (id, name) => runMutation(() => renameLocation(id, name)),
    onMove: (id, newParentId) => runMutation(() => moveLocation(id, newParentId)),
  });

  addRootButton.addEventListener("click", showAddRootForm);

  await reload();
}

// showAddRootForm adds a root node, the one case the tree component cannot
// cover: its inline add-child editor opens underneath an existing node, and a
// first top-level location has none to open under. It is the same inline
// pattern rather than a prompt(), which browsers suppress in some contexts and
// which cannot be styled or reached the same way by assistive tech.
function showAddRootForm() {
  if (document.querySelector("#add-root-form")) return;

  const input = el("input", {
    type: "text",
    id: "add-root-name",
    "aria-label": "Name of the new top-level location",
    placeholder: "Kitchen",
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
        runMutation(() => createLocation(null, name));
      },
    },
    [
      input,
      el("button", { type: "submit", class: "btn" }, [text("Add")]),
      el(
        "button",
        { type: "button", class: "btn btn--ghost", onclick: () => form.remove() },
        [text("Cancel")],
      ),
    ],
  );

  treeContainer.before(form);
  input.focus();
}

function basePath() {
  return `/api/storages/${storageId}/locations`;
}

function createLocation(parentId, name) {
  return post(basePath(), { name, parent_id: parentId });
}

function renameLocation(id, name) {
  return patch(`${basePath()}/${id}`, { name });
}

function moveLocation(id, newParentId) {
  // parent_id is sent explicitly, including as null: the API separates "leave
  // the parent alone" (field absent) from "make this a root" (field null), and
  // dropping to the top of the tree is the second one.
  return patch(`${basePath()}/${id}`, { parent_id: newParentId });
}

// runMutation applies one change and then re-reads the tree.
//
// The server is what validates a move — same-storage membership and the cycle
// rule both live there — so the UI redraws from its answer rather than
// predicting the new shape and hoping. That also means a refused move visibly
// snaps back, which is the honest thing to show.
async function runMutation(mutate) {
  clearError();
  try {
    await mutate();
  } catch (err) {
    showError(err);
    // Re-read anyway: the local tree may already show an optimistic drag the
    // server just refused, and leaving that on screen would be a lie.
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
    view.render(body.items);
  } catch (err) {
    showError(err);
  }
}

function renderEmptyTree() {
  clearChildren(treeContainer);
  treeContainer.append(
    el("p", { class: "empty-state" }, [
      text("No locations yet. Add a top-level one to describe where things live — a room, a cupboard, a shelf."),
    ]),
  );
}

function showError(err) {
  // ApiError.message is the server's human-readable text: a cycle and a
  // location that still holds stock each say what actually went wrong.
  errorBox.textContent =
    err instanceof ApiError ? err.message : "Could not reach the server. Check your connection and try again.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
