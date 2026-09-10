import "../register-sw.js";

// Page module for storages.html.
//
// docs/specs/05-frontend-pwa-foundations.md scopes this page to the storage
// picker ("only if >1 membership"). Until dashboard.html ships with
// docs/specs/10-reorder-and-shopping-export.md and
// docs/specs/11-reporting-and-analytics.md, this page also serves as the
// landing shell once a storage is resolved — a header with the switcher, and
// an honest placeholder rather than a fabricated dashboard. That scope note
// is recorded on the PR for issue #10.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam, renderEmptyState } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { el, text, clearChildren } from "../dom.js";
import { post, ApiError } from "../api.js";

const switcherContainer = document.querySelector("#storage-switcher");
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
    switcherContainer.hidden = true;
    renderEmptyState(mainContainer);
    return;
  }

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    // More than one membership and nothing selected yet: ask, per
    // docs/specs/05-frontend-pwa-foundations.md — never guess which
    // household the user meant.
    switcherContainer.hidden = true;
    renderPicker(me.storages);
    return;
  }

  rememberStorageId(resolved);
  // Canonicalise the URL so a bookmark or reload lands on the same storage
  // without asking again — "a link is always sufficient to describe where
  // the user is" only holds if the link actually carries the id.
  if (new URLSearchParams(location.search).get("storage") !== resolved) {
    history.replaceState(null, "", withStorageParam(resolved));
  }

  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: resolved });
  renderLanding(me, me.storages.find((s) => s.id === resolved));
}

function renderPicker(storages) {
  clearChildren(mainContainer);
  mainContainer.append(
    el("h2", {}, ["Choose a storage"]),
    el(
      "div",
      { class: "stack" },
      storages.map((storage) =>
        el(
          "button",
          {
            type: "button",
            class: "btn btn--block card",
            onclick: () => {
              rememberStorageId(storage.id);
              location.assign(withStorageParam(storage.id));
            },
          },
          [text(storage.name)],
        ),
      ),
    ),
  );
}

function renderLanding(me, storage) {
  clearChildren(mainContainer);
  mainContainer.append(
    el("div", { class: "card stack" }, [
      el("h2", {}, [storage.name]),
      el("p", {}, [`Signed in as ${me.display_name}.`]),
      el("a", { class: "btn", href: withStorageParam(storage.id, "/locations.html") }, [
        text("Locations"),
      ]),
      el("p", { class: "empty-state" }, [
        "Products, shelf scanning, shopping lists, consumption logging, and " +
          "the reorder dashboard each ship with their own spec issue.",
      ]),
    ]),
  );
}

function renderLoadError(err) {
  clearChildren(mainContainer);
  mainContainer.append(
    el("div", { class: "alert", role: "alert" }, [
      "Could not load your account. ",
      err instanceof ApiError ? err.message : "Check your connection and try again.",
    ]),
  );
}

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
