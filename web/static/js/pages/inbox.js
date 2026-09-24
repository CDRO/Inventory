import "../register-sw.js";

// Page module for inbox.html — the review inbox of
// docs/specs/06-vision-shelf-ingestion.md.
//
// Everything uploaded to this storage and not yet applied: proposals waiting
// for review, photos still being analysed, and failures with the reason. Any
// member reviews any job, not only the one who took the photo. Nothing here
// expires on its own; a job leaves the inbox only by being confirmed or
// discarded.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { initGamification } from "../gamification.js";
import { get, del, ApiError } from "../api.js";
import { el, fromTemplate, qs, text } from "../dom.js";
import { t, tCount, apiErrorMessage } from "../i18n.js";

const jobsList = qs("#jobs");
const moreButton = qs("#more");
const template = qs("#job-template");
const errorBox = qs("#error");
const notice = qs("#notice");

const KIND_KEYS = {
  shelf_ingestion: "inbox.kind.shelf_ingestion",
  product_photo: "inbox.kind.product_photo",
  shopping_list_photo: "inbox.kind.shopping_list_photo",
  consumption_photo: "inbox.kind.consumption_photo",
};

const STATUS_KEYS = {
  pending: "inbox.status.pending",
  done: "inbox.status.done",
  failed: "inbox.status.failed",
};

let storageId = null;
let cursor = null;

init();

async function init() {
  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    showError(err);
    return;
  }

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    location.assign("/storages.html");
    return;
  }
  storageId = resolved;
  rememberStorageId(storageId);
  const params = new URLSearchParams(location.search);
  if (params.get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(qs("#storage-switcher"), { storages: me.storages, currentId: storageId });
  initGamification(storageId);
  qs("#scan-link").setAttribute("href", withStorageParam(storageId, "/ingest.html"));

  const confirmed = params.get("confirmed");
  const consumed = params.get("consumed");
  if (confirmed !== null) {
    notice.textContent = confirmationText(
      Number(confirmed),
      Number(params.get("products") || 0),
      Number(params.get("locations") || 0),
    );
    notice.hidden = false;
  } else if (consumed !== null) {
    notice.textContent = consumptionText(Number(consumed));
    notice.hidden = false;
  }

  moreButton.addEventListener("click", loadPage);
  await loadPage();
}

async function loadPage() {
  clearError();
  moreButton.disabled = true;
  try {
    const query = new URLSearchParams({ limit: "20" });
    if (cursor) query.set("cursor", cursor);
    const page = await get(`/api/storages/${storageId}/jobs?${query}`);

    if (!cursor && page.items.length === 0) {
      jobsList.replaceChildren(
        el("p", { class: "empty-state" }, [text(t("inbox.emptyState"))]),
      );
    }
    for (const job of page.items) {
      jobsList.append(renderJob(job));
    }

    cursor = page.next_cursor;
    moreButton.hidden = cursor == null;
  } catch (err) {
    showError(err);
  } finally {
    moreButton.disabled = false;
  }
}

function renderJob(job) {
  const card = fromTemplate(template);
  card.dataset.jobId = job.id;

  qs('[data-role="kind"]', card).textContent = KIND_KEYS[job.kind] ? t(KIND_KEYS[job.kind]) : job.kind;
  qs('[data-role="status"]', card).textContent = STATUS_KEYS[job.status] ? t(STATUS_KEYS[job.status]) : job.status;

  if (job.has_image) {
    const thumb = qs('[data-role="thumb"]', card);
    thumb.src = `/api/storages/${storageId}/jobs/${job.id}/image`;
    thumb.hidden = false;
  }

  const parts = [age(job.created_at)];
  if (job.status === "done" && job.item_count != null) {
    parts.push(tCount("inbox.itemCount", job.item_count));
  }
  if (job.status === "failed" && job.error) {
    // The server writes these messages for the reader; still text, never markup.
    parts.push(job.error);
  }
  qs('[data-role="detail"]', card).textContent = parts.join(" · ");

  const actions = qs('[data-role="actions"]', card);
  if (job.status === "done") {
    // Consumption jobs review on their own screen
    // (docs/specs/09-consumption-logging.md): the row shape — batches to
    // decrement, not a location to place into — differs enough that they
    // need a different page while still sharing js/review.js underneath.
    const page = job.kind === "consumption_photo" ? "/consume-review.html" : "/review.html";
    const href = new URL(withStorageParam(storageId, page), location.origin);
    href.searchParams.set("job", job.id);
    actions.append(el("a", { class: "btn btn--primary", href: href.pathname + href.search }, [text(t("inbox.review"))]));
  }
  actions.append(
    el("button", { type: "button", class: "btn btn--ghost", onclick: () => discard(job, card) }, [text(t("inbox.discard"))]),
  );
  return card;
}

async function discard(job, card) {
  if (!window.confirm(t("inbox.discardConfirm"))) {
    return;
  }
  clearError();
  try {
    await del(`/api/storages/${storageId}/jobs/${job.id}`);
    card.remove();
  } catch (err) {
    showError(err);
  }
}

// confirmationText summarises a confirm from the counts the server returned.
// They arrive through the URL, so they are read as numbers and only numbers
// are printed.
function confirmationText(batches, products, locations) {
  if (!Number.isInteger(batches) || batches <= 0) {
    return t("inbox.confirmNothingAdded");
  }
  const parts = [tCount("inbox.confirmItemsAdded", batches)];
  if (products > 0) parts.push(tCount("inbox.confirmNewProducts", products));
  if (locations > 0) parts.push(tCount("inbox.confirmNewLocations", locations));
  return t("inbox.confirmApplied", { parts: parts.join(", ") });
}

// consumptionText summarises a consumption confirm from the batch count the
// server returned, arriving through the URL and read as a number.
function consumptionText(batches) {
  if (!Number.isInteger(batches) || batches <= 0) {
    return t("inbox.consumeNothingRemoved");
  }
  return tCount("inbox.consumeBatchesUpdated", batches);
}

function age(iso) {
  const days = Math.floor((Date.now() - new Date(iso).getTime()) / 86_400_000);
  if (days <= 0) return t("inbox.today");
  if (days === 1) return t("inbox.yesterday");
  return tCount("inbox.daysAgo", days);
}

function showError(err) {
  errorBox.textContent = err instanceof ApiError ? apiErrorMessage(err) : t("inbox.networkError");
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
