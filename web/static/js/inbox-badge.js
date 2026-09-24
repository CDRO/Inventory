// The review-inbox link and its waiting count
// (docs/specs/06-vision-shelf-ingestion.md).
//
// It used to sit next to the storage switcher on whichever pages remembered
// to place it. Since docs/specs/34-navigation-and-start-page.md it is one
// entry of the navigation bar, rendered by js/nav.js on every storage-scoped
// page — which is also the module's only caller. The count logic stays here
// rather than moving into the bar so that there is still one implementation
// of "how many proposals are waiting".
//
// The count is the only nudge the system gives: no notifications, no emails.
// It counts proposals that are ready to review — done jobs — because those are
// the ones a person can act on; a pending job is not waiting for anyone yet.

import { get } from "./api.js";
import { clearChildren, el, text } from "./dom.js";
import { withStorageParam } from "./session.js";
import { t } from "./i18n.js";

// Past this many, the badge says "200+" rather than paging through an inbox
// just to print a number.
const COUNT_LIMIT = 200;

/**
 * renderInboxLink fills container with a link to the storage's inbox and, once
 * the count arrives, a badge when anything is waiting. A failed count leaves
 * the plain link: the badge is a convenience, never a reason to show an error.
 *
 * @param {Element} container
 * @param {string} storageId
 * @param {Object} [options]
 * @param {boolean} [options.current] - true on inbox.html itself, so this
 *   entry carries `aria-current="page"` like every other entry of the bar.
 */
export async function renderInboxLink(container, storageId, { current = false } = {}) {
  if (!container) return;
  clearChildren(container);

  const badge = el("span", { class: "badge", hidden: true });
  container.append(
    el(
      "a",
      {
        class: current ? "nav__link nav__link--current inbox-link" : "nav__link inbox-link",
        href: withStorageParam(storageId, "/inbox.html"),
        "aria-current": current ? "page" : null,
      },
      [text(t("inboxBadge.inbox")), badge],
    ),
  );
  container.hidden = false;

  try {
    const page = await get(`/api/storages/${storageId}/jobs?status=done&limit=${COUNT_LIMIT}`);
    const count = page.items.length;
    if (count > 0) {
      badge.textContent = page.next_cursor ? `${COUNT_LIMIT}+` : String(count);
      badge.setAttribute("aria-label", t("inboxBadge.waitingForReview", { count: badge.textContent }));
      badge.hidden = false;
    }
  } catch {
    // See above: the link still works without a number.
  }
}
