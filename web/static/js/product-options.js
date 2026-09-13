// Product lookups for the manual-correction picker and the batch allocator in
// docs/specs/09-consumption-logging.md, the same style as location-options.js:
// a full list fetched once and rendered as native controls.

import { get } from "./api.js";

/** @typedef {{id: string, name: string}} ProductRef */

/**
 * fetchProducts loads a storage's whole product list, for the "pick an
 * existing product" search in a consumption row's manual correction.
 * Consumption never creates a product, so unlike location-options.js there is
 * nothing here for a review screen to propose creating.
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
