// The icon picker: a local, alias-backed icon search
// (docs/specs/40-icon-picker.md), sibling to js/image-picker.js and following
// its established shape — a search input, a result grid of clickable icon
// buttons, loading/empty states.
//
// Every result comes from this server's own database (icon_aliases, icons —
// docs/specs/42-local-icon-library.md), so unlike image-picker.js there is no
// "provider unavailable" state to design for: the only failure this search
// can suffer is this server itself being unreachable, and a no-results
// answer is a plain empty state, not a degraded one.
//
// A vendored icon's SVG body is trusted content (this project's
// trusted-external-provider posture, docs/specs/42-local-icon-library.md) and
// is rendered by parsing it and inserting the resulting node — never through
// innerHTML (js/dom.js's rule) — so it is drawn as a real vector graphic, not
// read out of a plain-text identifier. An uploaded icon is person-supplied
// content instead, so it is rendered through an <img> pointed at the
// server's own ServeSVG route, the same rendering discipline that route's own
// comment documents (internal/httpapi/icons.go): never injected inline.

import { el, text, clearChildren } from "./dom.js";
import { get, post } from "./api.js";
import { t } from "./i18n.js";

// Mirrors js/pages/products.js's own SEARCH_DEBOUNCE_MS — this is the one
// picker that owns a live search input rather than taking a fixed query, so
// it needs its own debounce.
const SEARCH_DEBOUNCE_MS = 250;

const ICON_SIZE = 40;

// Per-container guard state, the same shape and the same reasoning as
// image-picker.js's pickerState: `settling` refuses a second pick while one
// is still writing, and `run` lets a stale search response notice a newer
// invocation has since taken over the container and stand down instead of
// rendering into it.
const pickerState = new WeakMap();

function stateFor(container) {
  let state = pickerState.get(container);
  if (!state) {
    state = { settling: false, run: 0, searchRun: 0 };
    pickerState.set(container, state);
  }
  return state;
}

/**
 * renderIconPicker fills `container` with a search box and this storage's
 * icon search results, and reports each choice back through `onPick`.
 *
 * @param {Element} container - emptied first; the picker is rendered into it.
 *   Pass the same element on every reopen of one logical picker, exactly as
 *   image-picker.js's `container` parameter documents.
 * @param {Object} options
 * @param {string} options.storageId - the storage the icon-suggestions route
 *   is scoped under (the underlying data is global, but the route is not).
 * @param {string|null} [options.currentIconName] - the product's current
 *   icon_name, if any. Looked up once, at open time, so the picker opens
 *   showing it highlighted (docs/specs/40-icon-picker.md) — the search box
 *   itself stays empty and focused, ready for a fresh search.
 * @param {string} options.keyPrefix - i18n section, e.g. "products.icons".
 * @param {(iconName: string|null) => (void|Promise<void>)} options.onPick -
 *   called on every click with the picked icon_name, or null for "clear
 *   icon". Never called during rendering. If it returns a rejected promise,
 *   the selection rolls back to what it was.
 */
export async function renderIconPicker(container, { storageId, currentIconName = null, keyPrefix, onPick }) {
  const state = stateFor(container);
  const myRun = ++state.run;

  clearChildren(container);

  const searchInput = el("input", {
    type: "search",
    "data-role": "icon-search",
    placeholder: t(`${keyPrefix}.searchPlaceholder`),
  });
  const status = el("span", { class: "empty-state" });
  const row = el("div", { class: "row" });

  container.append(searchInput, status, row);

  const clearButton = el(
    "button",
    {
      type: "button",
      class: "btn btn--ghost",
      "aria-pressed": "false",
      "data-role": "icon-none",
      onclick: (event) => choose(event.currentTarget, null),
    },
    [text(t(`${keyPrefix}.clear`))],
  );

  async function choose(button, iconName, recordAlias) {
    if (state.settling) return;
    state.settling = true;
    const previous = row.querySelector(".btn--selected");
    markChosen(row, button);
    try {
      await onPick(iconName);
      if (recordAlias) {
        // Fire-and-forget: teaching the alias table is a courtesy to the
        // next search, not a condition of this pick having succeeded
        // (docs/specs/40-icon-picker.md — "no confirmation step, no checkbox
        // to opt out"), so a failure here must not surface as if the pick
        // itself failed.
        post(`/api/storages/${storageId}/icon-suggestions/aliases`, recordAlias).catch(() => {});
      }
    } catch {
      if (previous) markChosen(row, previous);
      else unmarkAll(row);
    } finally {
      state.settling = false;
    }
  }

  function renderResults(suggestions, query, highlightIconName) {
    clearChildren(row);
    row.append(clearButton);
    let highlightButton = null;
    for (const suggestion of suggestions) {
      const isDirectNameHit = !suggestion.matched_alias;
      const button = el(
        "button",
        {
          type: "button",
          class: "btn btn--ghost",
          "aria-pressed": "false",
          "data-role": "icon-suggestion",
          "data-icon-name": suggestion.icon_name,
          "aria-label": suggestion.matched_alias
            ? t(`${keyPrefix}.matchedVia`, { name: suggestion.icon_name, alias: suggestion.matched_alias })
            : t(`${keyPrefix}.useThis`, { name: suggestion.icon_name }),
          onclick: (event) =>
            choose(
              event.currentTarget,
              suggestion.icon_name,
              // Only a direct-name hit records a new alias, and only for a
              // query the person actually typed — the initial current-icon
              // lookup below passes no query, so picking it back records
              // nothing (docs/specs/40-icon-picker.md).
              isDirectNameHit && query ? { icon_name: suggestion.icon_name, alias: query } : null,
            ),
        },
        [iconGraphic(suggestion, storageId)],
      );
      row.append(button);
      if (suggestion.icon_name === highlightIconName) highlightButton = button;
    }
    container.append(row);
    // The initial open highlights the product's current icon
    // (docs/specs/40-icon-picker.md) — never a later, user-typed search that
    // happens to also match it, which is why callers only pass
    // highlightIconName on the lookup runSearch does at open time.
    if (highlightButton) markChosen(row, highlightButton);
  }

  async function runSearch(query, { recordQuery, highlightIconName } = {}) {
    const mySearchRun = ++state.searchRun;
    status.textContent = t(`${keyPrefix}.loading`);

    let suggestions = [];
    try {
      const body = await get(`/api/storages/${storageId}/icon-suggestions?query=${encodeURIComponent(query)}`);
      if (myRun !== state.run || mySearchRun !== state.searchRun) return;
      suggestions = body.suggestions || [];
    } catch {
      if (myRun !== state.run || mySearchRun !== state.searchRun) return;
      suggestions = [];
    }

    if (suggestions.length === 0) {
      status.textContent = t(`${keyPrefix}.none`);
      clearChildren(row);
      row.append(clearButton);
      container.append(row);
      return;
    }

    status.textContent = t(`${keyPrefix}.pick`);
    renderResults(suggestions, recordQuery ? query : null, highlightIconName);
  }

  let debounceHandle;
  searchInput.addEventListener("input", (event) => {
    clearTimeout(debounceHandle);
    const query = event.currentTarget.value.trim();
    debounceHandle = setTimeout(() => {
      if (query === "") {
        status.textContent = "";
        clearChildren(row);
        row.append(clearButton);
        container.append(row);
        return;
      }
      runSearch(query, { recordQuery: true });
    }, SEARCH_DEBOUNCE_MS);
  });

  if (currentIconName) {
    await runSearch(currentIconName, { recordQuery: false, highlightIconName: currentIconName });
  } else {
    status.textContent = "";
    row.append(clearButton);
    container.append(row);
  }

  searchInput.focus();
}

/**
 * iconGraphic renders one search result's actual icon (docs/specs/40-icon-
 * picker.md: "a person recognizes a beer icon on sight far faster than they
 * recognize its name"), never its bare identifier text.
 */
function iconGraphic(suggestion, storageId) {
  if (suggestion.svg_body) {
    return parseTrustedSVG(suggestion.svg_body);
  }
  return el("img", {
    src: `/api/storages/${storageId}/icons/${suggestion.icon_id}/svg`,
    alt: "",
    width: ICON_SIZE,
    height: ICON_SIZE,
    loading: "lazy",
  });
}

/**
 * parseTrustedSVG turns a vendored icon's SVG body into a live DOM node
 * without ever touching innerHTML (js/dom.js's rule): DOMParser parses the
 * string into its own inert document, and importNode adopts the resulting
 * <svg> element into this one. Only ever called on `svg_body`, which the
 * server sends only for a `vendored` icon — trusted, offline-vendored
 * content (docs/specs/42-local-icon-library.md) — never on anything a person
 * uploaded.
 */
function parseTrustedSVG(svgBody) {
  const parsed = new DOMParser().parseFromString(svgBody, "image/svg+xml");
  const svg = document.importNode(parsed.documentElement, true);
  svg.setAttribute("width", String(ICON_SIZE));
  svg.setAttribute("height", String(ICON_SIZE));
  return svg;
}

function markChosen(container, button) {
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
