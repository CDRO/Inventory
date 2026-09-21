import "../register-sw.js";

// Page module for storages.html.
//
// docs/specs/05-frontend-pwa-foundations.md scopes this page to the storage
// picker ("only if >1 membership"). dashboard.html now ships the reorder
// dashboard from docs/specs/10-reorder-and-shopping-export.md; until
// docs/specs/11-reporting-and-analytics.md's turnover/waste charts land on
// it too, this page also serves as the landing shell once a storage is
// resolved — a header with the switcher, and an honest placeholder rather
// than a fabricated one. That scope note is recorded on the PR for issue #10.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam, renderEmptyState } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { initGamification } from "../gamification.js";
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
  renderInboxLink(document.querySelector("#inbox-link"), resolved);
  initGamification(resolved);
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
      el("div", { class: "row" }, [
        el("a", { class: "btn", href: withStorageParam(storage.id, "/dashboard.html") }, [
          text("Dashboard"),
        ]),
        el("a", { class: "btn", href: withStorageParam(storage.id, "/locations.html") }, [
          text("Locations"),
        ]),
        el("a", { class: "btn", href: withStorageParam(storage.id, "/categories.html") }, [
          text("Categories"),
        ]),
        el("a", { class: "btn", href: withStorageParam(storage.id, "/shopping-list.html") }, [
          text("Shopping list"),
        ]),
        el("a", { class: "btn", href: withStorageParam(storage.id, "/ingest.html") }, [
          text("Scan photos"),
        ]),
      ]),
      el("p", { class: "empty-state" }, [
        "Browsing and managing products directly ships with its own spec issue " +
          '— use "Scan photos" to stock up, use up, or scan a shelf in the meantime.',
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
