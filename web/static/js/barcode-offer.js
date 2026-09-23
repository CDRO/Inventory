// The capture-time barcode offer (docs/specs/20-barcode-recall.md,
// "Offering a barcode at first capture").
//
// A product created with no barcode is the common case the
// second-scan-is-free promise depends on: nobody benefits from it until a
// first code exists to recall. So the moment a new product appears — a
// confirmed vision-ingestion row (docs/specs/06-vision-shelf-ingestion.md), an
// accepted shopping-list New Item (docs/specs/07-shopping-list-reconciliation.md),
// an item added from the reorder dashboard
// (docs/specs/10-reorder-and-shopping-export.md) — this offers to scan one
// while the item is still in the user's hand.
//
// # Three choices, every time, never a silent fourth
//
// Do it now · Not this time · Turn this off. The middle one records nothing
// at all: no per-product state, no "declined" counter, nothing the user could
// later be asked about. It is the ordinary answer for an item whose packaging
// is already in the bin.
//
// # It never blocks the thing it attaches to
//
// The confirm it follows has already happened and already been reported by
// the time this is called. offerBarcodeCapture resolves either way and throws
// nothing: an offer that could fail the write it rides on would be worse than
// no offer.
//
// # Nothing here is scored
//
// Accepting, declining and disabling all write no gamification contribution
// (docs/specs/50-gamification-overview.md's own principle — the layer excludes
// activity a naive design could inflate for reward, and a barcode scan is
// exactly that shape). There is no call into js/gamification.js from this
// file, and the association endpoint records nothing either.

import { get, post, patch } from "./api.js";
import { el, text } from "./dom.js";
import { openScanSheet } from "./barcode.js";

/**
 * shouldOffer asks the server whether the offer applies to this user and in
 * which tone, and records that it is being shown.
 *
 * The question and the recording are one request on purpose: splitting them
 * would let two qualifying products — two tabs, or a confirm and a list accept
 * seconds apart — both be told they are the first, and the playful copy would
 * appear twice for a user it is meant to greet once.
 *
 * @returns {Promise<{enabled: boolean, firstTime: boolean}>}
 */
async function shouldOffer() {
  try {
    const state = await post("/api/auth/barcode-prompt/shown");
    return { enabled: Boolean(state?.enabled), firstTime: Boolean(state?.first_time) };
  } catch {
    // A failure here means no offer, which costs the user nothing. It must
    // never surface as an error on a screen that has just succeeded at the
    // thing the user actually asked for.
    return { enabled: false, firstTime: false };
  }
}

/**
 * hasBarcodeAlready reports whether a product can already be recalled by a
 * code, so a product created *with* one — a shopping-list line matched
 * through a catalog barcode hint, say — is never offered another.
 *
 * @param {string} storageId
 * @param {string} productId
 * @returns {Promise<boolean>}
 */
async function hasBarcodeAlready(storageId, productId) {
  try {
    const list = await get(`/api/storages/${storageId}/products/${productId}/barcodes`);
    return Array.isArray(list?.items) && list.items.length > 0;
  } catch {
    // Unknown. Treated as "already has one" so a broken read errs towards not
    // pestering somebody rather than towards offering twice.
    return true;
  }
}

/**
 * offerBarcodeCapture shows the offer for one newly created product, if it
 * applies, and resolves once the user has answered it.
 *
 * @param {string} storageId
 * @param {{productId: string, productName?: string}} product
 * @returns {Promise<{associated: string|null}>} the code that was attached, or
 *   null for either of the two answers that attach nothing.
 */
export async function offerBarcodeCapture(storageId, { productId, productName = "" }) {
  if (!storageId || !productId) return { associated: null };

  if (await hasBarcodeAlready(storageId, productId)) return { associated: null };

  const { enabled, firstTime } = await shouldOffer();
  if (!enabled) return { associated: null };

  // Only once the offer is certain to appear: naming the product is worth a
  // request when somebody is about to be asked about it, and worth nothing
  // when they are not.
  const name = productName || (await productLabel(storageId, productId));

  return showOffer(storageId, { productId, productName: name, firstTime });
}

// productLabel names the product the offer is about, so a confirm that created
// three of them asks three recognisable questions rather than three identical
// ones. A failed read simply leaves the dialog without the name line.
async function productLabel(storageId, productId) {
  try {
    const product = await get(`/api/storages/${storageId}/products/${productId}`);
    return typeof product?.name === "string" ? product.name : "";
  } catch {
    return "";
  }
}

/**
 * offerScannedBarcode is the same offer for a code the user has *already*
 * scanned — the miss path of docs/specs/20-barcode-recall.md, where an unknown
 * code sent them to the single-product photo instead, and the code was carried
 * client-side through that flow.
 *
 * Still three choices, still nothing written until one of them is chosen. The
 * only difference is that the first one attaches the code in hand rather than
 * opening the scan sheet again: asking somebody to re-scan a box they scanned
 * ninety seconds ago is the friction this whole spec exists to remove.
 *
 * @param {string} storageId
 * @param {{productId: string, productName?: string, code: string}} options
 * @returns {Promise<{associated: string|null}>}
 */
export async function offerScannedBarcode(storageId, { productId, productName = "", code }) {
  if (!storageId || !productId || !code) return { associated: null };
  if (await hasBarcodeAlready(storageId, productId)) return { associated: null };

  const { enabled, firstTime } = await shouldOffer();
  if (!enabled) return { associated: null };

  const name = productName || (await productLabel(storageId, productId));
  return showOffer(storageId, { productId, productName: name, firstTime, presetCode: code });
}

function showOffer(storageId, { productId, productName, firstTime, presetCode = null }) {
  const titleId = "barcode-offer-title";
  const errorBox = el("div", { class: "alert", role: "alert", hidden: true });

  // The first-ever occurrence explains why; every later one is the same three
  // actions with plain copy. No streak, no points, no second onboarding.
  const heading = firstTime ? "One scan now, one tap forever" : "Scan its barcode?";
  const body = firstTime
    ? "Scan it once now, and every future one of these is a single tap — no photo, no waiting, no guessing."
    : "Scanning it now makes the next one a single tap.";

  const nowButton = el("button", { type: "button", class: "btn btn--primary" }, [
    text(presetCode ? "Attach the code you scanned" : "Scan it now"),
  ]);
  const laterButton = el("button", { type: "button", class: "btn btn--ghost" }, [text("Not this time")]);
  const offButton = el("button", { type: "button", class: "btn btn--ghost" }, [text("Turn this off")]);

  const children = [el("h2", { id: titleId }, [text(heading)])];
  if (productName) children.push(el("p", { class: "muted" }, [text(productName)]));
  if (presetCode) children.push(el("p", { class: "muted" }, [text(presetCode)]));
  children.push(
    el("p", {}, [text(body)]),
    errorBox,
    // All three, always, in one row. A build that dropped one — because the
    // camera is missing, say — would break the acceptance criterion: the scan
    // sheet itself always offers typing and photographing, so "Scan it now"
    // is never the unavailable choice.
    el("div", { class: "row" }, [nowButton, laterButton, offButton]),
  );

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": titleId }, children);

  let settled = false;
  let resolveOuter = () => {};
  const result = new Promise((resolve) => {
    resolveOuter = resolve;
  });

  function finish(associated) {
    if (settled) return;
    settled = true;
    dialog.close();
    resolveOuter({ associated });
  }

  nowButton.addEventListener("click", async () => {
    errorBox.hidden = true;
    const code =
      presetCode ||
      (await openScanSheet(storageId, {
        title: "Scan a barcode",
        hint: productName ? `This code will mean “${productName}” in this storage.` : "",
      }));
    if (!code) return; // The sheet was dismissed; the offer is still open.

    try {
      await post(`/api/storages/${storageId}/products/${productId}/barcodes`, { barcode: code });
      finish(code);
    } catch (err) {
      errorBox.textContent =
        err && err.code === "conflict"
          ? "That barcode already belongs to another product here."
          : "That barcode could not be attached. Try again, or skip it for now.";
      errorBox.hidden = false;
    }
  });

  laterButton.addEventListener("click", () => finish(null));

  offButton.addEventListener("click", async () => {
    try {
      await patch("/api/auth/barcode-prompt", { enabled: false });
    } catch {
      // The offer still closes. Failing to persist the preference means it
      // appears again next time, which is annoying but harmless; refusing to
      // close would be worse.
    }
    finish(null);
  });

  // Dismissing the dialog is "not this time": it attaches nothing and records
  // nothing, which is exactly what the middle button does.
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) finish(null);
  });
  dialog.addEventListener("close", () => {
    dialog.remove();
    if (!settled) {
      settled = true;
      resolveOuter({ associated: null });
    }
  });

  document.body.append(dialog);
  dialog.showModal();
  nowButton.focus();
  return result;
}
