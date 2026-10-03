// Picture sizes (docs/specs/43-image-derivatives.md).
//
// The server keeps every job photo and product picture at a fixed set of
// sizes beside the original, and a screen asks for the size it shows: a
// 36 px thumbnail is a 96 px file, not the 1024 px picture, and a review crop
// is cut on the server rather than painted by every browser from a 50 MP
// photo. This module is the one place that knows the size ladder and how a
// variant is addressed, so no page builds those URLs by hand.
//
// A variant is a path segment appended to the URL the API handed out —
// `/product-images/{name}/thumb-192`, `/jobs/{id}/image/preview` — and the
// bare URL still serves the original, so a URL this module does not
// recognise (a suggestion-cache picture, an icon) is passed through as it is.

/** The square thumbnail sizes the server renders, smallest first. */
export const THUMB_SIZES = [96, 192, 384, 768];

const PRODUCT_IMAGE = /\/product-images\/[^/?#]+$/;
const JOB_IMAGE = /\/jobs\/[^/?#]+\/image$/;

/**
 * supportsVariants reports whether url is one the server renders sizes for.
 * An SVG icon has no sizes — it is served as it is at every one — and a
 * suggestion-cache picture is already normalised small.
 *
 * @param {string|null|undefined} url
 * @returns {boolean}
 */
export function supportsVariants(url) {
  if (!url || url.endsWith(".svg")) return false;
  return PRODUCT_IMAGE.test(url) || JOB_IMAGE.test(url);
}

/**
 * variantURL appends a variant — "preview", "thumb-192", … — to a picture URL,
 * or returns the URL untouched when it has no variants.
 *
 * @param {string|null|undefined} url
 * @param {string} variant
 * @returns {string|null|undefined}
 */
export function variantURL(url, variant) {
  return supportsVariants(url) ? `${url}/${variant}` : url;
}

/**
 * rowCropURL is the crop of one detected item from a job's photo, at a
 * square size. `version` is the job's updated_at: the crop changes when the
 * job is analysed again while its URL does not, so the version in the query
 * string is what keeps a browser from showing the old one.
 *
 * @param {string} imageURL - the job's `/image` URL
 * @param {string} rowId
 * @param {number} size - 192 or 384
 * @param {string} version
 * @returns {string}
 */
export function rowCropURL(imageURL, rowId, size, version) {
  return `${imageURL}/rows/${encodeURIComponent(rowId)}/thumb-${size}?v=${encodeURIComponent(version)}`;
}

/**
 * thumbAttrs returns the `src`, `srcset` and `sizes` attributes for a square
 * thumbnail shown at cssPx CSS pixels: every size the server has as a
 * candidate, so the browser picks the smallest one that covers its own
 * device pixel ratio, and the 2× size as the plain src for anything that
 * ignores srcset. A URL without variants gets only its src back.
 *
 * @param {string|null|undefined} url
 * @param {number} cssPx
 * @returns {{src: string|null|undefined, srcset?: string, sizes?: string}}
 */
export function thumbAttrs(url, cssPx) {
  if (!supportsVariants(url)) return { src: url };
  const fallback = THUMB_SIZES.find((n) => n >= cssPx * 2) ?? THUMB_SIZES[THUMB_SIZES.length - 1];
  return {
    src: `${url}/thumb-${fallback}`,
    srcset: THUMB_SIZES.map((n) => `${url}/thumb-${n} ${n}w`).join(", "),
    sizes: `${cssPx}px`,
  };
}

/**
 * setThumb points an existing <img> at a thumbnail, the way thumbAttrs builds
 * one. For elements that come from a template rather than el().
 *
 * @param {HTMLImageElement} img
 * @param {string|null|undefined} url
 * @param {number} cssPx
 */
export function setThumb(img, url, cssPx) {
  const attrs = thumbAttrs(url, cssPx);
  if (attrs.srcset) {
    img.srcset = attrs.srcset;
    img.sizes = attrs.sizes;
  } else {
    img.removeAttribute("srcset");
    img.removeAttribute("sizes");
  }
  img.src = attrs.src ?? "";
}
