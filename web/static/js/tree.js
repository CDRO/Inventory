// The shared tree view used for both locations and categories — identical
// shape per docs/specs/02-data-model.md
// (docs/specs/05-frontend-pwa-foundations.md): expand/collapse, inline
// add-child, rename, a "move to…" picker, and required drag-and-drop
// re-parenting via native HTML5 drag events
// (docs/specs/06-vision-shelf-ingestion.md).
//
// Every node this module renders comes from the server — a name typed by a
// household member, stored as-is. Every piece of text goes through
// textContent, never through a template string turned into markup.

import { el, text, clearChildren } from "./dom.js";

/**
 * TreeNode is the shape both `GET /api/storages/{id}/locations` and the
 * equivalent categories endpoint return
 * (docs/specs/06-vision-shelf-ingestion.md): a nested tree, each node's
 * children already resolved.
 *
 * @typedef {Object} TreeNode
 * @property {string} id
 * @property {string} name
 * @property {string} [description]
 * @property {TreeNode[]} children
 */

/**
 * TreeView renders a tree and wires expand/collapse, inline rename,
 * inline add-child, and re-parenting (drag-and-drop, plus a dialog-based
 * picker as its keyboard-reachable equivalent — required, since
 * drag-and-drop alone is neither accessible nor reliable on touch).
 *
 * This component never calls the network itself. Every mutation goes through
 * the callbacks supplied to the constructor, so the same class serves the
 * locations tree and the categories tree, which differ only in which
 * endpoint their callbacks post to.
 */
export class TreeView {
  /**
   * @param {Element} container - a <ul> or <div> to render the tree into.
   * @param {Object} callbacks
   * @param {(parentId: string|null, name: string) => void} callbacks.onAddChild
   * @param {(id: string, name: string) => void} callbacks.onRename
   * @param {(id: string, newParentId: string|null) => void} callbacks.onMove -
   *   called for both drag-and-drop and the picker dialog; the caller is
   *   expected to re-render on the server's response rather than this module
   *   predicting the new shape, since the server is the one that validates
   *   same-storage membership and rejects a cycle
   *   (docs/specs/02-data-model.md).
   */
  constructor(container, { onAddChild, onRename, onMove }) {
    this.container = container;
    this.onAddChild = onAddChild;
    this.onRename = onRename;
    this.onMove = onMove;
    /** @type {TreeNode[]} */
    this.nodes = [];
    /** @type {Set<string>} ids of nodes currently expanded. */
    this.expanded = new Set();
  }

  /**
   * render draws the whole tree from scratch. Called with the server's
   * response after every mutation, rather than this module guessing the new
   * shape locally — the server is the source of truth for what is now valid.
   *
   * @param {TreeNode[]} nodes - root-level nodes.
   */
  render(nodes) {
    this.nodes = nodes;
    clearChildren(this.container);
    this.container.classList.add("tree");
    this.container.append(...nodes.map((node) => this._renderNode(node, 0)));
  }

  _renderNode(node, depth) {
    const hasChildren = node.children && node.children.length > 0;
    const isExpanded = this.expanded.has(node.id);

    const toggle = el(
      "button",
      {
        class: "tree-toggle",
        type: "button",
        "aria-expanded": hasChildren ? String(isExpanded) : undefined,
        title: hasChildren ? "Expand or collapse" : undefined,
        onclick: hasChildren ? () => this._toggleExpanded(node.id) : undefined,
      },
      [text(!hasChildren ? "" : isExpanded ? "▾" : "▸")],
    );

    const nameSpan = el("span", { class: "tree-name", "data-field": "name" }, [text(node.name)]);

    const nodeEl = el(
      "div",
      {
        class: "tree-node",
        draggable: "true",
        "data-id": node.id,
      },
      [
        toggle,
        nameSpan,
        el("div", { class: "row" }, [
          el("button", { type: "button", class: "btn btn--ghost", onclick: () => this._startRename(node, nameSpan) }, [
            text("Rename"),
          ]),
          el("button", { type: "button", class: "btn btn--ghost", onclick: () => this._startAddChild(node.id, li) }, [
            text("Add"),
          ]),
          el("button", { type: "button", class: "btn btn--ghost", onclick: () => this._openMovePicker(node.id) }, [
            text("Move to…"),
          ]),
        ]),
      ],
    );

    this._wireDragAndDrop(nodeEl, node.id);

    const li = el("li", {}, [nodeEl]);

    if (hasChildren) {
      const childList = el("ul", { hidden: !isExpanded }, node.children.map((child) => this._renderNode(child, depth + 1)));
      li.append(childList);
    }

    return li;
  }

  _toggleExpanded(id) {
    if (this.expanded.has(id)) {
      this.expanded.delete(id);
    } else {
      this.expanded.add(id);
    }
    this.render(this.nodes);
  }

  _startRename(node, nameSpan) {
    const input = el("input", { type: "text", value: node.name, class: "tree-rename-input" });
    nameSpan.replaceWith(input);
    input.focus();
    input.select();

    const commit = () => {
      const value = input.value.trim();
      if (value && value !== node.name) {
        this.onRename(node.id, value);
      } else {
        input.replaceWith(nameSpan);
      }
    };
    input.addEventListener("blur", commit);
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") input.blur();
      if (event.key === "Escape") {
        input.value = node.name;
        input.blur();
      }
    });
  }

  _startAddChild(parentId, afterLi) {
    const input = el("input", { type: "text", placeholder: "New name" });
    const confirmBtn = el("button", { type: "button", class: "btn btn--primary" }, [text("Add")]);
    const cancelBtn = el("button", { type: "button", class: "btn btn--ghost" }, [text("Cancel")]);
    const row = el("li", { class: "row" }, [input, confirmBtn, cancelBtn]);

    afterLi.after(row);
    input.focus();

    const submit = () => {
      const value = input.value.trim();
      if (value) this.onAddChild(parentId, value);
      row.remove();
    };
    confirmBtn.addEventListener("click", submit);
    cancelBtn.addEventListener("click", () => row.remove());
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") submit();
      if (event.key === "Escape") row.remove();
    });
  }

  /**
   * _openMovePicker is the keyboard- and touch-reachable equivalent of
   * dragging a node onto a new parent, required alongside drag-and-drop by
   * docs/specs/06-vision-shelf-ingestion.md. It lists every node that would
   * not create a cycle — the node itself and its own descendants are
   * excluded — plus a "No parent (root)" option.
   */
  _openMovePicker(nodeId) {
    const options = this._flatten().filter((entry) => !this._isSelfOrDescendant(nodeId, entry.id));

    const select = el(
      "select",
      {},
      [
        el("option", { value: "" }, [text("No parent (root)")]),
        ...options.map((entry) => el("option", { value: entry.id }, [text("  ".repeat(entry.depth) + entry.name)])),
      ],
    );

    const dialog = el("dialog", { class: "card" }, [
      el("h2", {}, [text("Move to…")]),
      select,
      el("div", { class: "row", style: "margin-top: 1rem" }, [
        el("button", {
          type: "button",
          class: "btn btn--primary",
          onclick: () => {
            this.onMove(nodeId, select.value || null);
            dialog.close();
          },
        }, [text("Move")]),
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => dialog.close() }, [text("Cancel")]),
      ]),
    ]);

    dialog.addEventListener("close", () => dialog.remove());
    document.body.append(dialog);
    dialog.showModal();
  }

  /** _flatten returns every node as {id, name, depth}, in tree order. */
  _flatten() {
    const out = [];
    const walk = (nodes, depth) => {
      for (const node of nodes) {
        out.push({ id: node.id, name: node.name, depth });
        if (node.children) walk(node.children, depth + 1);
      }
    };
    walk(this.nodes, 0);
    return out;
  }

  /**
   * _isSelfOrDescendant reports whether `candidateId` is `nodeId` itself or
   * appears under it, for the client-side cycle guard: the picker and the
   * drag target both need to refuse these before the server ever sees the
   * request, so the invalid state is visible immediately rather than after a
   * round trip (docs/specs/06-vision-shelf-ingestion.md). The server's own
   * check in internal/store/tree.go is still authoritative; this is a UX
   * courtesy, not the enforcement.
   */
  _isSelfOrDescendant(nodeId, candidateId) {
    const root = this._findNode(this.nodes, nodeId);
    if (!root) return false;
    if (root.id === candidateId) return true;
    const walk = (nodes) => nodes.some((n) => n.id === candidateId || (n.children && walk(n.children)));
    return Boolean(root.children && walk(root.children));
  }

  _findNode(nodes, id) {
    for (const node of nodes) {
      if (node.id === id) return node;
      if (node.children) {
        const found = this._findNode(node.children, id);
        if (found) return found;
      }
    }
    return null;
  }

  _wireDragAndDrop(nodeEl, nodeId) {
    nodeEl.addEventListener("dragstart", (event) => {
      // dataTransfer.getData is unreadable during dragover in every major
      // browser — only dragstart and drop expose it, for cross-origin drag
      // security reasons that do not apply here. The dragover handler below
      // needs to know which node is being dragged in order to show the
      // invalid-drop state immediately, so that id is tracked on the
      // instance instead; setData is still called too, since drop still
      // needs a value that survives even if this instance were somehow torn
      // down mid-drag.
      this._draggedId = nodeId;
      event.dataTransfer.setData("text/plain", nodeId);
      event.dataTransfer.effectAllowed = "move";
    });

    nodeEl.addEventListener("dragover", (event) => {
      const draggedId = this._draggedId;
      const invalid = !draggedId || draggedId === nodeId || this._isSelfOrDescendant(draggedId, nodeId);
      if (!invalid) {
        // Only a valid target gets preventDefault: that is what tells the
        // browser a drop is allowed here at all, so an invalid target simply
        // never becomes droppable rather than needing a second check on drop.
        event.preventDefault();
        nodeEl.classList.add("tree-node--drop-target");
        nodeEl.classList.remove("tree-node--drop-invalid");
      } else {
        nodeEl.classList.add("tree-node--drop-invalid");
        nodeEl.classList.remove("tree-node--drop-target");
      }
    });

    nodeEl.addEventListener("dragleave", () => {
      nodeEl.classList.remove("tree-node--drop-target", "tree-node--drop-invalid");
    });

    nodeEl.addEventListener("drop", (event) => {
      event.preventDefault();
      nodeEl.classList.remove("tree-node--drop-target", "tree-node--drop-invalid");
      const draggedId = event.dataTransfer.getData("text/plain") || this._draggedId;
      if (!draggedId || draggedId === nodeId) return;
      if (this._isSelfOrDescendant(draggedId, nodeId)) return; // client-side guard only; the server re-checks
      this.onMove(draggedId, nodeId);
    });

    nodeEl.addEventListener("dragend", () => {
      this._draggedId = null;
      nodeEl.classList.remove("tree-node--drop-target", "tree-node--drop-invalid");
    });
  }
}
