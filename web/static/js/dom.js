// Small DOM helpers shared by every page and component
// (docs/specs/05-frontend-pwa-foundations.md).
//
// Everything here exists to keep one rule easy to follow: user- and
// AI-supplied text is never turned into HTML. A shelf-photo label, a catalog
// display name, a shopping-list line — all of it was written by someone else
// (a stranger, in the catalog's case) and goes into the DOM through
// `textContent`, never through `innerHTML` built from a template string.

/**
 * el(tag, attrs, children) creates one element without ever touching
 * innerHTML.
 *
 * @param {string} tag
 * @param {Object<string, string|Function>} [attrs] - attribute name/value
 *   pairs; a key starting with "on" (e.g. "onclick") is added as an event
 *   listener instead of an attribute. `className` and `dataset.*` are not
 *   special-cased — use `class` and `data-*` directly, since those are the
 *   real attribute names and this helper does not translate between the two.
 * @param {Array<Node|string>} [children] - text is inserted as text, not
 *   parsed as markup.
 * @returns {HTMLElement}
 */
export function el(tag, attrs = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (key.startsWith("on") && typeof value === "function") {
      node.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (value === true) {
      node.setAttribute(key, "");
    } else {
      node.setAttribute(key, String(value));
    }
  }
  for (const child of children) {
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

/**
 * text(value) returns a text node. A thin wrapper so callers never have to
 * choose between this and a raw string when building an `el()` children
 * array — both work, but spelling it out at a call site that handles
 * untrusted content documents the intent.
 *
 * @param {string} value
 * @returns {Text}
 */
export function text(value) {
  return document.createTextNode(value == null ? "" : String(value));
}

/**
 * clearChildren(node) empties an element without a innerHTML = "" — which is
 * harmless on its own, but a codebase that never touches innerHTML is a
 * codebase where a reviewer does not have to check whether a given use was
 * the safe kind.
 *
 * @param {Element} node
 */
export function clearChildren(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/**
 * fromTemplate(template) clones a <template>'s content and returns the
 * single root element inside it. Used by js/review.js and js/tree.js, whose
 * rows are cloned from a <template> in the page's own HTML rather than built
 * from a string (docs/specs/05-frontend-pwa-foundations.md).
 *
 * @param {HTMLTemplateElement} template
 * @returns {HTMLElement}
 */
export function fromTemplate(template) {
  const fragment = template.content.cloneNode(true);
  const root = fragment.firstElementChild;
  if (!root) {
    throw new Error("fromTemplate: <template> has no element child");
  }
  return root;
}

/**
 * qs(selector, within) is document.querySelector with an obvious name, kept
 * this small on purpose: this project has no reason to grow a query
 * mini-language on top of what the DOM already provides.
 *
 * @param {string} selector
 * @param {ParentNode} [within]
 * @returns {Element|null}
 */
export function qs(selector, within = document) {
  return within.querySelector(selector);
}

/**
 * qsa(selector, within) is document.querySelectorAll, returned as a real
 * array so callers get Array methods (map/filter) instead of having to
 * remember to spread a NodeList first.
 *
 * @param {string} selector
 * @param {ParentNode} [within]
 * @returns {Element[]}
 */
export function qsa(selector, within = document) {
  return Array.from(within.querySelectorAll(selector));
}
