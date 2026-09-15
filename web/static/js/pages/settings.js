import "../register-sw.js";

// Page module for settings.html — the one user-facing settings surface for
// gamification (docs/specs/52-gamification-quests-and-ui.md's fourth UI
// integration point): the user's own gamification_enabled toggle and holiday
// weeks, plus the currently selected storage's flat toggles. Never suggested,
// prompted, or advertised elsewhere — it exists here for whoever needs it.

import { fetchMe, resolveStorage, rememberStorageId, withStorageParam } from "../session.js";
import { renderStorageSwitcher } from "../storage-switcher.js";
import { get, put, ApiError } from "../api.js";
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
