// How long ago a location was last walked, in words
// (docs/specs/13-stocktake-and-audit.md).
//
// Shared by the locations tree, which shows it per node so staleness is
// visible where the tree is edited, and by the stocktake sheet, which shows it
// for the one location being walked. One module rather than two copies,
// because "never audited" and "audited 3 weeks ago" have to read identically
// in both places — a node that says one thing in the tree and another on its
// own page is worse than either.

import { t, getLanguage } from "./i18n.js";

const relative = new Intl.RelativeTimeFormat(getLanguage(), { numeric: "auto" });

// Largest first: the walk below picks the first unit the gap actually fills,
// so five days reads as "5 days ago" rather than "0 weeks ago".
const UNITS = [
  ["year", 365 * 24 * 60 * 60 * 1000],
  ["month", 30 * 24 * 60 * 60 * 1000],
  ["week", 7 * 24 * 60 * 60 * 1000],
  ["day", 24 * 60 * 60 * 1000],
  ["hour", 60 * 60 * 1000],
  ["minute", 60 * 1000],
];

/**
 * formatAudited renders a location's last_audited_at as a phrase.
 *
 * null — which is what every location that has never been walked carries —
 * is a real answer, not a missing value, so it gets words of its own rather
 * than an empty string. That is the whole feature: a shelf nobody has ever
 * checked should say so.
 *
 * @param {string|null|undefined} timestamp - RFC 3339, as the API sends it.
 * @returns {string}
 */
export function formatAudited(timestamp) {
  if (!timestamp) return t("audited.never");

  const then = new Date(timestamp);
  if (Number.isNaN(then.getTime())) return t("audited.never");

  const elapsed = Date.now() - then.getTime();
  // A clock that is behind the server's would otherwise produce "in 2
  // minutes", which reads as a bug to anyone who sees it.
  if (elapsed < 60 * 1000) return t("audited.justNow");

  for (const [unit, ms] of UNITS) {
    const value = Math.floor(elapsed / ms);
    if (value >= 1) return t("audited.phrase", { relative: relative.format(-value, unit) });
  }
  return t("audited.justNow");
}
