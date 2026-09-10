// The shared proposal-review component used by shelf ingestion, shopping-list
// reconciliation, and consumption logging
// (docs/specs/05-frontend-pwa-foundations.md). All three follow the same
// shape — upload, job, AI proposal, editable list, explicit confirm — and a
// user who learns this screen once has learned all three.
//
// This module knows nothing about any one feature's confirm endpoint. It
// collects edits and decisions and hands the assembled payload to a callback
// the page supplies, which is what lets shelf ingestion, shopping lists, and
// consumption logging share one implementation despite posting to three
// different endpoints (docs/specs/06-vision-shelf-ingestion.md,
// 07-shopping-list-reconciliation.md, 09-consumption-logging.md).

import { fromTemplate, clearChildren } from "./dom.js";

// Row decisions, mirroring the confirm payload shape in
// docs/specs/09-consumption-logging.md: every row states exactly one of
// these. "Correct manually" is a richer editing flow (see markCorrected
// below), not a third wire value; once a correction is made the row is
// accepted with the corrected fields.
const DECISION_ACCEPT = "accept";
const DECISION_REJECT = "reject";

/**
 * ReviewList renders one job's proposed rows and tracks the three actions —
 * accept, correct, reject — until the caller confirms or discards.
 *
 * The row markup comes from a <template> in the page's own HTML, per the
 * spec: rows are cloned, not built from a string, so nothing here ever
 * constructs HTML out of AI- or user-supplied text. Bindable elements inside
 * the template are found by a data-field attribute; form controls
 * (input/select/textarea) bind their .value and stay editable, and anything
 * else binds via textContent. An img with data-role="thumb" gets its src set
 * to the item's crop or image URL — that assignment is a same-origin URL,
 * not text content, so it does not go through the injection rule the text
 * fields do.
 */
export class ReviewList {
  /**
   * @param {Element} container - where rows are rendered.
   * @param {HTMLTemplateElement} template - one row's markup; its single root
   *   element is cloned per row.
   */
  constructor(container, template) {
    this.container = container;
    this.template = template;
    /** @type {Map<string, {el: Element, item: Object, decision: string, corrected: boolean}>} */
    this.rows = new Map();
  }

  /**
   * setItems replaces the whole list with a fresh proposal — the shape of a
   * job's payload.items (docs/specs/04-backend-api-conventions.md).
   *
   * @param {Array<Object>} items - each must carry a unique row_id.
   */
  setItems(items) {
    clearChildren(this.container);
    this.rows.clear();
    for (const item of items) {
      this._addRow(item);
    }
  }

  _addRow(item) {
    const rowEl = fromTemplate(this.template);
    rowEl.dataset.rowId = item.row_id;
    bindFields(rowEl, item);

    const state = { el: rowEl, item, decision: DECISION_ACCEPT, corrected: false };
    this.rows.set(item.row_id, state);

    for (const button of rowEl.querySelectorAll("[data-action]")) {
      const action = button.getAttribute("data-action");
      button.addEventListener("click", () => this._handleAction(item.row_id, action));
    }

    this.container.append(rowEl);
  }

  _handleAction(rowId, action) {
    const state = this.rows.get(rowId);
    if (!state) return;

    switch (action) {
      case "accept":
        state.decision = DECISION_ACCEPT;
        state.el.classList.remove("review-row--rejected");
        break;
      case "reject":
        // Reversible: toggling again un-rejects. Rejected rows stay
        // rendered, struck through, and are simply omitted from the confirm
        // payload — never removed from the DOM
        // (docs/specs/05-frontend-pwa-foundations.md).
        if (state.decision === DECISION_REJECT) {
          state.decision = DECISION_ACCEPT;
          state.el.classList.remove("review-row--rejected");
        } else {
          state.decision = DECISION_REJECT;
          state.el.classList.add("review-row--rejected");
        }
        break;
      case "correct":
        this.onCorrect?.(rowId, state.el, state.item);
        break;
      default:
        // An unrecognised data-action is a template authoring mistake, not a
        // user error; fail loudly in development rather than doing nothing.
        console.warn(`review.js: unknown row action "${action}"`);
    }
  }

  /**
   * onCorrect is called when a row's "correct" action fires. Manual
   * correction is feature-specific — an autocomplete product picker in shelf
   * ingestion, an existing-product-only picker in consumption logging — so
   * this component does not implement it; it hands control to the page and
   * waits for markCorrected.
   *
   * @type {((rowId: string, rowEl: Element, item: Object) => void)|null}
   */
  onCorrect = null;

  /**
   * markCorrected is called by the page once its own correction UI has
   * updated the row's bound fields (directly, via the DOM elements it was
   * handed in onCorrect). The row is then accepted with the corrected
   * values — correction is a richer edit, not a fourth decision.
   *
   * @param {string} rowId
   */
  markCorrected(rowId) {
    const state = this.rows.get(rowId);
    if (!state) return;
    state.decision = DECISION_ACCEPT;
    state.corrected = true;
    state.el.classList.remove("review-row--rejected");
  }

  /**
   * getPayload assembles the confirm-endpoint body: one entry per row, each
   * carrying row_id and an explicit decision
   * (docs/specs/09-consumption-logging.md) — a missing row is a validation
   * error server-side, never an implicit rejection, which is exactly why
   * every row is included here regardless of its decision.
   *
   * @returns {Array<{row_id: string, decision: string, corrected?: boolean}>}
   */
  getPayload() {
    return Array.from(this.rows.values()).map((state) => {
      if (state.decision === DECISION_REJECT) {
        return { row_id: state.item.row_id, decision: DECISION_REJECT };
      }
      return {
        row_id: state.item.row_id,
        decision: DECISION_ACCEPT,
        ...readFields(state.el),
        ...(state.corrected ? { corrected: true } : {}),
      };
    });
  }
}

/**
 * bindFields populates a cloned row's data-field elements from item, and
 * wires form controls to update item live as the user edits — so getPayload
 * above can read the current DOM state directly rather than tracking a
 * parallel copy that could drift from what is on screen.
 */
function bindFields(rowEl, item) {
  for (const field of rowEl.querySelectorAll("[data-field]")) {
    const key = field.getAttribute("data-field");
    const value = item[key];

    if (isFormControl(field)) {
      if (value != null) field.value = value;
      field.addEventListener("input", () => {
        item[key] = field.value;
      });
    } else {
      // textContent only: this value may have come from a shelf photo the
      // vision model read, or from the anonymous catalog — untrusted text
      // either way, and never rendered as markup
      // (docs/specs/02-data-model.md, 04-backend-api-conventions.md).
      field.textContent = value == null ? "" : String(value);
    }
  }

  const thumb = rowEl.querySelector('[data-role="thumb"]');
  if (thumb) {
    const src = item.crop_url || item.image_url;
    if (src) thumb.src = src;
  }
}

function isFormControl(node) {
  return (
    node instanceof HTMLInputElement ||
    node instanceof HTMLSelectElement ||
    node instanceof HTMLTextAreaElement
  );
}

/** readFields collects the current value of every data-field form control. */
function readFields(rowEl) {
  const out = {};
  for (const field of rowEl.querySelectorAll("[data-field]")) {
    if (isFormControl(field)) {
      out[field.getAttribute("data-field")] = field.value;
    }
  }
  return out;
}
