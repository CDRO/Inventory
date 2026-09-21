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
const passwordForm = qs("#password-form");
const currentPasswordInput = qs("#current-password");
const newPasswordInput = qs("#new-password");
const repeatPasswordInput = qs("#repeat-password");
const passwordError = qs("#password-error");
const passwordStatus = qs("#password-status");
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

  await Promise.all([loadPreferences(), loadStorageSettings()]);
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
