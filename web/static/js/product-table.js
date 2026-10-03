// Shared piece of the one product-table implementation
// docs/specs/16-product-maintenance.md and
// docs/specs/33-inventory-overview-table.md both render: "make it look like
// the inventory page," not a second table. js/pages/inventory.js and
// js/pages/products.js both build a `<table class="inventory-table">` (the
// CSS lives once, in css/components.css) and both need a product cell —
// a thumbnail beside a name that links somewhere. That cell is the one piece
// small and identical enough to actually share; the surrounding columns
// differ per page and stay in each page's own module.

import { el, text } from "./dom.js";
import { thumbAttrs } from "./images.js";

/**
 * productCell renders an `.inventory-table__product` cell: a thumbnail
 * (when `imageUrl` is set) beside a name that links wherever the caller
 * points it — another page (inventory.html, into products.html) or, on
 * products.html itself, back into this same page's own detail view.
 *
 * @param {string|null|undefined} imageUrl
 * @param {string} name - user text; always passed through text(), never
 *   innerHTML.
 * @param {Object} anchorAttrs - attributes for the `<a>`, e.g. `{href}` or
 *   `{href, onclick}`.
 */
export function productCell(imageUrl, name, anchorAttrs) {
  // 36 px on screen, so the 96 px file at 1× and 2×, never the 1024 px
  // picture (docs/specs/43-image-derivatives.md). Lazy, with its size
  // declared, so a long table neither fetches every picture at once nor
  // shifts as they arrive.
  const thumb = imageUrl
    ? [el("img", { class: "inventory-table__thumb", ...thumbAttrs(imageUrl, 36), alt: "", loading: "lazy", width: "36", height: "36" })]
    : [];
  return el("span", { class: "inventory-table__product" }, [
    ...thumb,
    el("a", anchorAttrs, [text(name)]),
  ]);
}
