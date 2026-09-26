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
 * that may already exist. `keyPrefix` names that section; the six leaf keys
 * below must exist under it in every catalog (docs/specs/19-localization.md).
 *
 * @param {Element} container - emptied first; the picker is rendered into it.
 * @param {Object} options
 * @param {string} options.storageId - the storage whose suggestions to ask for.
 * @param {string} options.query - what to find pictures of.
 * @param {string} options.keyPrefix - i18n section, e.g. "shoppingList.pictures".
 * @param {(hash: string|null) => void} options.onPick - called on every click,
 *   with the picked suggestion's cache hash, or null for the "no picture"
 *   choice. Never called during rendering: a caller that writes on pick must
 *   not be made to write merely because the picker opened.
 */
export async function renderImagePicker(container, { storageId, query, keyPrefix, onPick }) {
  clearChildren(container);

  const status = el("span", { class: "empty-state" }, [text(t(`${keyPrefix}.loading`))]);
  container.append(status);

  let suggestions = [];
  try {
    const body = await get(
      `/api/storages/${storageId}/image-suggestions?query=${encodeURIComponent(query)}`,
    );
    suggestions = body.suggestions || [];
  } catch {
    status.textContent = t(`${keyPrefix}.unavailable`);
    return;
  }

  if (suggestions.length === 0) {
    status.textContent = t(`${keyPrefix}.none`);
    return;
  }

  status.textContent = t(`${keyPrefix}.pick`);
  const row = el("div", { class: "row" });
  const none = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost btn--selected",
      "aria-pressed": "true",
      onclick: (event) => {
        markChosen(row, event.currentTarget);
        onPick(null);
      },
    },
    [text(t(`${keyPrefix}.noPicture`))],
  );
  row.append(none);

  for (const suggestion of suggestions) {
    const hash = suggestion.url.split("/").pop();
    row.append(
      el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "aria-pressed": "false",
          "aria-label": t(`${keyPrefix}.useThis`, { type: suggestion.type }),
          onclick: (event) => {
            markChosen(row, event.currentTarget);
            onPick(hash);
          },
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
  for (const other of container.querySelectorAll("button")) {
    other.classList.remove("btn--selected");
    other.setAttribute("aria-pressed", "false");
  }
  button.classList.add("btn--selected");
  button.setAttribute("aria-pressed", "true");
}
