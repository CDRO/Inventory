// The review-inbox link and its waiting count, shown next to the storage
// switcher (docs/specs/06-vision-shelf-ingestion.md).
//
// The count is the only nudge the system gives: no notifications, no emails.
// It counts proposals that are ready to review — done jobs — because those are
// the ones a person can act on; a pending job is not waiting for anyone yet.

import { get } from "./api.js";
import { clearChildren, el, text } from "./dom.js";
import { withStorageParam } from "./session.js";

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
 */
export async function renderInboxLink(container, storageId) {
  if (!container) return;
  clearChildren(container);

  const badge = el("span", { class: "badge", hidden: true });
  container.append(
    el("a", { class: "btn btn--ghost inbox-link", href: withStorageParam(storageId, "/inbox.html") }, [
      text("Inbox"),
      badge,
    ]),
  );
  container.hidden = false;

  try {
    const page = await get(`/api/storages/${storageId}/jobs?status=done&limit=${COUNT_LIMIT}`);
    const count = page.items.length;
    if (count > 0) {
      badge.textContent = page.next_cursor ? `${COUNT_LIMIT}+` : String(count);
      badge.setAttribute("aria-label", `${badge.textContent} waiting for review`);
      badge.hidden = false;
    }
  } catch {
    // See above: the link still works without a number.
  }
}
