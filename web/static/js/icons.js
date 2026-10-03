// The small set of inline SVG icons that compact buttons draw
// (docs/specs/05-frontend-pwa-foundations.md: no icon font, no CDN, no build
// step — every icon is a few path commands in this file).
//
// Why buttons have icons at all: a row that carries several actions — a tree
// node with Rename, Add and Move to…, a batch with Count, Split, Move and
// Container — has no room for four text labels beside a name on a phone, and
// the name was what lost. Below 48rem such a button shows its icon alone;
// the label stays in the DOM, visually hidden, so the button's accessible
// name, the e2e role queries and the i18n swap are exactly what they were.
// From 48rem up the icon sits beside the label (css/components.css,
// `.btn--compact`).
//
// Stroke icons on a 24-unit grid, drawn in currentColor so they take the
// button's own text colour in every state.

import { el, svgEl, text } from "./dom.js";

// Each entry is the list of elements inside the <svg>: [tag, attrs].
const ICONS = {
  plus: [["path", { d: "M12 5v14M5 12h14" }]],
  pencil: [["path", { d: "M12 20h9" }], ["path", { d: "M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z" }]],
  move: [["path", { d: "M5 9l-3 3 3 3M9 5l3-3 3 3M15 19l-3 3-3-3M19 9l3 3-3 3M2 12h20M12 2v20" }]],
  x: [["path", { d: "M18 6 6 18M6 6l12 12" }]],
  check: [["path", { d: "M20 6 9 17l-5-5" }]],
  clock: [["circle", { cx: "12", cy: "12", r: "9" }], ["path", { d: "M12 7v5l3 2" }]],
  clipboard: [
    ["path", { d: "M9 3h6v3H9z" }],
    ["path", { d: "M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" }],
    ["path", { d: "m9 14 2 2 4-4" }],
  ],
  split: [["path", { d: "M16 3h5v5M8 3H3v5M12 22v-8.3a4 4 0 0 0-1.17-2.87L3 3M15 9l6-6" }]],
  "arrow-right": [["path", { d: "M5 12h14M12 5l7 7-7 7" }]],
  box: [
    ["path", { d: "M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" }],
    ["path", { d: "M3.3 7 12 12l8.7-5M12 22V12" }],
  ],
};

/**
 * icon(name) returns an inline SVG for one of the icons above. It is
 * decorative — `aria-hidden` — because every button that carries one also
 * carries its text label (see iconLabel); an icon is never the only thing
 * that names an action.
 *
 * @param {keyof typeof ICONS} name
 * @returns {SVGElement}
 */
export function icon(name) {
  const parts = ICONS[name];
  if (!parts) throw new Error(`icons.js: no icon named ${name}`);
  return svgEl(
    "svg",
    {
      viewBox: "0 0 24 24",
      width: "20",
      height: "20",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": "2",
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
      "aria-hidden": "true",
      focusable: "false",
      class: "btn__icon",
    },
    parts.map(([tag, attrs]) => svgEl(tag, attrs)),
  );
}

/**
 * iconLabel(name, label) is the children of a button that shows an icon and a
 * text label: the SVG, then the label in a `.btn__label` span. On a button
 * with `.btn--compact` the span is visually hidden below 48rem and the icon
 * stands alone; without that class both always show.
 *
 * `label` is rendered as text, never as markup — it may be a name the user
 * typed (a container label, say), and dom.js's rule applies.
 *
 * @param {keyof typeof ICONS} name
 * @param {string} label
 * @returns {Array<Node>}
 */
export function iconLabel(name, label) {
  return [icon(name), el("span", { class: "btn__label" }, [text(label)])];
}
