// The barcode scan sheet, shared by every screen that needs a code
// (docs/specs/20-barcode-recall.md).
//
// Three ways to get a barcode, offered together rather than as a chain of
// fallbacks the user has to discover:
//
//   1. The browser's native `BarcodeDetector` reading frames from the camera.
//      Chromium-based mobile browsers have it, which is the PWA's actual
//      platform. Feature-checked, never assumed.
//   2. Photographing the barcode and letting the server decode it
//      (POST …/barcodes/decode). The server holds the photo in memory for the
//      length of the call and writes it nowhere — see that handler.
//   3. Typing the digits. Always present, for a damaged or worn label, and
//      the only path on a browser with neither of the above.
//
// No vendored decoder library. docs/specs/20-barcode-recall.md permits one
// later under the vendoring rule of
// docs/specs/05-frontend-pwa-foundations.md if `BarcodeDetector` coverage
// proves too narrow in practice; it is explicitly not required, and the
// server fallback above already closes the gap for the browsers that lack it.

import { postForm, ApiError } from "./api.js";
import { el, text } from "./dom.js";

// The formats docs/specs/20-barcode-recall.md names. The same list the server
// decoder tries, so the two paths agree about what counts as a barcode.
const FORMATS = ["ean_13", "ean_8", "upc_a", "upc_e", "code_128"];

// How often a frame is handed to the detector. Fast enough to feel instant on
// a code already in frame, slow enough that an older phone is not pinned at
// 100% CPU while somebody lines the camera up.
const SCAN_INTERVAL_MS = 250;

/**
 * supportsLiveScan reports whether this browser can read the camera itself.
 *
 * Both halves are required and both are genuinely absent somewhere: Safari
 * has getUserMedia but no BarcodeDetector, and any browser on an insecure
 * origin has neither. A page that rendered a "Scan" button without checking
 * would show a camera that never resolves anything.
 *
 * @returns {boolean}
 */
export function supportsLiveScan() {
  return (
    typeof window !== "undefined" &&
    typeof window.BarcodeDetector === "function" &&
    typeof navigator !== "undefined" &&
    navigator.mediaDevices != null &&
    typeof navigator.mediaDevices.getUserMedia === "function"
  );
}

/**
 * openScanSheet asks for one barcode and resolves with it, or with null if
 * the sheet is dismissed.
 *
 * **It never writes anything.** Resolving a code is the caller's business;
 * this module only obtains the string. That separation is what keeps
 * docs/specs/20-barcode-recall.md's "nothing is written from the scan event
 * itself" true no matter which screen opens the sheet.
 *
 * @param {string} storageId - the storage whose decode route is used.
 * @param {{title?: string, hint?: string}} [options]
 * @returns {Promise<string|null>}
 */
export function openScanSheet(storageId, { title = "Scan a barcode", hint = "" } = {}) {
  const titleId = "barcode-sheet-title";
  const errorBox = el("div", { class: "alert", role: "alert", hidden: true });
  const statusBox = el("p", { class: "empty-state", role: "status", hidden: true });

  const video = el("video", { playsinline: true, muted: true, "aria-label": "Camera preview" });
  const videoWrap = el("div", { class: "stack", hidden: true }, [video]);

  const manualInput = el("input", {
    type: "text",
    inputmode: "numeric",
    autocomplete: "off",
    id: "barcode-sheet-manual",
    placeholder: "4006381333931",
  });
  const manualForm = el("form", { class: "stack" }, [
    el("label", { for: "barcode-sheet-manual" }, [text("Or type the number under the bars")]),
    el("div", { class: "row" }, [
      manualInput,
      el("button", { type: "submit", class: "btn btn--primary" }, [text("Use this")]),
    ]),
  ]);

  const photoInput = el("input", {
    type: "file",
    accept: "image/*",
    capture: "environment",
    id: "barcode-sheet-photo",
  });
  const photoField = el("div", { class: "field" }, [
    el("label", { for: "barcode-sheet-photo" }, [text("Or photograph the barcode")]),
    photoInput,
    el("p", { class: "muted" }, [
      text("The photo is read and discarded — it is never stored."),
    ]),
  ]);

  const cancelButton = el("button", { type: "button", class: "btn btn--ghost" }, [text("Cancel")]);

  const children = [el("h2", { id: titleId }, [text(title)]), errorBox, statusBox];
  if (hint) children.push(el("p", { class: "muted" }, [text(hint)]));
  children.push(videoWrap, manualForm, photoField, el("div", { class: "row" }, [cancelButton]));

  const dialog = el("dialog", { class: "card stack", "aria-labelledby": titleId }, children);

  let stream = null;
  let timer = null;
  let settled = false;
  // Assigned synchronously by the Promise executor below, before anything can
  // call finish(): a dialog cannot be dismissed before it is shown.
  let resolveOuter = () => {};
  const result = new Promise((resolve) => {
    resolveOuter = resolve;
  });

  function showError(err) {
    let message = "Something went wrong. Type the number instead.";
    if (err instanceof ApiError) {
      message =
        err.code === "validation_failed"
          ? "No barcode could be read from that photo. Try again, or type the number."
          : err.message || message;
    }
    errorBox.textContent = message;
    errorBox.hidden = false;
  }

  function showStatus(message) {
    statusBox.textContent = message;
    statusBox.hidden = message === "";
  }

  function stopCamera() {
    if (timer != null) {
      clearInterval(timer);
      timer = null;
    }
    if (stream) {
      for (const track of stream.getTracks()) track.stop();
      stream = null;
    }
  }

  // finish is the single exit. Every path — a live scan, a typed code, a
  // decoded photo, a cancel — goes through it, so the camera is released
  // exactly once and the dialog never resolves twice.
  function finish(code) {
    if (settled) return;
    settled = true;
    stopCamera();
    dialog.close();
    resolveOuter(code);
  }

  manualForm.addEventListener("submit", (event) => {
    event.preventDefault();
    const typed = manualInput.value.trim();
    if (typed === "") {
      showError(new Error("empty"));
      return;
    }
    finish(typed);
  });

  photoInput.addEventListener("change", async () => {
    const file = photoInput.files && photoInput.files[0];
    if (!file) return;
    errorBox.hidden = true;
    showStatus("Reading the photo…");
    const body = new FormData();
    body.append("image", file);
    try {
      const { barcode } = await postForm(`/api/storages/${storageId}/barcodes/decode`, body);
      finish(barcode);
    } catch (err) {
      showStatus("");
      photoInput.value = "";
      showError(err);
    }
  });

  cancelButton.addEventListener("click", () => finish(null));
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) finish(null);
  });
  // Esc fires "cancel" then "close"; both converge on finish so a dismissed
  // sheet still releases the camera.
  dialog.addEventListener("close", () => {
    stopCamera();
    dialog.remove();
    if (!settled) {
      settled = true;
      resolveOuter(null);
    }
  });

  document.body.append(dialog);
  dialog.showModal();
  manualInput.focus();

  if (supportsLiveScan()) {
    startLiveScan();
  }

  async function startLiveScan() {
    let detector;
    try {
      detector = new window.BarcodeDetector({ formats: FORMATS });
    } catch {
      // A browser that has the constructor but supports none of these
      // formats. The typed and photographed paths still work.
      return;
    }

    try {
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: "environment" },
        audio: false,
      });
    } catch {
      // Permission refused, or no camera. Not an error worth shouting
      // about — the other two paths are right there.
      showStatus("No camera available. Type the number or photograph the barcode.");
      return;
    }
    if (settled) {
      // The sheet was dismissed while the permission prompt was open.
      stopCamera();
      return;
    }

    video.srcObject = stream;
    video.muted = true;
    videoWrap.hidden = false;
    showStatus("Point the camera at the barcode.");
    try {
      await video.play();
    } catch {
      // Autoplay refused; the stream is still attached and some browsers
      // start it on the first frame anyway.
    }

    timer = setInterval(async () => {
      if (settled || video.readyState < 2) return;
      try {
        const codes = await detector.detect(video);
        if (codes.length > 0 && codes[0].rawValue) {
          finish(codes[0].rawValue);
        }
      } catch {
        // detect() throws on a frame it cannot grab yet. The next tick is
        // 250ms away; there is nothing to report.
      }
    }, SCAN_INTERVAL_MS);
  }

  return result;
}

/**
 * scanButton builds the "Scan" affordance in the one shape every caller
 * wants, so the label and behaviour do not drift between screens.
 *
 * @param {string} storageId
 * @param {{label?: string, title?: string, onCode: (code: string) => void}} options
 * @returns {HTMLElement}
 */
export function scanButton(storageId, { label = "Scan", title, onCode }) {
  return el(
    "button",
    {
      type: "button",
      class: "btn",
      "data-role": "scan-barcode",
      onclick: async () => {
        const code = await openScanSheet(storageId, title ? { title } : {});
        if (code) onCode(code);
      },
    },
    [text(label)],
  );
}
