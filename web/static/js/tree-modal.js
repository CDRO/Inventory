// The location or category tree editor, embedded as a modal escape hatch for
// any location or category field rendered by a review screen
// (docs/specs/26-location-quick-create.md, docs/specs/27-category-quick-create.md).
//
// This is a second embedding of components locations.html and categories.html
// already use — js/tree.js itself, and the same create/rename/move calls
// against GET/POST/PATCH /api/storages/{storage_id}/locations or
// /categories (docs/specs/06-vision-shelf-ingestion.md,
// docs/specs/08-expiration-and-classification.md) — never a new tree UI and
// never a second same-storage validation path. The server is still the only
// thing that checks a node belongs to storageId; this module only ever calls
// it with the storageId its caller supplied, and never navigates the page.
//
// One module, parametrized by kind, instead of two nearly-identical ones
// (docs/specs/27-category-quick-create.md: "generalizes 26's modal instead
// of duplicating it"). The categories kind's shelf-life-rule detail beside
// each node is the exact renderer categories.html uses, from
// js/category-shelf-life.js — not a stripped-down copy of it.

import { TreeView } from "./tree.js";
import { get, post, patch, ApiError } from "./api.js";
import { el, text, clearChildren } from "./dom.js";
import { createShelfLifeDetail, resolveInheritance } from "./category-shelf-life.js";
import { t, apiErrorMessage } from "./i18n.js";

// The locations kind's DOM ids keep their pre-rename "location-modal-*"
// spelling on purpose: e2e/specs/ingestion.spec.js and
// e2e/specs/shopping-list.spec.js already hardcode
// "#location-modal-add-root-form" for the location-quick-create tests this
// module's predecessor shipped with, and this generalization changes the
// module's shape, not its locations behavior or its tests
// (docs/specs/27-category-quick-create.md). The categories kind, being new,
// gets its own "category-modal-*" ids below rather than inheriting these.
const KINDS = {
  locations: {
    endpoint: "locations",
    get title() { return t("locations.title"); },
    titleId: "location-modal-title",
    addRootFormId: "location-modal-add-root-form",
    get hint() { return t("locations.hint"); },
    get addRootLabel() { return t("locations.addRoot"); },
    get rootNameLabel() { return t("locations.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("locations.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.locations.empty"); },
  },
  categories: {
    endpoint: "categories",
    get title() { return t("categories.title"); },
    titleId: "category-modal-title",
    addRootFormId: "category-modal-add-root-form",
    get hint() { return t("categories.hint"); },
    get addRootLabel() { return t("categories.addRoot"); },
    get rootNameLabel() { return t("categories.addRootNameLabel"); },
    get rootNamePlaceholder() { return t("categories.addRootNamePlaceholder"); },
    get emptyMessage() { return t("treeModal.categories.empty"); },
  },
};

/**
 * openTreeManager renders js/tree.js inside a native <dialog>, scoped to
 * storageId and to one kind of tree. Resolves once the dialog is dismissed —
 * "Done", Esc, or a backdrop click, all equivalent — with the ids of any
 * nodes created during that session, in creation order.
 *
 * Same contract as docs/specs/26-location-quick-create.md's
 * openLocationManager(storageId) used to have, parametrized by which tree
 * js/tree.js renders inside the dialog.
 *
 * @param {string} storageId
 * @param {{kind: "locations"|"categories"}} options
 * @returns {Promise<{createdIds: string[]}>}
 */
export function openTreeManager(storageId, { kind }) {
  const config = KINDS[kind];
  const createdIds = [];

  const errorBox = el("div", { class: "alert", role: "alert", hidden: true });
  const statusBox = el("p", { class: "empty-state", role: "status", hidden: true });
  const hint = el("p", { class: "empty-state" }, [text(config.hint)]);
  const addRootButton = el("button", { type: "button", class: "btn" }, [text(config.addRootLabel)]);
  const treeContainer = el("div");
  const doneButton = el("button", { type: "button", class: "btn btn--primary" }, [text(t("treeModal.done"))]);

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": config.titleId }, [
    el("h2", { id: config.titleId }, [text(config.title)]),
    errorBox,
    statusBox,
    el("div", { class: "row row--between" }, [hint, addRootButton]),
    treeContainer,
    el("div", { class: "row" }, [doneButton]),
  ]);

  function basePath() {
    return `/api/storages/${storageId}/${config.endpoint}`;
  }

  // inherited is only ever populated for the categories kind (see reload
  // below); the locations kind's renderDetail stays unset here, as it did
  // on this module's predecessor (js/location-modal.js) before this spec.
  // locations.html's own page-level TreeView does pass one — the audit
  // staleness detail (docs/specs/13-stocktake-and-audit.md) — but that was
  // never part of the modal's own contract (docs/specs/26-location-quick-create.md)
  // and adding it here is out of this spec's scope.
  let inherited = new Map();

  const view = new TreeView(treeContainer, {
    onAddChild: (parentId, name) => runMutation(() => createNode(parentId, name)),
    onRename: (id, name) => runMutation(() => patch(`${basePath()}/${id}`, { name })),
    onMove: (id, newParentId) => runMutation(() => patch(`${basePath()}/${id}`, { parent_id: newParentId })),
    renderDetail:
      kind === "categories"
        ? createShelfLifeDetail({ basePath, runMutation, showStatus, getInherited: () => inherited })
        : undefined,
  });

  function createNode(parentId, name) {
    return post(basePath(), { name, parent_id: parentId }).then((created) => {
      createdIds.push(created.id);
    });
  }

  // The server validates every mutation — same-storage membership, the cycle
  // rule on a move — so the dialog redraws from its answer rather than
  // predicting the new shape, exactly as locations.js and categories.js do.
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
        clearChildren(treeContainer);
        treeContainer.append(el("p", { class: "empty-state" }, [text(config.emptyMessage)]));
        return;
      }
      if (kind === "categories") inherited = resolveInheritance(body.items);
      view.render(body.items);
    } catch (err) {
      showError(err);
    }
  }

  function showError(err) {
    errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("treeModal.networkError");
    errorBox.hidden = false;
  }

  function showStatus(message) {
    statusBox.textContent = message;
    statusBox.hidden = false;
  }

  function clearMessages() {
    errorBox.textContent = "";
    errorBox.hidden = true;
    statusBox.textContent = "";
    statusBox.hidden = true;
  }

  // showAddRootForm is the one case the tree component cannot cover: its
  // inline add-child editor opens underneath an existing node, and a first
  // top-level node has none to open under. Same inline pattern locations.js
  // and categories.js use, for the same reason (no prompt(), styleable,
  // reachable by assistive tech).
  addRootButton.addEventListener("click", () => {
    if (dialog.querySelector(`#${config.addRootFormId}`)) return;

    const input = el("input", {
      type: "text",
      "aria-label": config.rootNameLabel,
      placeholder: config.rootNamePlaceholder,
      required: true,
    });

    const form = el(
      "form",
      {
        id: config.addRootFormId,
        class: "row",
        onsubmit: (event) => {
          event.preventDefault();
          const name = input.value.trim();
          if (!name) return;
          form.remove();
          runMutation(() => createNode(null, name));
        },
      },
      [
        input,
        el("button", { type: "submit", class: "btn" }, [text(t("tree.add"))]),
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => form.remove() }, [text(t("common.cancel"))]),
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
