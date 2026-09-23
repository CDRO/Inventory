// The location tree editor, embedded as a modal escape hatch for any location
// field rendered by a review screen (docs/specs/26-location-quick-create.md).
//
// This is a second embedding of components locations.html already uses —
// js/tree.js itself, and the same create/rename/move calls against
// GET/POST/PATCH /api/storages/{storage_id}/locations
// (docs/specs/06-vision-shelf-ingestion.md) — never a new tree UI and never a
// second same-storage validation path. The server is still the only thing
// that checks a location belongs to storageId; this module only ever calls it
// with the storageId its caller supplied, and never navigates the page.

import { TreeView } from "./tree.js";
import { get, post, patch, ApiError } from "./api.js";
import { el, text, clearChildren } from "./dom.js";

/**
 * openLocationManager renders the location tree inside a native <dialog>,
 * scoped to storageId. Resolves once the dialog is dismissed — "Done", Esc,
 * or a backdrop click, all equivalent — with the ids of any locations created
 * during that session, in creation order.
 *
 * @param {string} storageId
 * @returns {Promise<{createdIds: string[]}>}
 */
export function openLocationManager(storageId) {
  const createdIds = [];

  const errorBox = el("div", { class: "alert", role: "alert", hidden: true });
  const hint = el("p", { class: "empty-state" }, [
    text("Drag a location onto another to move it, or use its “Move to…” button."),
  ]);
  const addRootButton = el("button", { type: "button", class: "btn" }, [text("Add top-level location")]);
  const treeContainer = el("div");
  const doneButton = el("button", { type: "button", class: "btn btn--primary" }, [text("Done")]);

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": "location-modal-title" }, [
    el("h2", { id: "location-modal-title" }, [text("Locations")]),
    errorBox,
    el("div", { class: "row row--between" }, [hint, addRootButton]),
    treeContainer,
    el("div", { class: "row" }, [doneButton]),
  ]);

  function basePath() {
    return `/api/storages/${storageId}/locations`;
  }

  const view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) => runMutation(() => createLocation(parentId, name)),
    onRename: (id, name) => runMutation(() => renameLocation(id, name)),
    onMove: (id, newParentId) => runMutation(() => moveLocation(id, newParentId)),
  });

  function createLocation(parentId, name) {
    return post(basePath(), { name, parent_id: parentId }).then((created) => {
      createdIds.push(created.id);
    });
  }

  function renameLocation(id, name) {
    return patch(`${basePath()}/${id}`, { name });
  }

  function moveLocation(id, newParentId) {
    return patch(`${basePath()}/${id}`, { parent_id: newParentId });
  }

  // The server validates every mutation — same-storage membership, the cycle
  // rule on a move — so the dialog redraws from its answer rather than
  // predicting the new shape, exactly as locations.js does.
  async function runMutation(mutate) {
    clearError();
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
        clearChildren(treeContainer);
        treeContainer.append(el("p", { class: "empty-state" }, [text("No locations yet. Add a top-level one below.")]));
        return;
      }
      view.render(body.items);
    } catch (err) {
      showError(err);
    }
  }

  function showError(err) {
    errorBox.textContent =
      err instanceof ApiError ? err.message : "Could not reach the server. Check your connection and try again.";
    errorBox.hidden = false;
  }

  function clearError() {
    errorBox.textContent = "";
    errorBox.hidden = true;
  }

  // showAddRootForm is the one case the tree component cannot cover: its
  // inline add-child editor opens underneath an existing node, and a first
  // top-level location has none to open under. Same inline pattern
  // locations.js uses, for the same reason (no prompt(), styleable, reachable
  // by assistive tech).
  addRootButton.addEventListener("click", () => {
    if (dialog.querySelector("#location-modal-add-root-form")) return;

    const input = el("input", {
      type: "text",
      "aria-label": "Name of the new top-level location",
      placeholder: "Kitchen",
      required: true,
    });

    const form = el(
      "form",
      {
        id: "location-modal-add-root-form",
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
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text("Cancel")]),
      ],
    );

    treeContainer.before(form);
    input.focus();
  });

  return new Promise((resolve) => {
    doneButton.addEventListener("click", () => dialog.close());

    // A click that lands on the dialog element itself, rather than on any of
    // its children, is a click on the backdrop: the dialog's box only covers
    // its rendered content, so nothing else inside it can be the target of
    // such a click. Esc is handled by the browser already — it fires the
    // dialog's own "cancel" then "close" — so both dismissals converge here.
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog) dialog.close();
    });

    dialog.addEventListener("close", () => {
      dialog.remove();
      resolve({ createdIds: [...createdIds] });
    });

    document.body.append(dialog);
    dialog.showModal();
    reload();
  });
}
