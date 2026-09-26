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
  clearChildren(container);

  const status = el("span", { class: "empty-state" }, [text(t(`${keyPrefix}.loading`))]);
  container.append(status);

  const row = el("div", { class: "row" });

  // Writing is asynchronous for at least one caller, and two things follow
  // from that. A second click while the first write is still in flight would
  // race two writes whose order the server decides, so it is refused; and a
  // write that fails must not leave a button claiming to be the current
  // choice, so the selection rolls back to whatever it was.
  let settling = false;
  async function choose(button, value) {
    if (settling) return;
    settling = true;
    const previous = row.querySelector(".btn--selected");
    markChosen(row, button);
    try {
      await onPick(value);
    } catch {
      // The caller reports the failure in its own words; this only has to
      // stop the selection from contradicting it.
      if (previous) markChosen(row, previous);
      else unmarkAll(row);
    } finally {
      settling = false;
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
    suggestions = body.suggestions || [];
  } catch {
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
