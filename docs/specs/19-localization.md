# 19 — Localization (i18n)

Depends on: [`04-backend-api-conventions.md`](04-backend-api-conventions.md)
(error codes as the machine contract),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(no-toolchain constraint, `dom.js` conventions).

## Why this spec exists

The UI's language has never been specified, which de facto means
English — for a household tool whose primary deployment is a
German-speaking home. Localization under the no-toolchain rule cannot
reach for an npm i18n framework, so the mechanism itself needs to be
specified, or ad-hoc string handling will make it impossible later.

**Scope: the user-facing PWA, in German and English.** The
server-rendered admin area stays English-only, deliberately — it is an
operator surface seen by one person, and localizing Go templates doubles
the surface for near-zero benefit. Adding further languages later must
be a pure content task (one new JSON file), never a code change.

## Mechanism

- **Catalogs:** `web/static/i18n/en.json` and `web/static/i18n/de.json` —
  flat maps of stable key → string, e.g.
  `"review.confirm": "Confirm"` / `"review.confirm": "Bestätigen"`.
  Keys are dot-namespaced by page/component. They ship inside the
  binary like every other static file (`embed.FS`).
- **`js/i18n.js`:** loads the active catalog once per page load (plain
  `fetch`, cached by the service worker like other static assets —
  `/i18n/` joins the `sw.js` allowlist), and exposes
  `t(key, params)` with `{placeholder}` interpolation.
- **Static page text** is marked with `data-i18n="key"` attributes and
  filled by `i18n.js` at load; **dynamic strings** go through `t()` in
  page modules. Both write with `textContent` only — the injection rule
  from `05-frontend-pwa-foundations.md` applies to translated strings
  exactly as to user data, and translations must never be `innerHTML`'d.
- **Whole sentences per key.** No building sentences from fragments —
  concatenated fragments break the moment a language orders words
  differently. Countable phrases carry `.one`/`.other` key variants
  (`"inbox.waiting.one": "1 proposal waiting"`,
  `"inbox.waiting.other": "{count} proposals waiting"`), which suffices
  for German and English; languages with richer plural rules are a
  problem for the spec revision that adds one.
- **Language resolution**, in order: the `localStorage` override set on
  `settings.html` → the first of `navigator.languages` with a catalog →
  `en`. The chosen language sets `<html lang>` (screen readers care)
  and is used for `Intl.DateTimeFormat`/`Intl.NumberFormat` wherever
  the UI renders dates and numbers.
- **Fallback:** a key missing from the active catalog renders the `en`
  string (the `en` catalog is the reference and must be complete); a
  key missing from both renders the key itself — ugly on purpose, so it
  gets fixed.

## The API stays English

Error `message` strings, log lines, and everything the server emits
remain English. `04-backend-api-conventions.md` already defines the
contract: the frontend switches on the stable `code`, and this spec
adds the mapping — `js/api.js`'s `ApiError` is rendered through
`t("error." + code)`, falling back to the server's `message` for a code
without a translation. Validation field messages fall back the same
way. Localizing server output would couple deployments to languages and
break the one-serializer rule for no gain.

The same boundary holds for AI text: Gemini prompts and matching are
about the **product's own label text** as printed on the shelf
(`06-vision-shelf-ingestion.md`), which is whatever language the
household's products are in — that is data, not UI, and no translation
layer is applied to it. Consequence worth stating: `catalog_products`
naturally accumulates entries in multiple languages, and trigram
matching simply treats them as strings. That is correct behavior, not a
bug to fix.

## Keeping the catalogs honest

A Go test (running in the normal `docker compose run --rm app go test
./...` suite, reading the catalogs through the same `embed.FS`) asserts:

- `en.json` and `de.json` parse and contain **identical key sets** — a
  key added to one and not the other fails the build, which is the only
  mechanism that keeps a no-toolchain project's translations from
  rotting.
- Every `{placeholder}` appearing in a key's `en` string appears in its
  `de` string and vice versa.

E2E (`05-frontend-pwa-foundations.md`) gains one journey: switch the
language to German on `settings.html`, reload, and assert a known label
renders from `de.json` and `<html lang="de">` is set.

## What is deliberately out of scope

- Translating user data (product names, categories, locations) — it is
  the household's own text.
- Localizing the admin UI, API messages, CSV headers beyond what the
  frontend already renders, or Gemini prompts.
- RTL layout and complex plural rules — until a language needing them
  is actually added.

## Acceptance criteria

- Every user-visible string in `web/static` pages and `js/` modules
  resolves through a catalog key; a sweep for hard-coded English
  literals in page markup and modules comes back empty (enforced by
  review, spot-checked by the E2E journey).
- The key-parity and placeholder-parity tests fail when the catalogs
  diverge.
- With `de` active, dates and numbers render via `Intl` in German
  format; with no override and a `de-CH` browser, German is chosen; an
  unsupported browser language falls back to English without an error.
- Translated strings are inserted with `textContent` only.
- Error toasts render the localized string for known codes and the
  server `message` for unknown ones.
- The admin area renders unchanged, in English, regardless of the PWA
  language setting.
