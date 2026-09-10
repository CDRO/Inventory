import "../register-sw.js";

// Page module for index.html — the login screen
// (docs/specs/05-frontend-pwa-foundations.md).
//
// This page deliberately never calls GET /api/auth/me on load. That call is
// how a storage-scoped page resolves the current user, and an unauthenticated
// visitor here would get an entirely expected 401 back — not a reason to
// bounce the login page off of itself. The only network call this page makes
// is the login submission.

import { post, ApiError } from "../api.js";

const form = document.querySelector("#login-form");
const errorBox = document.querySelector("#login-error");
const submitButton = form.querySelector('button[type="submit"]');

form.addEventListener("submit", handleSubmit);

async function handleSubmit(event) {
  event.preventDefault();
  hideError();
  setBusy(true);

  const data = new FormData(form);
  const username = String(data.get("username") || "").trim();
  const password = String(data.get("password") || "");

  try {
    // skipAuthRedirect: a wrong password is a 401 this form must display, not
    // a dead session api.js should bounce to /index.html — which is this
    // page (docs/specs/03-auth-and-multi-tenancy.md defines the endpoint;
    // its route lands with the spec 03 HTTP surface work).
    await post("/api/auth/login", { username, password }, { skipAuthRedirect: true });
    location.assign("/storages.html");
  } catch (err) {
    setBusy(false);
    if (err instanceof ApiError) {
      showError(
        err.status === 401
          ? "Incorrect username or password."
          : err.message || "Something went wrong. Try again.",
      );
    } else {
      showError("Could not reach the server. Check your connection and try again.");
    }
  }
}

function showError(message) {
  errorBox.textContent = message; // never innerHTML — see js/dom.js
  errorBox.hidden = false;
}

function hideError() {
  errorBox.hidden = true;
  errorBox.textContent = "";
}

function setBusy(busy) {
  submitButton.disabled = busy;
  submitButton.textContent = busy ? "Signing in…" : "Sign in";
}
