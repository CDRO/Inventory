// Product lookups for the manual-correction picker and the batch allocator in
// docs/specs/09-consumption-logging.md, and the "all products" autocomplete
// group on the ingestion review screen in docs/specs/06-vision-shelf-ingestion.md
// (js/pages/review.js) — the same style as location-options.js: a full list
// fetched once and rendered as native controls.

import { get } from "./api.js";

/** @typedef {{id: string, name: string}} ProductRef */

/**
 * fetchProducts loads a storage's whole product list, for the "pick an
 * existing product" search in a consumption row's manual correction, and
 * for the ingestion review screen's product autocomplete. Consumption never
 * creates a product, so unlike location-options.js there is nothing here
 * for consume-review.js to propose creating; review.js has its own,
 * separate new-product flow that does not go through this function either.
 *
 * @param {string} storageId
 * @returns {Promise<ProductRef[]>}
 */
export async function fetchProducts(storageId) {
  const body = await get(`/api/storages/${storageId}/products`);
  return body.items;
}

/**
 * @typedef {{id: string, product_id: string, location_id: string,
 *   quantity: number, expiration_date: string|null,
 *   expiration_source: string}} BatchRef
 */

/**
 * fetchProductBatches loads one product's batches, nearest expiration first —
 * the default first-out order the decrement picker starts from.
 *
 * @param {string} storageId
 * @param {string} productId
 * @returns {Promise<BatchRef[]>}
 */
export async function fetchProductBatches(storageId, productId) {
  const body = await get(`/api/storages/${storageId}/products/${productId}/batches`);
  return body.items;
}
