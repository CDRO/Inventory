import "../register-sw.js";

// Page module for ingest.html — the upload half of
// docs/specs/06-vision-shelf-ingestion.md.
//
// Each selected photo is its own request and its own job, fired one after
// another as soon as the form is submitted. The page never waits for analysis:
// a 202 means the photo is safe on the server, and from then on the result
// lives in the inbox whether or not anyone stays here. Waiting for a result on
// this screen is offered per photo, as a convenience, never as the only way to
// get one.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { renderInboxLink } from "../inbox-badge.js";
import { fetchLocations, appendLocationOptions } from "../location-options.js";
import { postForm, ApiError } from "../api.js";
import { pollJob, JobFailedError } from "../jobs.js";
import { el, fromTemplate, qs, text } from "../dom.js";

const form = qs("#upload-form");
const submitButton = qs("#submit");
const photosInput = qs("#photos");
const locationSelect = qs("#location");
const uploadsList = qs("#uploads");
const uploadTemplate = qs("#upload-template");
const errorBox = qs("#error");

let storageId = null;

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
  if (new URLSearchParams(location.search).get("storage") !== storageId) {
    history.replaceState(null, "", withStorageParam(storageId));
  }

  renderStorageSwitcher(qs("#storage-switcher"), { storages: me.storages, currentId: storageId });
  renderInboxLink(qs("#inbox-link"), storageId);

  try {
    appendLocationOptions(locationSelect, await fetchLocations(storageId));
    // The tree view's "scan this shelf" link arrives with ?location=.
    const preset = new URLSearchParams(location.search).get("location");
    if (preset && [...locationSelect.options].some((o) => o.value === preset)) {
      locationSelect.value = preset;
    }
  } catch (err) {
    showError(err);
  }

  form.addEventListener("submit", onSubmit);
}

async function onSubmit(event) {
  event.preventDefault();
  clearError();

  const files = [...photosInput.files];
  if (files.length === 0) return;

  const mode = new FormData(form).get("mode");
  const endpoint = mode === "product" ? "product-photos" : "shelf-photos";
  const hint = locationSelect.value;

  submitButton.disabled = true;
  // One after another rather than all at once: a phone on a weak connection
  // uploading six 8MB photos in parallel finishes none of them.
  for (const file of files) {
    const card = addUploadCard(file.name);
    await upload(card, endpoint, file, hint);
  }
  submitButton.disabled = false;
  form.reset();
}

async function upload(card, endpoint, file, hint) {
  setStatus(card, "Uploading…", "");
  const body = new FormData();
  body.append("image", file);
  if (hint) body.append("location_id", hint);

  try {
    const { job_id: jobId } = await postForm(`/api/storages/${storageId}/ingest/${endpoint}`, body);
    setStatus(card, "Uploaded", "Being analysed. It will be waiting in your inbox.");
    setActions(card, [
      el("button", { type: "button", class: "btn btn--ghost", onclick: () => waitFor(card, jobId) }, [
        text("Wait here for the result"),
      ]),
    ]);
  } catch (err) {
    setStatus(card, "Not uploaded", uploadErrorMessage(err));
  }
}

async function waitFor(card, jobId) {
  setActions(card, [el("span", { class: "spinner", "aria-hidden": "true" })]);
  try {
    const payload = await pollJob(storageId, jobId);
    const count = Array.isArray(payload?.rows) ? payload.rows.length : 0;
    setStatus(card, "Ready", count === 1 ? "1 item found." : `${count} items found.`);
    setActions(card, [
      el("a", { class: "btn btn--primary", href: reviewHref(jobId) }, [text("Review now")]),
    ]);
  } catch (err) {
    if (err instanceof JobFailedError) {
      setStatus(card, "Failed", err.message);
    } else {
      setStatus(card, "Unknown", "Could not check on this photo. Look for it in your inbox.");
    }
    setActions(card, []);
  }
}

function uploadErrorMessage(err) {
  if (!(err instanceof ApiError)) {
    return "Could not reach the server. Check your connection and try again.";
  }
  switch (err.code) {
    case "model_unavailable":
      // A configuration problem, not something wrong with the photo
      // (docs/specs/06-vision-shelf-ingestion.md).
      return "Photo analysis is not available right now: the configured AI model is unavailable. An admin needs to choose another one.";
    case "payload_too_large":
      return "This photo is too large to upload.";
    default:
      return err.message || "The photo could not be uploaded.";
  }
}

function reviewHref(jobId) {
  const url = new URL(withStorageParam(storageId, "/review.html"), location.origin);
  url.searchParams.set("job", jobId);
  return url.pathname + url.search;
}

function addUploadCard(name) {
  const card = fromTemplate(uploadTemplate);
  qs('[data-role="name"]', card).textContent = name;
  uploadsList.prepend(card);
  return card;
}

function setStatus(card, status, detail) {
  qs('[data-role="status"]', card).textContent = status;
  qs('[data-role="detail"]', card).textContent = detail;
}

function setActions(card, nodes) {
  qs('[data-role="actions"]', card).replaceChildren(...nodes);
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
