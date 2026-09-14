// Gamification UI integration (docs/specs/52-gamification-quests-and-ui.md):
// the header ring, the dashboard card, and the toast helper other pages call
// after a confirm action.
//
// gamification_enabled = FALSE is signaled by every gamification route
// answering 204 with no body (docs/specs/51-gamification-scoring.md). This
// module fetches GET .../progress first and only that; every other
// gamification request (quests, achievements) is made only once progress
// comes back non-null. A disabled caller therefore triggers exactly one
// request, gets nothing back, and this module renders nothing at all — the
// dashboard reflows without a gap, since the containers are simply left
// `hidden` rather than replaced with a placeholder.
//
// Rings and bars are hand-written inline SVG/CSS; this file does not import a
// charting or animation library. Every value that came from the server is
// still written through textContent (via dom.js's text()/el()), not because
// XP numbers are untrusted exactly, but because "always textContent" is a
// rule a reviewer should never have to make an exception to.

import { get } from "./api.js";
import { el, text, clearChildren } from "./dom.js";
import { withStorageParam } from "./session.js";

const RING_SIZE = 40;
const RING_STROKE = 4;
const LONG_PRESS_MS = 500;

/**
 * initGamification wires up whichever of the page's gamification containers
 * are present: `#gamification-ring` (every storage-scoped page) and
 * `#gamification-card` (dashboard.html only). Pages that have neither do
 * nothing and make no request at all.
 *
 * @param {string} storageId
 */
export async function initGamification(storageId) {
  const ringContainer = document.querySelector("#gamification-ring");
  const cardContainer = document.querySelector("#gamification-card");
  if (!ringContainer && !cardContainer) return;

  let progress;
  try {
    progress = await get(`/api/storages/${storageId}/progress`);
  } catch {
    // A failed request is not the same as "disabled" — leave the containers
    // as they started (hidden), the same as any other best-effort widget.
    return;
  }

  if (progress == null) {
    if (ringContainer) ringContainer.hidden = true;
    if (cardContainer) cardContainer.hidden = true;
    return;
  }

  if (ringContainer) renderRing(ringContainer, storageId, progress);
  if (cardContainer) await renderCard(cardContainer, storageId, progress);
}

function progressPercent(progress) {
  const span = progress.xp_for_next_level - progress.xp_for_level;
  if (span <= 0) return 0;
  return Math.min(1, Math.max(0, (progress.xp - progress.xp_for_level) / span));
}

function renderRing(container, storageId, progress) {
  clearChildren(container);
  container.hidden = false;

  const link = el(
    "a",
    { class: "gami-ring", href: withStorageParam(storageId, "/dashboard.html"), "aria-label": `Level ${progress.level}` },
    [ringSvg(progressPercent(progress)), el("span", { class: "gami-ring__level" }, [text(String(progress.level))])],
  );

  const inLevel = progress.xp - progress.xp_for_level;
  const levelSpan = progress.xp_for_next_level - progress.xp_for_level;
  const popover = el("div", { class: "gami-popover", role: "status", hidden: true }, [
    text(`Progress to level ${progress.level + 1}: ${inLevel}/${levelSpan} XP`),
  ]);

  wirePopover(link, popover);
  container.append(el("div", { class: "gami-ring-wrapper" }, [link, popover]));
}

// wirePopover shows the exact-numbers popover three ways in — long press on
// touch, hover on pointer devices, focus for keyboard users
// (docs/specs/52-gamification-quests-and-ui.md) — so the information is not
// touch-only, and a long press must not also fire the ring's navigation.
function wirePopover(link, popover) {
  const show = () => {
    popover.hidden = false;
  };
  const hide = () => {
    popover.hidden = true;
  };

  link.addEventListener("mouseenter", show);
  link.addEventListener("mouseleave", hide);
  link.addEventListener("focus", show);
  link.addEventListener("blur", hide);

  let pressTimer = null;
  let longPressed = false;
  link.addEventListener(
    "touchstart",
    () => {
      longPressed = false;
      pressTimer = setTimeout(() => {
        longPressed = true;
        show();
      }, LONG_PRESS_MS);
    },
    { passive: true },
  );
  link.addEventListener("touchend", (event) => {
    clearTimeout(pressTimer);
    if (longPressed) {
      event.preventDefault(); // the long press revealed the popover; releasing must not also navigate
      setTimeout(hide, 2000);
    }
  });
  link.addEventListener("touchcancel", () => clearTimeout(pressTimer));
}

function ringSvg(pct) {
  const svgNS = "http://www.w3.org/2000/svg";
  const r = (RING_SIZE - RING_STROKE) / 2;
  const circumference = 2 * Math.PI * r;
  const offset = circumference * (1 - pct);
  const center = RING_SIZE / 2;

  const svg = document.createElementNS(svgNS, "svg");
  svg.setAttribute("width", String(RING_SIZE));
  svg.setAttribute("height", String(RING_SIZE));
  svg.setAttribute("viewBox", `0 0 ${RING_SIZE} ${RING_SIZE}`);
  svg.setAttribute("class", "gami-ring__svg");
  svg.setAttribute("aria-hidden", "true");

  const track = document.createElementNS(svgNS, "circle");
  track.setAttribute("cx", String(center));
  track.setAttribute("cy", String(center));
  track.setAttribute("r", String(r));
  track.setAttribute("class", "gami-ring__track");
  track.setAttribute("stroke-width", String(RING_STROKE));
  svg.append(track);

  const arc = document.createElementNS(svgNS, "circle");
  arc.setAttribute("cx", String(center));
  arc.setAttribute("cy", String(center));
  arc.setAttribute("r", String(r));
  arc.setAttribute("class", "gami-ring__arc");
  arc.setAttribute("stroke-width", String(RING_STROKE));
  arc.setAttribute("stroke-dasharray", String(circumference));
  arc.setAttribute("stroke-dashoffset", String(offset));
  arc.setAttribute("transform", `rotate(-90 ${center} ${center})`);
  svg.append(arc);

  return svg;
}

async function renderCard(container, storageId, progress) {
  clearChildren(container);
  container.hidden = false;

  let quests, achievements;
  try {
    [quests, achievements] = await Promise.all([
      get(`/api/storages/${storageId}/quests`),
      get(`/api/storages/${storageId}/achievements`),
    ]);
  } catch {
    // The ring above already rendered from the progress call that did
    // succeed; the richer card is a best-effort widget on top of it.
    container.append(el("p", { class: "empty-state" }, [text("Progress is unavailable right now.")]));
    return;
  }

  const sections = [
    el("div", { class: "gami-card__header" }, [
      ringSvg(progressPercent(progress)),
      el("div", {}, [
        el("strong", {}, [text(`Level ${progress.level}`)]),
        el("p", { class: "muted" }, [text(`${progress.xp} XP total`)]),
      ]),
    ]),
    barRow("Storage health", `${Math.round(progress.health_score)}%`, progress.health_score / 100),
  ];

  if (quests) {
    sections.push(
      barRow(
        "This week's goal",
        `${quests.weekly_goal.current} of ${quests.weekly_goal.target}`,
        quests.weekly_goal.target > 0 ? quests.weekly_goal.current / quests.weekly_goal.target : 0,
      ),
    );
    sections.push(renderQuestsSection(quests));
    sections.push(streakLine(progress.streak_weeks));
  }

  if (achievements && achievements.items.length > 0) {
    sections.push(renderRecentAchievements(achievements.items));
  }

  container.append(...sections);
}

function barRow(label, valueText, fraction) {
  const pct = Math.round(Math.min(1, Math.max(0, fraction)) * 100);
  return el("div", { class: "stack" }, [
    el("div", { class: "row row--between" }, [el("span", {}, [text(label)]), el("span", { class: "muted" }, [text(valueText)])]),
    el("div", { class: "bar-row__track" }, [el("span", { class: "bar-row__fill", style: `width: ${pct}%` })]),
  ]);
}

function streakLine(streakWeeks) {
  const message = streakWeeks > 0 ? `${streakWeeks}-week streak` : "No active streak yet";
  return el("p", { class: "muted" }, [text(message)]);
}

// renderQuestsSection is the all-clear read when there are none — never an
// empty list, a "no quests available" notice, or anything that looks broken
// (docs/specs/52-gamification-quests-and-ui.md) — or the list of this
// week's quests with progress otherwise.
function renderQuestsSection(quests) {
  if (quests.all_clear) {
    const streakText =
      quests.clean_streak_days > 0 ? ` ${Math.floor(quests.clean_streak_days / 7)} weeks running.` : "";
    return el("div", { class: "card stack" }, [
      el("strong", {}, [text("Everything's in order.")]),
      el("p", { class: "muted" }, [text(streakText.trim())]),
    ]);
  }

  const rows = quests.quests.map((quest) => {
    const done = quest.completed_at != null;
    return el("div", { class: "row row--between" }, [
      el("span", { class: done ? "muted" : "" }, [text(questText(quest))]),
      el("span", { class: "muted" }, [text(done ? "Done" : `${quest.progress}/${quest.target_count}`)]),
    ]);
  });
  return el("div", { class: "stack" }, rows);
}

// questText builds the rendered line from generator + params, per
// docs/specs/52-gamification-quests-and-ui.md's table — "wording can change
// without a migration" is exactly what keeping this in the frontend buys.
function questText(quest) {
  const params = quest.params || {};
  switch (quest.generator) {
    case "stale_location": {
      const since = params.since_month ? ` — untouched since ${params.since_month}` : "";
      return `Re-scan ${params.location_name}${since}`;
    }
    case "missing_expiry":
      return `Add expiry dates to ${quest.target_count} fresh items`;
    case "uncategorized":
      return `Sort ${quest.target_count} products into categories`;
    case "imageless":
      return `Give ${quest.target_count} products a picture`;
    case "untracked_reorder":
      return `Set minimum stock for ${quest.target_count} staples`;
    case "expiring_soon":
      return `Use up or discard ${quest.target_count} items expiring this week`;
    case "consumption_hygiene":
      return "Log something you've used up";
    case "first_mile":
      return "Map your first shelf";
    default:
      return "";
  }
}

// renderRecentAchievements shows only what unlocked in the last 7 days
// ("Recently unlocked achievements appear here for a week",
// docs/specs/52-gamification-quests-and-ui.md). Hidden (immaculate_*)
// achievements are not filtered out here specially: once unlocked they are
// exactly as visible as any other.
function renderRecentAchievements(items) {
  const cutoff = Date.now() - 7 * 24 * 60 * 60 * 1000;
  const recent = items.filter((a) => new Date(a.unlocked_at).getTime() >= cutoff);
  if (recent.length === 0) return el("div", {});

  return el("div", { class: "stack" }, [
    el("strong", {}, [text("Recently unlocked")]),
    el(
      "div",
      { class: "row" },
      recent.map((a) => el("span", { class: "badge" }, [text(achievementLabel(a.key))])),
    ),
  ]);
}

// achievementLabel turns a snake_case key into the display label — plain
// text substitution, not a lookup into anything user-supplied.
function achievementLabel(key) {
  return key.replace(/_/g, " ");
}
