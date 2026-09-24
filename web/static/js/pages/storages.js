import "../register-sw.js";

// Page module for storages.html — the one place that decides where a signed-in
// user lands (docs/specs/34-navigation-and-start-page.md).
//
// The page renders exactly two states now: the storage picker, for someone
// with more than one membership and nothing remembered, and the zero-storage
// empty state of docs/specs/29-first-run-admin-guidance.md. Once a storage is
// resolved it forwards to that storage's start page and renders nothing at
// all. The landing card of buttons it used to show is gone — every page has
// the navigation bar now, so a card that duplicated it would be a second
// navigation to keep in step.
//
// It is also the PWA's `start_url`. A signed-in user opening the installed app
// lands here and is forwarded on; a signed-out one gets a 401 from
// `GET /api/auth/me`, which js/api.js already redirects to the login page. The
// login form therefore appears exactly when it is needed and not before.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam, renderEmptyState } from "../session.js";
import { startPagePath, startPageFor } from "../nav.js";
import { el, text, clearChildren } from "../dom.js";
import { post, ApiError } from "../api.js";
import { t, apiErrorMessage } from "../i18n.js";

const mainContainer = document.querySelector("#main");
const logoutButton = document.querySelector("#logout");

logoutButton.addEventListener("click", handleLogout);

init();

async function init() {
  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    // A plain fetch/network failure, not a 401 (api.js already redirects
    // those to the login page before this catch ever sees them).
    renderLoadError(err);
    return;
  }

  if (me.storages.length === 0) {
    // Where a user with no storage belongs is decided by the server, not
    // here (docs/specs/29-first-run-admin-guidance.md). This page cannot
    // know whether the caller is an admin — no response it reads carries
    // that, and branching on it is forbidden
    // (docs/specs/03-auth-and-multi-tenancy.md) — so it navigates to one
    // neutral route and is never told what was decided. It only ever
    // observes its own outcome: coming back here with empty=1 means "render
    // the empty state", and nothing else.
    //
    // location.replace, not assign: the entry we would leave behind is the
    // one the server just redirected away from, so Back would bounce
    // through the same redirect again instead of leaving the app.
    if (new URLSearchParams(location.search).get("empty") !== "1") {
      location.replace("/no-storages");
      return;
    }
    renderEmptyState(mainContainer);
    return;
  }

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    // More than one membership and nothing selected yet: ask, per
    // docs/specs/05-frontend-pwa-foundations.md — never guess which
    // household the user meant.
    renderPicker(me.storages);
    return;
  }

  forwardToStartPage(me.storages, resolved);
}

/**
 * forwardToStartPage sends the browser to this member's chosen start page for
 * the resolved storage, carrying `?storage=`.
 *
 * `location.replace` rather than `assign`, and for the same reason as the
 * zero-storage redirect above: this page is a forwarder, so leaving it in the
 * history would make Back from the start page bounce straight through it and
 * land where it started. With `replace`, Back leaves the app.
 *
 * @param {Array<{id: string, start_page?: string}>} storages - `me.storages`.
 * @param {string} storageId
 */
function forwardToStartPage(storages, storageId) {
  rememberStorageId(storageId);
  location.replace(withStorageParam(storageId, startPagePath(startPageFor(storages, storageId))));
}

function renderPicker(storages) {
  clearChildren(mainContainer);
  mainContainer.append(
    el("h2", {}, [t("storages.choose")]),
    el(
      "div",
      { class: "stack" },
      storages.map((storage) =>
        el(
          "button",
          {
            type: "button",
            class: "btn btn--block card",
            // Picking a storage lands on that storage's own start page, like
            // every other way of resolving one.
            onclick: () => forwardToStartPage(storages, storage.id),
          },
          [text(storage.name)],
        ),
      ),
    ),
  );
}

function renderLoadError(err) {
  clearChildren(mainContainer);
  mainContainer.append(
    el("div", { class: "alert", role: "alert" }, [
      t("storages.loadFailedPrefix") + " ",
      err instanceof ApiError ? apiErrorMessage(err) : t("storages.networkError"),
    ]),
  );
}

// handleLogout stays on this page as well as in js/nav.js, and deliberately.
// The bar is what carries Log out everywhere else, but this page renders no
// bar in either of its two states — there is no resolved storage to build one
// around — and a user who belongs to no storage at all must still be able to
// sign out. The bar's copy is the one that moved; this is the one that was
// always here.
async function handleLogout() {
  logoutButton.disabled = true;
  try {
    await post("/api/auth/logout", {}, { skipAuthRedirect: true });
  } catch {
    // Logging out is best-effort from the client's point of view: the
    // session cookie is cleared server-side on success, but even if this
    // call fails, sending the user back to the login page is the right
    // outcome either way rather than leaving them stuck on a broken button.
  }
  location.assign("/index.html");
}
