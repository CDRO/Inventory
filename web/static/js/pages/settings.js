import "../register-sw.js";

// Page module for settings.html — the one user-facing settings surface.
//
// Two specs share it. Account self-service
// (docs/specs/14-account-self-service.md): display name, password change,
// and the list of signed-in devices. Gamification
// (docs/specs/52-gamification-quests-and-ui.md's fourth UI integration
// point): the user's own gamification_enabled toggle and holiday weeks, plus
// the currently selected storage's flat toggles.
//
// **Nothing here branches on admin status**, because no response this page
// reads carries any (docs/specs/03-auth-and-multi-tenancy.md). The admin
// password reset is in the server-rendered admin area, and this file has no
// way to know whether the caller can open it — nor, deliberately, does it
// name the path: nothing shipped under web/static/ does (web/embed_test.go).

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { get, put, post, patch, del, ApiError } from "../api.js";
import { el, text, clearChildren, qs } from "../dom.js";

const switcherContainer = qs("#storage-switcher");
const errorBox = qs("#error");
const gamificationEnabled = qs("#gamification-enabled");
const holidayWeeksList = qs("#holiday-weeks");
const holidayWeekInput = qs("#holiday-week-input");
const holidayAddButton = qs("#holiday-add");
const leaderboardEnabled = qs("#leaderboard-enabled");
const weeklyGoalInput = qs("#weekly-goal");
const storageSaveButton = qs("#storage-save");
const resetLocalDataButton = qs("#reset-local-data");
const resetStatus = qs("#reset-status");
const displayNameInput = qs("#display-name");
const displayNameSaveButton = qs("#display-name-save");
const displayNameStatus = qs("#display-name-status");
const barcodePromptEnabled = qs("#barcode-prompt-enabled");
const barcodePromptStatus = qs("#barcode-prompt-status");
const passwordForm = qs("#password-form");
const currentPasswordInput = qs("#current-password");
const newPasswordInput = qs("#new-password");
const repeatPasswordInput = qs("#repeat-password");
const passwordError = qs("#password-error");
const passwordStatus = qs("#password-status");
const notificationsSection = qs("#notifications-section");
const notifyEnabled = qs("#notify-enabled");
const notifyKind = qs("#notify-kind");
const notifyUrl = qs("#notify-url");
const notifyUrlHint = qs("#notify-url-hint");
const notifyToken = qs("#notify-token");
const notifyTokenRemoveRow = qs("#notify-token-remove-row");
const notifyTokenRemove = qs("#notify-token-remove");
const notifyHour = qs("#notify-hour");
const notifyIncludeSoon = qs("#notify-include-soon");
const notifyLastResult = qs("#notify-last-result");
const notifySaveButton = qs("#notify-save");
const notifyTestButton = qs("#notify-test");
const notifyTestStatus = qs("#notify-test-status");
const devicesList = qs("#devices");

let storageId = null;
/** @type {string[]} every holiday week the caller currently has on record, "YYYY-MM-DD". */
let currentHolidayWeeks = [];

init();

async function init() {
  let me;
  try {
    me = await fetchMe();
  } catch (err) {
    showError(err);
    return;
  }

  // The account section works whether or not the caller has a storage yet,
  // so it is filled before the storage resolution below can navigate away.
  displayNameInput.value = me.display_name;
  displayNameSaveButton.addEventListener("click", saveDisplayName);
  passwordForm.addEventListener("submit", changePassword);
  loadDevices();
  loadBarcodePrompt();

  const resolved = resolveStorage(me.storages);
  if (resolved == null) {
    location.assign("/storages.html");
    return;
  }
  storageId = resolved;
  rememberStorageId(storageId);
  renderStorageSwitcher(switcherContainer, { storages: me.storages, currentId: storageId });

  gamificationEnabled.addEventListener("change", updateGamificationEnabled);
  holidayAddButton.addEventListener("click", addHolidayWeek);
  storageSaveButton.addEventListener("click", saveStorageSettings);
  resetLocalDataButton.addEventListener("click", resetLocalAppData);
  notifySaveButton.addEventListener("click", saveNotificationSettings);
  notifyTestButton.addEventListener("click", sendTestNotification);
  notifyKind.addEventListener("change", renderNotifyUrlHint);
  // Only now that a storage is resolved: the configuration is per storage
  // (docs/specs/17-expiry-notifications.md).
  notificationsSection.hidden = false;

  await Promise.all([loadPreferences(), loadStorageSettings(), loadNotificationSettings()]);
}

async function loadPreferences() {
  try {
    const prefs = await get("/api/me/preferences");
    gamificationEnabled.checked = prefs.gamification_enabled;
    currentHolidayWeeks = prefs.holiday_weeks;
    renderHolidayWeeks();
  } catch (err) {
    showError(err);
  }
}

function renderHolidayWeeks() {
  clearChildren(holidayWeeksList);
  if (currentHolidayWeeks.length === 0) {
    holidayWeeksList.append(el("p", { class: "empty-state" }, [text("No holiday weeks marked.")]));
    return;
  }

  const today = mondayOf(new Date());
  for (const week of [...currentHolidayWeeks].sort()) {
    const isFuture = week > today;
    const row = el("div", { class: "row row--between" }, [el("span", {}, [text(week)])]);
    if (isFuture) {
      row.append(
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => removeHolidayWeek(week) }, [
          text("Un-mark"),
        ]),
      );
    } else {
      row.append(el("span", { class: "muted" }, [text("Already in effect")]));
    }
    holidayWeeksList.append(row);
  }
}

// mondayOf mirrors the server's own week boundary (Monday-start, UTC) so the
// "already in effect" split matches what the backend will actually accept.
function mondayOf(date) {
  const utc = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate()));
  const offset = (utc.getUTCDay() + 6) % 7;
  utc.setUTCDate(utc.getUTCDate() - offset);
  return utc.toISOString().slice(0, 10);
}

async function updateGamificationEnabled() {
  clearError();
  try {
    await put("/api/me/preferences", {
      gamification_enabled: gamificationEnabled.checked,
      holiday_weeks: currentHolidayWeeks,
    });
  } catch (err) {
    gamificationEnabled.checked = !gamificationEnabled.checked; // revert on failure
    showError(err);
  }
}

async function addHolidayWeek() {
  clearError();
  const week = holidayWeekInput.value;
  if (!week) {
    showError(new Error("Pick a week first."));
    return;
  }

  const next = [...currentHolidayWeeks, week];
  try {
    const prefs = await put("/api/me/preferences", {
      gamification_enabled: gamificationEnabled.checked,
      holiday_weeks: next,
    });
    currentHolidayWeeks = prefs.holiday_weeks;
    holidayWeekInput.value = "";
    renderHolidayWeeks();
  } catch (err) {
    showError(err);
  }
}

async function removeHolidayWeek(week) {
  clearError();
  const next = currentHolidayWeeks.filter((w) => w !== week);
  try {
    const prefs = await put("/api/me/preferences", {
      gamification_enabled: gamificationEnabled.checked,
      holiday_weeks: next,
    });
    currentHolidayWeeks = prefs.holiday_weeks;
    renderHolidayWeeks();
  } catch (err) {
    showError(err);
  }
}

// --- The capture-time barcode offer (docs/specs/20-barcode-recall.md) ---
//
// A per-user preference, not a per-storage one, and deliberately not a field
// on PATCH /api/auth/me: that route's contract is display name and nothing
// else, and it refuses an unknown field outright.
//
// The checkbox is left as the markup renders it until the server answers, so a
// failed read shows no state rather than a wrong one.

async function loadBarcodePrompt() {
  try {
    const state = await get("/api/auth/barcode-prompt");
    barcodePromptEnabled.checked = Boolean(state.enabled);
    barcodePromptEnabled.addEventListener("change", saveBarcodePrompt);
  } catch {
    // Reading a preference is not worth an error banner on a page full of
    // other working sections; the checkbox simply stays inert.
    barcodePromptEnabled.disabled = true;
  }
}

async function saveBarcodePrompt() {
  barcodePromptStatus.hidden = true;
  barcodePromptEnabled.disabled = true;
  try {
    const state = await patch("/api/auth/barcode-prompt", { enabled: barcodePromptEnabled.checked });
    barcodePromptEnabled.checked = Boolean(state.enabled);
    barcodePromptStatus.textContent = "Saved.";
    barcodePromptStatus.hidden = false;
  } catch (err) {
    // Put the checkbox back where the server still has it, so the screen never
    // claims a preference that was not stored.
    barcodePromptEnabled.checked = !barcodePromptEnabled.checked;
    showError(err);
  } finally {
    barcodePromptEnabled.disabled = false;
  }
}

// --- Account (docs/specs/14-account-self-service.md) ---

async function saveDisplayName() {
  clearError();
  displayNameStatus.hidden = true;
  displayNameSaveButton.disabled = true;
  try {
    const me = await patch("/api/auth/me", { display_name: displayNameInput.value });
    displayNameInput.value = me.display_name;
    displayNameStatus.textContent = "Saved.";
    displayNameStatus.hidden = false;
  } catch (err) {
    showError(err);
  } finally {
    displayNameSaveButton.disabled = false;
  }
}

// changePassword submits the form and, on success, tells the user what it
// just did to their other sessions.
//
// The repeat field is checked here and nowhere else: it is courtesy, to catch
// a typo before it becomes a password nobody knows. The server never sees it
// and has no opinion about it, which is why a mismatch is reported locally
// rather than sent off to be refused.
async function changePassword(event) {
  event.preventDefault();
  clearError();
  passwordError.hidden = true;
  passwordStatus.hidden = true;

  if (newPasswordInput.value !== repeatPasswordInput.value) {
    passwordError.textContent = "The two new passwords do not match.";
    passwordError.hidden = false;
    return;
  }

  const submit = passwordForm.querySelector("button[type=submit]");
  submit.disabled = true;
  try {
    await post("/api/auth/password", {
      current_password: currentPasswordInput.value,
      new_password: newPasswordInput.value,
    });
    passwordForm.reset();
    passwordStatus.textContent =
      "Password changed. Every other browser and paired device has been signed out.";
    passwordStatus.hidden = false;
    // The device list just lost every row but this one.
    await loadDevices();
  } catch (err) {
    passwordError.textContent = passwordMessage(err);
    passwordError.hidden = false;
  } finally {
    submit.disabled = false;
  }
}

// passwordMessage turns an error envelope into one line next to the form.
//
// A wrong current password comes back as a 422 with the message under
// `fields.current_password` rather than as a 401, precisely so the browser
// stays signed in and the message can be shown here
// (docs/specs/14-account-self-service.md).
function passwordMessage(err) {
  if (!(err instanceof ApiError)) {
    return err.message || "Something went wrong.";
  }
  const details = Object.values(err.fields || {}).flat();
  return details.length ? details.join(" ") : err.message;
}

async function loadDevices() {
  try {
    const devices = await get("/api/auth/devices");
    renderDevices(devices.items);
  } catch (err) {
    showError(err);
  }
}

function renderDevices(devices) {
  clearChildren(devicesList);
  if (devices.length === 0) {
    devicesList.append(el("p", { class: "empty-state" }, [text("No signed-in devices.")]));
    return;
  }

  for (const device of devices) {
    const row = el("div", { class: "row row--between" }, [
      el("span", {}, [text(describeDevice(device))]),
    ]);
    if (device.current) {
      row.append(el("span", { class: "muted" }, [text("This device")]));
    } else {
      row.append(
        el("button", { type: "button", class: "btn btn--ghost", onclick: () => revokeDevice(device.id) }, [
          text("Sign out"),
        ]),
      );
    }
    devicesList.append(row);
  }
}

function describeDevice(device) {
  const name = device.label || (device.kind === "device" ? "Paired device" : "Browser");
  const seen = device.last_seen_at ? device.last_seen_at.slice(0, 10) : "not since signing in";
  return `${name} — last active ${seen}`;
}

async function revokeDevice(id) {
  clearError();
  try {
    await del(`/api/auth/devices/${encodeURIComponent(id)}`);
    await loadDevices();
  } catch (err) {
    showError(err);
  }
}

function storageBasePath() {
  return `/api/storages/${storageId}/gamification/settings`;
}

async function loadStorageSettings() {
  try {
    const settings = await get(storageBasePath());
    leaderboardEnabled.checked = settings.leaderboard_enabled;
    weeklyGoalInput.value = String(settings.weekly_goal_items);
  } catch (err) {
    showError(err);
  }
}

async function saveStorageSettings() {
  clearError();
  try {
    const settings = await put(storageBasePath(), {
      leaderboard_enabled: leaderboardEnabled.checked,
      weekly_goal_items: Number(weeklyGoalInput.value),
    });
    leaderboardEnabled.checked = settings.leaderboard_enabled;
    weeklyGoalInput.value = String(settings.weekly_goal_items);
  } catch (err) {
    showError(err);
  }
}

// --- Expiry notifications (docs/specs/17-expiry-notifications.md) ---
//
// Per storage, off by default, and about the food rather than about anyone's
// use of the app — which is why this does not conflict with the "no
// notifications" rules in specs 06 and 50.

function notificationsPath() {
  return `/api/storages/${storageId}/notification-settings`;
}

async function loadNotificationSettings() {
  try {
    renderNotificationSettings(await get(notificationsPath()));
  } catch (err) {
    showError(err);
  }
}

// renderNotificationSettings fills the form from the server's copy.
//
// The token field is always left empty: the API never returns a stored token,
// so there is nothing to put in it. What the form does show is *whether* one
// is saved, which is the only part a person needs in order to decide whether
// to replace or remove it.
function renderNotificationSettings(settings) {
  notifyEnabled.checked = settings.enabled;
  notifyKind.value = settings.kind;
  notifyUrl.value = settings.url;
  notifyHour.value = String(settings.send_hour);
  notifyIncludeSoon.checked = settings.include_soon;
  notifyToken.value = "";
  notifyTokenRemove.checked = false;
  notifyTokenRemoveRow.hidden = !settings.has_token;
  notifyToken.placeholder = settings.has_token ? "A token is saved" : "";
  renderNotifyUrlHint();
  renderLastResult(settings);
}

// renderLastResult shows how the last run went. A failure is only ever
// visible here and in the test button's reply, because there is no retry
// queue: the next day's run is the retry, and this line is how somebody
// notices that the digest has been failing quietly.
function renderLastResult(settings) {
  if (!settings.last_result) {
    notifyLastResult.hidden = true;
    return;
  }
  const when = settings.last_run_at ? new Date(settings.last_run_at).toLocaleString() : "";
  const outcome =
    settings.last_result === "sent"
      ? "Last digest sent"
      : settings.last_result === "empty"
        ? "Nothing to report on the last run"
        : `Last run failed: ${settings.last_result}`;
  notifyLastResult.textContent = when ? `${outcome} (${when}).` : `${outcome}.`;
  notifyLastResult.hidden = false;
}

function renderNotifyUrlHint() {
  const hints = {
    ntfy: "The full topic URL, e.g. https://ntfy.example/inventory.",
    gotify: "The Gotify server's base URL; /message is appended for you.",
    webhook: "Any URL that accepts a JSON POST.",
  };
  notifyUrlHint.textContent = hints[notifyKind.value] ?? "";
}

// saveNotificationSettings sends the form, expressing the three token states
// the API distinguishes: a typed value replaces the stored token, the
// explicit "remove" checkbox sends null to clear it, and leaving both alone
// omits the field so the stored one survives a save that was about the hour.
async function saveNotificationSettings() {
  clearError();
  notifyTestStatus.hidden = true;
  notifySaveButton.disabled = true;
  try {
    const body = {
      enabled: notifyEnabled.checked,
      kind: notifyKind.value,
      url: notifyUrl.value.trim(),
      send_hour: Number(notifyHour.value),
      include_soon: notifyIncludeSoon.checked,
    };
    if (notifyToken.value !== "") {
      body.token = notifyToken.value;
    } else if (notifyTokenRemove.checked) {
      body.token = null;
    }
    renderNotificationSettings(await put(notificationsPath(), body));
  } catch (err) {
    showError(err);
  } finally {
    notifySaveButton.disabled = false;
  }
}

// sendTestNotification is the debugging tool the spec puts in place of a
// retry queue: a typo in the URL is found now rather than days later, when a
// digest quietly never arrived.
async function sendTestNotification() {
  clearError();
  notifyTestButton.disabled = true;
  notifyTestStatus.hidden = false;
  notifyTestStatus.textContent = "Sending…";
  try {
    const outcome = await post(`${notificationsPath()}/test`, {});
    notifyTestStatus.textContent = outcome.ok ? "Sent. Check your notifications." : `Failed: ${outcome.result}`;
    await loadNotificationSettings();
  } catch (err) {
    notifyTestStatus.hidden = true;
    showError(err);
  } finally {
    notifyTestButton.disabled = false;
  }
}

// resetLocalAppData is the manual escape hatch
// (docs/specs/05-frontend-pwa-foundations.md) for whatever sw.js's own
// version-bump cleanup does not anticipate — a browser stuck on a stale
// service worker or a stale cache entry, with no way back short of DevTools.
// It unregisters every registration and clears every Cache Storage entry for
// this origin, then reloads so the next request for everything, including
// this very page, goes to the network fresh.
async function resetLocalAppData() {
  resetLocalDataButton.disabled = true;
  resetStatus.hidden = false;
  resetStatus.textContent = "Clearing local data…";
  try {
    if ("serviceWorker" in navigator) {
      const registrations = await navigator.serviceWorker.getRegistrations();
      await Promise.all(registrations.map((r) => r.unregister()));
    }
    if ("caches" in window) {
      const names = await caches.keys();
      await Promise.all(names.map((n) => caches.delete(n)));
    }
    resetStatus.textContent = "Cleared. Reloading…";
    location.reload();
  } catch (err) {
    resetLocalDataButton.disabled = false;
    resetStatus.hidden = true;
    showError(err instanceof Error ? err : new Error("Could not clear local data."));
  }
}

function showError(err) {
  errorBox.textContent = err instanceof ApiError ? err.message : err.message || "Something went wrong.";
  errorBox.hidden = false;
}

function clearError() {
  errorBox.textContent = "";
  errorBox.hidden = true;
}
