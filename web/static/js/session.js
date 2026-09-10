// Current user + storage resolution (docs/specs/05-frontend-pwa-foundations.md).
//
// The active storage is carried in the query string and resolved once at
// page load; it is never held in ambient global state, so a bookmarked or
// shared link is always sufficient to describe where the user is. Callers
// read the resolved id from what this module returns and pass it along
// themselves — there is no module-level "current storage" variable to get out
// of sync with the URL.

import { el, clearChildren } from "./dom.js";
import { get } from "./api.js";

const STORAGE_KEY = "inventory:lastStorageId";

/**
 * Me is the shape of `GET /api/auth/me`
 * (docs/specs/03-auth-and-multi-tenancy.md). `is_admin` is deliberately not a
 * field here — the response never carries it, and this module never checks
 * for it.
 *
 * @typedef {Object} Me
 * @property {string} id
 * @property {string} username
 * @property {string} display_name
 * @property {Array<{id: string, name: string}>} storages
 */

/**
 * fetchMe calls `GET /api/auth/me` once. Callers on a storage-scoped page
 * call this on load, per the spec; nothing here caches the result, so a
 * caller that needs it more than once holds onto what this returns rather
 * than calling it again.
 *
 * @returns {Promise<Me>}
 */
export function fetchMe() {
  return get("/api/auth/me");
}

/**
 * getQueryStorageId reads `?storage=` from the current URL. It does not
 * validate that the id names a storage the caller belongs to — resolveStorage
 * does that — so a stale or forged query value is never trusted on its own.
 *
 * @returns {string|null}
 */
export function getQueryStorageId() {
  return new URLSearchParams(location.search).get("storage");
}

/**
 * withStorageParam returns the current path with `storage` set to id, for
 * navigation that must keep the rest of the query string and hash intact.
 *
 * @param {string} id
 * @returns {string}
 */
export function withStorageParam(id) {
  const url = new URL(location.href);
  url.searchParams.set("storage", id);
  return url.pathname + url.search;
}

/**
 * rememberStorageId persists the selection in localStorage, so a page opened
 * without `?storage=` — a bookmark to the app's root, say — can be resolved
 * without asking again. localStorage is per-browser, per-origin storage; it
 * is never sent to the server and carries nothing the backend trusts.
 *
 * A failure here (private browsing, a full quota) is silently ignored: this
 * is a convenience, and losing it must never block using the app.
 *
 * @param {string} id
 */
export function rememberStorageId(id) {
  try {
    localStorage.setItem(STORAGE_KEY, id);
  } catch {
    // See above: convenience only.
  }
}

/**
 * recallStorageId reads the last remembered selection, or null if there is
 * none or storage is unavailable.
 *
 * @returns {string|null}
 */
export function recallStorageId() {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

/**
 * resolveStorage picks the effective storage id for this page load, given the
 * caller's own memberships. Order of preference, per
 * docs/specs/05-frontend-pwa-foundations.md:
 *
 *   1. `?storage=` in the URL, if it names a storage the caller belongs to —
 *      a link is always sufficient to describe where the user is, so an
 *      explicit query value wins over anything remembered.
 *   2. The remembered selection from a previous visit, if still valid — a
 *      membership can be revoked between visits, so a stale value is not
 *      trusted just because it once was.
 *   3. The caller's only storage, when they belong to exactly one — there is
 *      nothing to choose, so nothing is asked.
 *
 * Returns null when none of these apply: the caller belongs to more than one
 * storage and neither the URL nor localStorage names one. The page must then
 * ask, rather than guessing which of several households the user meant.
 *
 * @param {Array<{id: string, name: string}>} storages
 * @returns {string|null}
 */
export function resolveStorage(storages) {
  const fromUrl = getQueryStorageId();
  if (fromUrl && storages.some((s) => s.id === fromUrl)) {
    return fromUrl;
  }

  const remembered = recallStorageId();
  if (remembered && storages.some((s) => s.id === remembered)) {
    return remembered;
  }

  if (storages.length === 1) {
    return storages[0].id;
  }

  return null;
}

/**
 * renderEmptyState shows the "ask an admin for access" state for a user with
 * zero storage memberships, into `container`.
 *
 * This must never be rendered as an error, and must never imply that other
 * storages exist which this user cannot see
 * (docs/specs/03-auth-and-multi-tenancy.md) — the copy below says only that
 * this user has no storage, nothing about how many there are in total.
 *
 * @param {Element} container
 */
export function renderEmptyState(container) {
  clearChildren(container);
  container.append(
    el("div", { class: "empty-state" }, [
      el("h2", {}, ["No storage yet"]),
      el("p", {}, [
        "Ask an admin to add you to a storage to get started.",
      ]),
    ]),
  );
}
