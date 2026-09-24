// The navigation bar, and the one place the start page is turned into a path
// (docs/specs/34-navigation-and-start-page.md).
//
// There is one implementation of the navigation, not one per page — the same
// rule docs/specs/05-frontend-pwa-foundations.md applies to the review
// component and the location tree. Every storage-scoped page calls
// `renderNav` and gets the identical bar; a page that wanted its own entry
// order or its own logout button would have to stop using this module, which
// is the point.
//
// **This module is never told who is an admin, and renders no link to the
// admin area.** It takes a storage id, a page key and a start page, and there
// is no fourth argument it could be handed that would carry admin status
// (docs/specs/03-auth-and-multi-tenancy.md). An admin with no storage is sent
// to the admin view by the *server*
// (docs/specs/29-first-run-admin-guidance.md), precisely so that no
// client-side link has to exist. web/embed_test.go's
// TestNoStaticAssetNamesTheAdminArea scans every file served from
// web/static/, this one included, for the path — in code, copy or a comment.
//
// Pages with no resolved storage — the login form, the storage picker and the
// zero-storage empty state — render no bar. There would be nothing to link
// to, and showing links that led to empty states would imply storages exist
// which the user cannot see.

import { el, text, clearChildren } from "./dom.js";
import { post } from "./api.js";
import { withStorageParam } from "./session.js";
import { renderInboxLink } from "./inbox-badge.js";
import { t } from "./i18n.js";

/**
 * START_PAGES is the closed list of docs/specs/34-navigation-and-start-page.md
 * as a value→path map: what `storage_members.start_page` may hold, and where
 * each value sends a browser.
 *
 * The same list is written out twice more — the column's `CHECK` in
 * migrations/00013_storage_member_start_page.sql and the Go allowlist in
 * internal/httpapi/membership.go — and
 * TestStartPagesMatchTheShippedNavigationMap parses this literal to keep the
 * three in step. A page added here and nowhere else is a select box whose
 * choice the API answers `422` to.
 *
 * Pages that need a parameter to mean anything (`review.html?job=`,
 * `stocktake.html?location=`) are deliberately absent: a start page with no
 * parameter would open on an error. So is `settings.html`, which is not a
 * place anyone starts their day.
 *
 * @type {Object<string, string>}
 */
export const START_PAGES = {
  dashboard: "/dashboard.html",
  inventory: "/inventory.html",
  products: "/products.html",
  locations: "/locations.html",
  shopping_list: "/shopping-list.html",
  ingest: "/ingest.html",
  inbox: "/inbox.html",
};

/** The column default, for a value the server has not answered with yet. */
const DEFAULT_START_PAGE = "dashboard";

// The catalog key naming each start page, for the settings select
// (docs/specs/19-localization.md). Kept beside START_PAGES rather than merged
// into it so that map stays the flat value→path literal the Go test parses.
// The labels are the navigation bar's own, deliberately: the select offers the
// same destinations under the same words the user reads in the bar.
const START_PAGE_LABELS = {
  dashboard: "nav.dashboard",
  inventory: "nav.inventory",
  products: "nav.products",
  locations: "nav.locations",
  shopping_list: "nav.shoppingList",
  ingest: "nav.scan",
  inbox: "inboxBadge.inbox",
};

/**
 * startPageOptions lists the choosable start pages, in the order of
 * START_PAGES, each with its translated label.
 *
 * settings.html builds its select from this rather than spelling the list out
 * in markup: a fourth copy of a closed list is a fourth place to forget.
 * A value with no label renders as the raw value — ugly on purpose, the same
 * way a missing catalog key renders as the key.
 *
 * @returns {Array<{value: string, label: string}>}
 */
export function startPageOptions() {
  return Object.keys(START_PAGES).map((value) => ({
    value,
    label: t(START_PAGE_LABELS[value] ?? value),
  }));
}

/**
 * startPagePath maps a stored `start_page` to the page it names, falling back
 * to the dashboard for anything unrecognised.
 *
 * The fallback matters on exactly one day: the one where a newer server has
 * been deployed, knows a value this cached copy of the app does not, and
 * sends it. Landing on the dashboard is wrong but harmless; `undefined` in a
 * URL is a blank page with no way out.
 *
 * @param {string|undefined} startPage
 * @returns {string} a path, e.g. "/dashboard.html"
 */
export function startPagePath(startPage) {
  return START_PAGES[startPage] ?? START_PAGES[DEFAULT_START_PAGE];
}

/**
 * startPageFor reads the caller's own start page for one storage out of what
 * `GET /api/auth/me` returned. The response carries the caller's value and no
 * other member's, so there is nothing here to pick between.
 *
 * @param {Array<{id: string, start_page?: string}>} storages - `me.storages`.
 * @param {string} storageId
 * @returns {string} a `start_page` value; the default when the server sent none.
 */
export function startPageFor(storages, storageId) {
  const storage = (storages || []).find((s) => s.id === storageId);
  return storage?.start_page ?? DEFAULT_START_PAGE;
}

// The bar's entries, in the order docs/specs/34-navigation-and-start-page.md
// lists them. `key` is what a page passes as `current`; it is not the
// start_page value list — Categories and Stocktake are reachable but cannot be
// chosen as a start page, and Settings sits in the end group below.
//
// Stocktake points at stocktake.html with no `?location=`, which
// docs/specs/35-stocktake-entry-points.md renders as a location chooser: the
// tree in read-only mode plus a "Stalest first" shortlist, rather than a page
// that needs a location and offers no way to pick one.
const NAV_ITEMS = [
  { key: "dashboard", path: "/dashboard.html", label: "nav.dashboard" },
  { key: "inventory", path: "/inventory.html", label: "nav.inventory" },
  { key: "products", path: "/products.html", label: "nav.products" },
  { key: "locations", path: "/locations.html", label: "nav.locations" },
  { key: "categories", path: "/categories.html", label: "nav.categories" },
  { key: "shopping_list", path: "/shopping-list.html", label: "nav.shoppingList" },
  { key: "stocktake", path: "/stocktake.html", label: "nav.stocktake" },
  { key: "ingest", path: "/ingest.html", label: "nav.scan" },
];

/**
 * renderNav fills `container` with the navigation bar and points the header's
 * wordmark at this storage's start page.
 *
 * The wordmark is the "home" link, and it goes wherever the user chose, so
 * that tapping it matches what opening the app shows. It lives in the page's
 * own `<header>` rather than in `container`, and this module reaches it by id
 * — that is the one exception to "render only into what you were handed", and
 * it is here because the wordmark is navigation and this is the navigation
 * module.
 *
 * @param {Element} container - the page's `<nav>`; nothing is rendered if absent.
 * @param {Object} options
 * @param {string} options.storageId - the resolved storage; every link carries it.
 * @param {string} [options.current] - this page's own key, marked
 *   `aria-current="page"`. Pages with no entry of their own (review,
 *   consume-review) pass nothing and no entry is marked.
 * @param {string} [options.startPage] - the caller's `start_page` for this
 *   storage, for the wordmark.
 */
export function renderNav(container, { storageId, current, startPage } = {}) {
  const wordmark = document.querySelector("#wordmark");
  if (wordmark) {
    wordmark.setAttribute("href", withStorageParam(storageId, startPagePath(startPage)));
  }

  if (!container) return;
  clearChildren(container);

  let currentLink = null;
  for (const item of NAV_ITEMS) {
    const link = navLink(item.label, withStorageParam(storageId, item.path), item.key === current);
    if (item.key === current) currentLink = link;
    container.append(link);
  }

  // The inbox entry is the existing badge component rather than a link built
  // here, so the waiting count keeps one implementation. The container keeps
  // the id the badge has always been rendered into, so a page or a test that
  // reaches for `#inbox-link` finds it in the bar rather than finding nothing.
  const inbox = el("span", { id: "inbox-link", class: "nav__item" });
  container.append(inbox);
  renderInboxLink(inbox, storageId, { current: current === "inbox" });
  if (current === "inbox") currentLink = inbox;

  // Settings and Log out, visually separated by a rule rather than by being
  // pushed to the far end: inside a horizontally scrolling row there is no
  // "far end" to push them to.
  container.append(el("span", { class: "nav__separator", "aria-hidden": "true" }));
  const settings = navLink("common.settings", withStorageParam(storageId, "/settings.html"), current === "settings");
  container.append(settings);
  if (current === "settings") currentLink = settings;
  container.append(logoutButton());

  container.hidden = false;

  // On a phone the bar scrolls sideways, so the entry for the page the user
  // is on can start off-screen. `inline: "nearest"` scrolls the bar and not
  // the page; omitting `block` would let the browser scroll the document
  // vertically to reach it, which on load reads as the page jumping.
  if (currentLink?.scrollIntoView) {
    currentLink.scrollIntoView({ inline: "nearest", block: "nearest" });
  }
}

/**
 * navLink builds one entry. `aria-current="page"` is the only marker that
 * carries to a screen reader, so the class exists for the eye and the
 * attribute for everything else — never one without the other.
 *
 * @param {string} labelKey - a catalog key (docs/specs/19-localization.md).
 * @param {string} href
 * @param {boolean} isCurrent
 * @returns {HTMLElement}
 */
function navLink(labelKey, href, isCurrent) {
  return el(
    "a",
    {
      class: isCurrent ? "nav__link nav__link--current" : "nav__link",
      href,
      "aria-current": isCurrent ? "page" : null,
    },
    [text(t(labelKey))],
  );
}

/**
 * logoutButton is the only way to sign out of a storage-scoped page.
 *
 * It moved here from storages.html, which used to be the one screen carrying
 * it. That page still has its own: it renders no bar in either of the two
 * states it is left with — the storage picker and the zero-storage empty
 * state — and a user with no membership must still be able to sign out.
 *
 * @returns {HTMLElement}
 */
function logoutButton() {
  return el(
    "button",
    { type: "button", id: "logout", class: "nav__link nav__link--button", onclick: handleLogout },
    [text(t("common.logout"))],
  );
}

/**
 * handleLogout ends the session and returns to the login page. Moved from
 * web/static/js/pages/storages.js unchanged, including its reasoning:
 *
 * logging out is best-effort from the client's point of view. The session
 * cookie is cleared server-side on success, but even if this call fails,
 * sending the user back to the login page is the right outcome either way
 * rather than leaving them stuck on a broken button.
 *
 * @param {Event} event
 */
async function handleLogout(event) {
  const button = event.currentTarget;
  button.disabled = true;
  try {
    await post("/api/auth/logout", {}, { skipAuthRedirect: true });
  } catch {
    // See above: the destination is the same either way.
  }
  location.assign("/index.html");
}
