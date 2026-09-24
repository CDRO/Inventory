// Localization (docs/specs/19-localization.md). Every page module imports
// this for its side effect (loading the active catalog and translating every
// `data-i18n` element already on the page) before it renders anything of its
// own, the same way every page already imports "./register-sw.js" first.
//
// Translated strings are written with `textContent`/`setAttribute` only,
// never `innerHTML` — the injection rule from
// docs/specs/05-frontend-pwa-foundations.md applies to catalog strings
// exactly as it does to user- and AI-supplied text.

const SUPPORTED_LANGUAGES = ["en", "de"];
const STORAGE_KEY = "inventory.language";

/**
 * resolveLanguage implements the order from the spec: a `localStorage`
 * override set on settings.html, then the first of `navigator.languages`
 * with a catalog, then "en".
 *
 * @returns {string}
 */
function resolveLanguage() {
  let override = null;
  try {
    override = localStorage.getItem(STORAGE_KEY);
  } catch {
    // Private browsing / blocked storage: fall through to browser detection.
  }
  if (override && SUPPORTED_LANGUAGES.includes(override)) {
    return override;
  }

  for (const candidate of navigator.languages || [navigator.language]) {
    if (!candidate) continue;
    const base = candidate.split("-")[0].toLowerCase();
    if (SUPPORTED_LANGUAGES.includes(base)) {
      return base;
    }
  }

  return "en";
}

/**
 * @param {string} lang
 * @returns {Promise<Object<string, string>>}
 */
async function loadCatalog(lang) {
  const response = await fetch(`/i18n/${lang}.json`);
  if (!response.ok) {
    throw new Error(`i18n: failed to load /i18n/${lang}.json (${response.status})`);
  }
  return response.json();
}

const language = resolveLanguage();

// The "en" catalog is always loaded, even when it is also the active
// language, so it can serve as the single fallback source for a key missing
// from the active catalog (the spec's fallback rule) without a second
// network round trip on demand.
const [activeCatalog, enCatalog] = await Promise.all([
  language === "en" ? loadCatalog("en") : loadCatalog(language),
  language === "en" ? Promise.resolve(null) : loadCatalog("en"),
]);

document.documentElement.lang = language;

/**
 * t(key, params) resolves a catalog key to a string, interpolating
 * `{placeholder}` tokens from `params`. A key missing from the active
 * catalog falls back to the `en` string; a key missing from both renders the
 * key itself, deliberately ugly so it gets noticed and fixed.
 *
 * @param {string} key
 * @param {Object<string, string|number>} [params]
 * @returns {string}
 */
export function t(key, params = {}) {
  const template = activeCatalog[key] ?? enCatalog?.[key] ?? key;
  return template.replace(/\{(\w+)\}/g, (match, name) =>
    Object.prototype.hasOwnProperty.call(params, name) ? String(params[name]) : match,
  );
}

/**
 * tCount(key, count, params) resolves the `.one`/`.other` plural variant of
 * a key for English and German — the spec's whole-sentence-per-key rule for
 * countable phrases (`"inbox.waiting.one"` / `"inbox.waiting.other"`).
 *
 * @param {string} key - the key without its `.one`/`.other` suffix.
 * @param {number} count
 * @param {Object<string, string|number>} [params]
 * @returns {string}
 */
export function tCount(key, count, params = {}) {
  const suffix = count === 1 ? "one" : "other";
  return t(`${key}.${suffix}`, { count, ...params });
}

/** @returns {string} the resolved active language ("en" or "de"). */
export function getLanguage() {
  return language;
}

/**
 * getLanguageOverride reads the raw `localStorage` override, distinct from
 * `getLanguage()`'s resolved value — settings.html's `<select>` needs to
 * show "matches my browser" rather than whatever language that happens to
 * resolve to today.
 *
 * @returns {string|null} "en", "de", or null when there is no override.
 */
export function getLanguageOverride() {
  try {
    const override = localStorage.getItem(STORAGE_KEY);
    return SUPPORTED_LANGUAGES.includes(override) ? override : null;
  } catch {
    return null;
  }
}

/**
 * setLanguage persists an override for the next page load. It does not
 * itself re-render the current page — the catalog for the new language has
 * not even been fetched — callers reload after calling this.
 *
 * @param {string|null} lang - "en", "de", or null to clear the override and
 *   fall back to browser-language detection.
 */
export function setLanguage(lang) {
  if (lang == null) {
    localStorage.removeItem(STORAGE_KEY);
  } else {
    localStorage.setItem(STORAGE_KEY, lang);
  }
}

/**
 * apiErrorMessage(err) renders an ApiError (js/api.js) through
 * `t("error." + code)`, falling back to the server's own `message` for a
 * code with no translation — the boundary docs/specs/19-localization.md
 * draws between the API (always English) and the UI.
 *
 * @param {{code?: string, message?: string}} err
 * @returns {string}
 */
export function apiErrorMessage(err) {
  const code = err && err.code;
  if (!code) {
    return (err && err.message) || t("error.internal_error");
  }
  const key = `error.${code}`;
  if (Object.prototype.hasOwnProperty.call(activeCatalog, key) || (enCatalog && Object.prototype.hasOwnProperty.call(enCatalog, key))) {
    return t(key);
  }
  return err.message || t("error.internal_error");
}

/**
 * formatDate(date, options) formats a Date via Intl in the active language —
 * "de" renders German conventions, everything else renders as the browser's
 * "en" defaults, per the spec's `Intl.DateTimeFormat` requirement.
 *
 * @param {Date} date
 * @param {Intl.DateTimeFormatOptions} [options]
 * @returns {string}
 */
export function formatDate(date, options) {
  return new Intl.DateTimeFormat(language, options).format(date);
}

/**
 * formatNumber(value, options) formats a number via Intl in the active
 * language (e.g. German decimal comma vs. English decimal point).
 *
 * @param {number} value
 * @param {Intl.NumberFormatOptions} [options]
 * @returns {string}
 */
export function formatNumber(value, options) {
  return new Intl.NumberFormat(language, options).format(value);
}

/**
 * applyI18n fills every `data-i18n*` element under `root` (default: the
 * whole document) from the loaded catalog: `data-i18n` (textContent),
 * `data-i18n-placeholder`, `data-i18n-title`, `data-i18n-aria-label`.
 *
 * Called once automatically below for the document at load. A `<template>`'s
 * content is not part of the live document until cloned (`dom.js`'s
 * `fromTemplate`), so that first pass never reaches it — any page or module
 * that clones a template containing `data-i18n*` markup must call
 * `applyI18n(clonedRoot)` itself right after cloning, before filling in the
 * row's own dynamic (non-translated) data.
 *
 * `root` itself is checked too, not just its descendants, so a call with a
 * single already-cloned element (rather than a container) still translates
 * that element if it carries a `data-i18n*` attribute directly.
 *
 * @param {ParentNode & Element} [root]
 */
export function applyI18n(root = document) {
  const matches = (selector) => {
    const found = root.matches?.(selector) ? [root] : [];
    return found.concat(Array.from(root.querySelectorAll(selector)));
  };

  for (const el of matches("[data-i18n]")) {
    el.textContent = t(el.getAttribute("data-i18n"));
  }
  for (const el of matches("[data-i18n-placeholder]")) {
    el.setAttribute("placeholder", t(el.getAttribute("data-i18n-placeholder")));
  }
  for (const el of matches("[data-i18n-title]")) {
    el.setAttribute("title", t(el.getAttribute("data-i18n-title")));
  }
  for (const el of matches("[data-i18n-aria-label]")) {
    el.setAttribute("aria-label", t(el.getAttribute("data-i18n-aria-label")));
  }
}

applyI18n();
