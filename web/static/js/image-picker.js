// The shared picture-suggestion picker: asks the server for pictures matching
// a query and lets someone choose one
// (docs/specs/07-shopping-list-reconciliation.md).
//
// It was lifted out of js/pages/shopping-list.js when the product detail view
// of docs/specs/16-product-maintenance.md needed the same two change paths
// spec 07 describes — "the change paths from 07 — suggestion picker, custom
// upload" — and the picker existed in exactly one page module.
//
// Every URL this renders is on this origin. The suggestions endpoint fetches
// from Iconify and SerpAPI server-side and re-serves the bytes from its own
// cache, so no <img> here ever points at a provider: an external src would
// leak the viewer's IP and user-agent to that host, and would not load at all
// on a LAN-only NAS.
//
// A picked picture travels back as the hash of its cache entry, never as a
// URL. The server promotes that hash into permanent storage. A URL would let
// a caller point a household's product at a server of their choosing, and a
// suggestion-cache URL recorded on a product could be evicted from under it
// (internal/httpapi/products.go, SetImage).

import { el, text, clearChildren } from "./dom.js";
import { get } from "./api.js";
import { t } from "./i18n.js";

// Per-invocation guard state, keyed by the container a caller passes in — not
// by anything module-level. A caller like js/pages/products.js's renderPicture
// builds one `pickerBox` per product and reuses it for every "change picture"
// click, so the container is what makes two reopens of the *same* logical
// picker instance identifiable, without a module-global that would leak
// across every product ever opened in the session (the exact bug class this
// guards against — see renderPictureUpload's own guard for the same reasoning
// on the upload control beside this one). A container that is discarded (the
// caller moved on, e.g. a different product) drops out of the WeakMap on its
// own, so nothing here has to clean up after a picker that never reopens.
const pickerState = new WeakMap();

function stateFor(container) {
  let state = pickerState.get(container);
  if (!state) {
    state = { settling: false, run: 0 };
    pickerState.set(container, state);
  }
  return state;
}

/**
 * renderImagePicker fills `container` with the storage's picture suggestions
 * for `query` and reports each choice back through `onPick`.
 *
 * The three terminal states a caller never has to render itself — the
 * provider being unreachable, no suggestions coming back, and a live list —
 * all end with the status line replaced rather than an error thrown: a
 * provider being down must not block the flow this picker sits in, which is
 * spec 07's own degradation rule carried through to the UI.
 *
 * Labels come from the caller's own catalog section rather than a shared one,
 * because the surrounding sentence differs per page: the shopping list offers
 * to "continue without one", while the product screen is changing a picture
 * that may already exist. `keyPrefix` names that section; the leaf keys used
 * below must exist under it in every catalog (docs/specs/19-localization.md).
 *
 * @param {Element} container - emptied first; the picker is rendered into it.
 *   Its identity is the guard's key (see `pickerState` above): pass the same
 *   element on every reopen of one logical picker so the guard survives
 *   across them, and a different element per logical picker (a different
 *   product, a different shopping-list row) so their guards never collide.
 * @param {Object} options
 * @param {string} options.storageId - the storage whose suggestions to ask for.
 * @param {string} options.query - what to find pictures of.
 * @param {string} options.keyPrefix - i18n section, e.g. "shoppingList.pictures".
 * @param {boolean} [options.clearable] - true when the thing being changed may
 *   already have a picture, so removing it is one of the choices. See the
 *   clear button below: it is what decides that the choice survives a
 *   provider outage, and it uses `<keyPrefix>.removePicture` instead of
 *   `<keyPrefix>.noPicture`.
 * @param {(hash: string|null) => (void|Promise<void>)} options.onPick - called
 *   on every click, with the picked suggestion's cache hash, or null for the
 *   clear choice. Never called during rendering: a caller that writes on pick
 *   must not be made to write merely because the picker opened. If it returns
 *   a promise that rejects, the selection is rolled back to what it was.
 */
export async function renderImagePicker(
  container,
  { storageId, query, keyPrefix, onPick, clearable = false },
) {
  const state = stateFor(container);
  const myRun = ++state.run;

  clearChildren(container);

  const status = el("span", { class: "empty-state" }, [text(t(`${keyPrefix}.loading`))]);
  container.append(status);

  const row = el("div", { class: "row" });

  // Writing is asynchronous for at least one caller, and two things follow
  // from that. A second click while a write is still in flight would race two
  // writes whose order the server decides, so it is refused; and a write that
  // fails must not leave a button claiming to be the current choice, so the
  // selection rolls back to whatever it was.
  //
  // `state.settling` lives on the container-keyed state above, not in this
  // call's own closure, so it stays true across a close-and-reopen of the
  // same logical picker while its write is still outstanding (#249) — a
  // second suggestion clicked in the reopened picker is refused exactly like
  // a double click within one open picker always was.
  async function choose(button, value) {
    if (state.settling) return;
    state.settling = true;
    const previous = row.querySelector(".btn--selected");
    markChosen(row, button);
    try {
      await onPick(value);
    } catch {
      // The caller reports the failure in its own words; this only has to
      // stop the selection from contradicting it. `row` is this invocation's
      // own — if a newer invocation has since cleared the container, `row` is
      // detached and this is a no-op rather than a visible glitch.
      if (previous) markChosen(row, previous);
      else unmarkAll(row);
    } finally {
      state.settling = false;
    }
  }

  // The clear choice. For a caller changing a picture that may already exist
  // it is the only way to remove one, so it is rendered in every state below
  // — including the two where no suggestion ever arrives, which is the
  // ordinary case whenever no image provider is configured. For a caller
  // choosing a picture for something that has none yet, it is instead the
  // pre-selected default and only means anything beside real suggestions.
  const clearButton = el(
    "button",
    {
      type: "button",
      class: clearable ? "btn btn--ghost" : "btn btn--ghost btn--selected",
      "aria-pressed": clearable ? "false" : "true",
      "data-role": "picture-none",
      onclick: (event) => choose(event.currentTarget, null),
    },
    [text(t(clearable ? `${keyPrefix}.removePicture` : `${keyPrefix}.noPicture`))],
  );

  function degradeTo(message) {
    status.textContent = message;
    if (clearable) {
      row.append(clearButton);
      container.append(row);
    }
  }

  let suggestions = [];
  try {
    const body = await get(
      `/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`,
    );
    // A newer invocation of this same container has since cleared it and
    // started its own fetch — this one no longer owns `container`. Returning
    // here rather than falling through is what stops #252: without it, this
    // stale invocation would still append its own `row` to the live
    // container below, landing beside the newer invocation's row.
    if (myRun !== state.run) return;
    suggestions = body.suggestions || [];
  } catch {
    if (myRun !== state.run) return;
    degradeTo(t(`${keyPrefix}.unavailable`));
    return;
  }

  if (suggestions.length === 0) {
    degradeTo(t(`${keyPrefix}.none`));
    return;
  }

  status.textContent = t(`${keyPrefix}.pick`);
  row.append(clearButton);

  for (const suggestion of suggestions) {
    const hash = suggestion.url.split("/").pop();
    row.append(
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "aria-pressed": "false",
          "data-role": "picture-suggestion",
          "aria-label": t(`${keyPrefix}.useThis`, { type: suggestion.type }),
          onclick: (event) => choose(event.currentTarget, hash),
        },
        [el("img", { src: suggestion.url, alt: "", width: 72, height: 72, loading: "lazy" })],
      ),
    );
  }
  container.append(row);
}

/**
 * markChosen makes one button in a group the selected one, in both the class
 * that styles it and the aria-pressed a screen reader announces — the two must
 * move together or the visual and the announced selection drift apart.
 */
export function markChosen(container, button) {
  unmarkAll(container);
  button.classList.add("btn--selected");
  button.setAttribute("aria-pressed", "true");
}

function unmarkAll(container) {
  for (const other of container.querySelectorAll("button")) {
    other.classList.remove("btn--selected");
    other.setAttribute("aria-pressed", "false");
  }
}
