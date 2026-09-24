// Shared shelf-life-rule rendering and editing for the category tree
// (docs/specs/08-expiration-and-classification.md): the label and inline
// editor js/tree.js's renderDetail hook draws beside each category node.
//
// Factored out so categories.html's page module and the categories kind of
// js/tree-modal.js (docs/specs/27-category-quick-create.md) render and save
// it through exactly one implementation, never a stripped-down copy kept in
// sync by hand.

import { el, text, clearChildren } from "./dom.js";
import { patch } from "./api.js";
import { t, tCount } from "./i18n.js";

// The server's bound (maxShelfLifeDays in internal/httpapi/expiry.go). The
// input carries it so a browser flags an out-of-range number before a round
// trip; the server still decides.
export const MAX_SHELF_LIFE_DAYS = 36500;

/**
 * resolveInheritance walks a category tree once, carrying the nearest rule
 * set above each node, so every node's label can name the rule it falls
 * back on without re-walking the tree per node.
 *
 * @param {Array} roots
 * @returns {Map<string, {days: number, from: string}|null>}
 */
export function resolveInheritance(roots) {
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

function days(n) {
  return tCount("categoryShelfLife.days", n);
}

function shelfLifeLabel(node, inherited) {
  if (node.default_shelf_life_days != null) {
    return days(node.default_shelf_life_days);
  }
  const rule = inherited.get(node.id);
  if (rule) {
    return t("categoryShelfLife.inheritedFrom", { days: days(rule.days), from: rule.from });
  }
  // Nothing above sets a rule: the product's item type decides
  // (docs/specs/08-expiration-and-classification.md, step 5).
  return t("categoryShelfLife.byItemType");
}

/**
 * createShelfLifeDetail returns a renderDetail callback for js/tree.js's
 * TreeView, scoped to one caller's own basePath, mutate-then-reload and
 * inherited-rule lookup — so categories.html and the tree-modal's categories
 * kind each save through their own route and redraw from their own reload,
 * while sharing every line of the label and editor themselves.
 *
 * @param {Object} args
 * @param {() => string} args.basePath - e.g. () => `/api/storages/${id}/categories`.
 * @param {(mutate: () => Promise<void>) => Promise<void>} args.runMutation -
 *   applies one change and reloads the tree from the server's answer,
 *   exactly as every other tree mutation does.
 * @param {(message: string) => void} args.showStatus - shown once the save
 *   succeeds, naming how many existing expiry dates moved.
 * @param {() => Map<string, {days: number, from: string}|null>} args.getInherited
 * @returns {(node: import("./tree.js").TreeNode & {default_shelf_life_days: number|null}) => Node}
 */
export function createShelfLifeDetail({ basePath, runMutation, showStatus, getInherited }) {
  function showLabel(node, wrapper) {
    clearChildren(wrapper);
    wrapper.append(
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost tree-shelf-life",
          title: t("categoryShelfLife.editTitle", { name: node.name }),
          onclick: () => openShelfLifeEditor(node, wrapper),
        },
        [text(shelfLifeLabel(node, getInherited()))],
      ),
    );
  }

  function openShelfLifeEditor(node, wrapper) {
    const input = el("input", {
      type: "number",
      min: "0",
      max: String(MAX_SHELF_LIFE_DAYS),
      step: "1",
      inputmode: "numeric",
      class: "tree-shelf-life-input",
      "aria-label": t("categoryShelfLife.inputAriaLabel", { name: node.name }),
      placeholder: t("categoryShelfLife.inheritPlaceholder"),
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
        el("button", { type: "submit", class: "btn btn--primary" }, [text(t("common.save"))]),
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => showLabel(node, wrapper) }, [
          text(t("common.cancel")),
        ]),
      ],
    );

    clearChildren(wrapper);
    wrapper.append(form);
    input.focus();
  }

  function saveShelfLife(node, value) {
    return patch(`${basePath()}/${node.id}/shelf-life`, { default_shelf_life_days: value }).then((body) => {
      // The count is the point of this route
      // (docs/specs/08-expiration-and-classification.md): a person changing a
      // rule is doing it to change dates, so say how many actually moved.
      const moved = body.recomputed_batches;
      showStatus(
        moved === 0
          ? t("categoryShelfLife.savedNoChange", { name: node.name })
          : tCount("categoryShelfLife.savedRecalculated", moved, { name: node.name }),
      );
    });
  }

  return function renderShelfLife(node) {
    const wrapper = el("span", { class: "tree-detail" });
    showLabel(node, wrapper);
    return wrapper;
  };
}
