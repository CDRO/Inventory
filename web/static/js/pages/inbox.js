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
import { get, del, ApiError } from "../api.js";
import { el, fromTemplate, qs, text } from "../dom.js";

const jobsList = qs("#jobs");
const moreButton = qs("#more");
const template = qs("#job-template");
const errorBox = qs("#error");
const notice = qs("#notice");

const KIND_LABELS = {
  shelf_ingestion: "Shelf photo",
  product_photo: "Product photo",
  shopping_list_photo: "Shopping list photo",
  consumption_photo: "Consumption photo",
};

const STATUS_LABELS = {
  pending: "Analysing",
  done: "Ready to review",
  failed: "Failed",
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
  qs("#scan-link").setAttribute("href", withStorageParam(storageId, "/ingest.html"));

  const confirmed = params.get("confirmed");
  if (confirmed !== null) {
    notice.textContent = confirmationText(
      Number(confirmed),
      Number(params.get("products") || 0),
      Number(params.get("locations") || 0),
    );
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
        el("p", { class: "empty-state" }, [text("Nothing waiting. Photos you upload appear here until you review them.")]),
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

  qs('[data-role="kind"]', card).textContent = KIND_LABELS[job.kind] || job.kind;
  qs('[data-role="status"]', card).textContent = STATUS_LABELS[job.status] || job.status;

  if (job.has_image) {
    const thumb = qs('[data-role="thumb"]', card);
    thumb.src = `/api/storages/${storageId}/jobs/${job.id}/image`;
    thumb.hidden = false;
  }

  const parts = [age(job.created_at)];
  if (job.status === "done" && job.item_count != null) {
    parts.push(job.item_count === 1 ? "1 item" : `${job.item_count} items`);
  }
  if (job.status === "failed" && job.error) {
    // The server writes these messages for the reader; still text, never markup.
    parts.push(job.error);
  }
  qs('[data-role="detail"]', card).textContent = parts.join(" · ");

  const actions = qs('[data-role="actions"]', card);
  if (job.status === "done") {
    const href = new URL(withStorageParam(storageId, "/review.html"), location.origin);
    href.searchParams.set("job", job.id);
    actions.append(el("a", { class: "btn btn--primary", href: href.pathname + href.search }, [text("Review")]));
  }
  actions.append(
    el("button", { type: "button", class: "btn btn--ghost", onclick: () => discard(job, card) }, [text("Discard")]),
  );
  return card;
}

async function discard(job, card) {
  if (!window.confirm("Discard this photo and its proposal? Nothing has been added to your inventory from it.")) {
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
  const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
  if (!Number.isInteger(batches) || batches <= 0) {
    return "Proposal applied. Nothing was added to your inventory.";
  }
  const parts = [plural(batches, "item", "items") + " added to your inventory"];
  if (products > 0) parts.push(plural(products, "new product", "new products"));
  if (locations > 0) parts.push(plural(locations, "new location", "new locations"));
  return `Proposal applied: ${parts.join(", ")}.`;
}

function age(iso) {
  const days = Math.floor((Date.now() - new Date(iso).getTime()) / 86_400_000);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  return `${days} days ago`;
}

function showError(err) {
  errorBox.textContent =
    err instanceof ApiError ? err.message : "Could not reach the server. Check your connection and try again.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
