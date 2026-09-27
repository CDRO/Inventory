// The in-page camera: a viewfinder on the capture screen
// (docs/specs/37-in-page-camera.md).
//
// docs/specs/36-photo-source-picker.md's Camera control hands off to the
// phone's own camera app: the page is left, one shot is taken, the page comes
// back. For a run of twenty shelves — what
// docs/specs/09-consumption-logging.md says the real usage is — that is
// twenty page-leave-and-return round trips, with the sticky mode and the
// location field out of sight each time. This module keeps the page: a modal
// <dialog> with a live preview, one tap per shot, and the growing selection
// list exactly where it was when Done is pressed.
//
// It owns no list and uploads nothing. Captures go into 36's picker through
// its module API — `picker.add()` for a shot, `picker.setStatus()` for the one
// status line — so the list stays the single place a photo waits before
// upload.
//
// Multi-photo mode only. The barcode scan sheet's Camera control keeps opening
// the OS input: that sheet is already a modal dialog, has no list to append
// to, and a live camera for a barcode is docs/specs/20-barcode-recall.md's own
// scan path (36, "Single-photo mode").
//
// Deliberately NOT shared with js/barcode.js, which opens a rear-camera stream
// into a dialog too. That stream exists to be *read* by a detector on a timer
// and closes on the first hit; this one exists to be *captured* on demand and
// stays open across shots. The overlap is ten lines of getUserMedia() and
// stop(); a shared abstraction over two different lifecycles would need
// options both callers have to reason about (37, "What is deliberately not
// shared").

import { el, text } from "./dom.js";
import { t, tCount } from "./i18n.js";

// The capture counter is per *page*, not per viewfinder session: spec 37 wants
// `capture-<n>.jpg` to keep counting up "for the lifetime of the page's
// picker, never restarting per viewfinder session", so that no two captures in
// one selection can share a name. Opening the viewfinder three times and
// taking one shot each gives capture-1.jpg, capture-2.jpg, capture-3.jpg.
//
// Module scope is the page's lifetime here because ingest.html mounts exactly
// one multi-photo picker; the scan sheet's single-photo picker never reaches
// this module at all.
let captureCount = 0;

// Set once a getUserMedia rejection arrives too late to reuse the tap's
// activation (see onCameraTap). From then on the Camera control goes straight
// to the OS input for the rest of the page's life, because a second dialog
// that cannot open is a second dead tap.
let cameraUnavailable = false;

/**
 * supportsViewfinder reports whether an in-page viewfinder can be offered at
 * all, checked at the moment Camera is tapped rather than assumed from a
 * page-load check.
 *
 * The optional chaining is load bearing: `navigator.mediaDevices` is
 * `undefined` — not an object with a missing method — on an insecure origin,
 * so `navigator.mediaDevices.getUserMedia` would throw a TypeError instead of
 * answering the question. A plain-HTTP origin on a home network gets the OS
 * camera silently, and that is correct (spec 37, "Fallback").
 *
 * @returns {boolean}
 */
export function supportsViewfinder() {
  return typeof navigator !== "undefined" && navigator.mediaDevices?.getUserMedia != null;
}

/**
 * handleCameraTap is what docs/specs/36-photo-source-picker.md's
 * `mountPhotoPicker({ onCameraTap })` delegates a Camera tap to. It decides
 * between the in-page viewfinder and the OS camera, on every tap, and never
 * from a page-load check.
 *
 * **It is deliberately not `async`.** The ordering here is the whole of spec
 * 37's "activation trap": a file input's `click()` only opens the chooser
 * while the tap's transient user activation is alive (~5 s in Chromium,
 * stricter on iOS), and `await getUserMedia()` can outlive it — the person
 * reads the permission prompt for ten seconds and then taps Block. So both
 * fallback branches below call `openOsCamera()` **synchronously, with nothing
 * awaited in front of them**, and only the branch that opens a viewfinder is
 * allowed to be asynchronous. Making this function `async` would put a
 * microtask before every `click()` and is exactly the regression that leaves
 * the Camera button dead on an insecure origin.
 *
 * @param {{picker: {add: (files: File[]) => void, setStatus: (text: string) => void}, modeLabel: () => string, openOsCamera: () => void}} args
 */
export function handleCameraTap({ picker, modeLabel, openOsCamera }) {
  if (cameraUnavailable) {
    openOsCamera();
    // The status line explains a Camera control that now behaves differently
    // than it did on the first tap. It is a status, not an error: the OS
    // camera is the answer and needs no browser permission.
    picker.setStatus(t("camera.usingPhoneCamera"));
    return;
  }
  if (!supportsViewfinder()) {
    // Silently — an insecure origin simply has no in-page camera, and saying
    // so would be reporting the platform as a fault.
    openOsCamera();
    return;
  }
  // Only now, with both synchronous paths already ruled out. Deliberately not
  // awaited: there is nothing for a caller to wait for, and the floating
  // promise is the viewfinder's own lifetime.
  void openViewfinder({ picker, modeLabel, openOsCamera });
}

async function openViewfinder({ picker, modeLabel, openOsCamera }) {
  const tappedAt = Date.now();
  let facingMode = "environment";
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode }, audio: false });
  } catch {
    // A denied permission is not an error worth showing (spec 37): the OS
    // camera is the answer either way. What differs is only whether this tap
    // can still open it.
    if (activationIsAlive(tappedAt)) {
      openOsCamera();
    } else {
      // click() would be a silent no-op now and the Camera button would look
      // dead. Remember it, show the one status line, and let the next tap go
      // straight to the OS input.
      cameraUnavailable = true;
      picker.setStatus(t("camera.usingPhoneCamera"));
    }
    return;
  }

  const titleId = "viewfinder-title";
  const heading = el("h2", { id: titleId }, [text(t("camera.heading", { mode: modeLabel() }))]);
  const video = el("video", {
    playsinline: true,
    muted: true,
    "aria-label": t("camera.preview"),
    class: "viewfinder__video",
    "data-role": "viewfinder-video",
  });
  // Muted text under the preview, shown once per viewfinder session on the
  // canvas path only — the owner's accepted trade-off, stated rather than
  // hidden (spec 37, step 1).
  const hint = el("p", { class: "muted", "data-role": "resolution-hint", hidden: true }, [
    text(t("camera.resolutionHint")),
  ]);
  const shotCount = el("p", { class: "muted", "data-role": "shot-count" });
  const shutter = el("button", {
    type: "button",
    class: "btn btn--primary viewfinder__shutter",
    "data-role": "shutter",
    // Disabled until the first frame exists. A <video> is laid out and
    // "visible" before it has one, and a canvas grab in that window yields a
    // null blob — so this is a correctness gate, not a nicety.
    disabled: true,
  }, [text(t("camera.shutter"))]);
  const doneButton = el("button", { type: "button", class: "btn", "data-role": "done" }, [text(t("camera.done"))]);
  const flipButton = el("button", { type: "button", class: "btn", "data-role": "flip", hidden: true }, [
    text(t("camera.flip")),
  ]);

  const dialog = el(
    "dialog",
    { class: "card stack", "aria-labelledby": titleId, "data-role": "viewfinder" },
    [heading, video, hint, shotCount, el("div", { class: "row" }, [shutter, flipButton, doneButton])],
  );

  // Per *session*, unlike captureCount above: spec 37's dialog shows "the
  // running count of shots taken in this viewfinder session".
  let sessionShots = 0;
  let imageCapture = makeImageCapture(stream.getVideoTracks()[0]);
  let frameTimer = null;
  let capturing = false;
  let hintShown = false;
  let closed = false;

  function renderShotCount() {
    shotCount.textContent = tCount("camera.shots", sessionShots);
  }
  renderShotCount();

  // release() stops every track and the first-frame poller. exit() is the
  // single way out — Done, Esc, a backdrop click and the dialog's own `close`
  // event all converge here, the same shape js/barcode.js uses, or the
  // camera indicator stays lit after the dialog is gone.
  function release() {
    if (frameTimer != null) {
      clearInterval(frameTimer);
      frameTimer = null;
    }
    if (stream) {
      for (const track of stream.getTracks()) track.stop();
      stream = null;
    }
    video.srcObject = null;
  }

  function exit() {
    if (closed) return;
    closed = true;
    release();
    if (dialog.open) dialog.close();
    dialog.remove();
  }

  // Waits for a decodable frame, not merely for metadata: `loadedmetadata`
  // fires before videoWidth is necessarily non-zero, and videoWidth > 0 is
  // the condition a canvas grab actually needs. Measured in this repo's
  // Playwright image: ~59 ms after srcObject is set.
  function waitForFirstFrame() {
    if (frameTimer != null) {
      clearInterval(frameTimer);
      frameTimer = null;
    }
    if (video.videoWidth > 0) {
      shutter.disabled = false;
      return;
    }
    frameTimer = setInterval(() => {
      if (closed) {
        clearInterval(frameTimer);
        frameTimer = null;
        return;
      }
      if (video.videoWidth > 0) {
        clearInterval(frameTimer);
        frameTimer = null;
        shutter.disabled = false;
      }
    }, 30);
  }

  async function attach(newStream) {
    stream = newStream;
    imageCapture = makeImageCapture(stream.getVideoTracks()[0]);
    video.srcObject = stream;
    video.muted = true;
    try {
      await video.play();
    } catch {
      // Autoplay refused; the stream is attached and some browsers start it
      // on the first frame anyway. The first-frame poller below is what
      // actually gates the Shutter, so there is nothing to report here.
    }
    waitForFirstFrame();
  }

  // encode() is the one place a File is produced, so the capture numbering
  // cannot drift between the two still paths: the counter advances only when
  // a File genuinely exists. A null return means "this path produced
  // nothing" and the caller tries the next one.
  async function encode(width, height, draw) {
    if (!(width > 0) || !(height > 0)) return null;
    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext("2d");
    if (!ctx) return null;
    draw(ctx);
    const jpeg = await new Promise((resolve) => canvas.toBlob(resolve, "image/jpeg", 0.92));
    // toBlob hands back null rather than throwing when it cannot encode — a
    // canvas too large for the platform's limits, most plausibly. #205 item 6:
    // the caller falls back to the raw video frame rather than doing nothing.
    if (!jpeg) return null;
    captureCount += 1;
    return new File([jpeg], `capture-${captureCount}.jpg`, { type: "image/jpeg" });
  }

  // Normalises a takePhoto() Blob — whatever the camera stack produced: a
  // JPEG that may carry an EXIF orientation tag on Android, an image/png from
  // Chromium's fake device in the E2E stack — to JPEG at the bitmap's own
  // size, so the sensor resolution survives. `imageOrientation: "from-image"`
  // applies any EXIF orientation to the pixels, which is why nothing leaving
  // here carries EXIF at all.
  //
  // **Never relabel bytes.** A `type: "image/jpeg"` File holding PNG bytes is
  // the failure this function exists to prevent, and the E2E asserts the JPEG
  // SOI marker FF D8 precisely so that relabelling cannot pass.
  async function normalise(blob) {
    let bitmap;
    try {
      bitmap = await createImageBitmap(blob, { imageOrientation: "from-image" });
    } catch {
      // Undecodable, or a bitmap the platform refuses at this size. The
      // caller falls back to the raw frame (#205 item 6).
      return null;
    }
    try {
      return await encode(bitmap.width, bitmap.height, (ctx) => ctx.drawImage(bitmap, 0, 0));
    } finally {
      // Frees the decoded bitmap immediately rather than waiting for GC —
      // a 50 MP still is ~200 MB of RGBA, and a run of twenty shelves would
      // hold every one of them (#205 item 7).
      bitmap.close();
    }
  }

  async function capture() {
    // A guard flag rather than toggling `disabled`: spec 37 says the Shutter
    // is "enabled from then on" once the first frame exists, and a
    // disable/re-enable cycle would make that briefly false — which a test
    // polling the attribute could legitimately catch.
    if (capturing || closed) return;
    capturing = true;
    try {
      let file = null;
      if (imageCapture) {
        let photo = null;
        try {
          // The sensor's photo resolution rather than the preview stream's,
          // where the platform offers it.
          photo = await imageCapture.takePhoto();
        } catch {
          // takePhoto() rejects on some Android devices (UnknownError,
          // InvalidStateError). That press falls back to the canvas grab
          // below rather than doing nothing.
          photo = null;
        }
        if (photo) file = await normalise(photo);
      }
      if (!file) {
        // The canvas grab: the current frame at the video's intrinsic size.
        // Reached three ways — no ImageCapture at all, a takePhoto()
        // rejection, or a normalisation that produced nothing.
        file = await encode(video.videoWidth, video.videoHeight, (ctx) => ctx.drawImage(video, 0, 0));
      }
      if (!file) {
        // Both paths produced nothing. Saying so beats a Shutter that
        // silently does nothing (#205 item 6).
        picker.setStatus(t("camera.captureFailed"));
        return;
      }
      picker.add([file]);
      sessionShots += 1;
      renderShotCount();
      // The hint is gated on ImageCapture being absent for this *stream*, not
      // on which path this particular press took — so a takePhoto() rejection
      // that falls back to the canvas shows no hint (#205 item 7), which is
      // right: the next press will use takePhoto() again, and the resolution
      // trade-off the hint describes is not the one that happened.
      if (!imageCapture && !hintShown) {
        hint.hidden = false;
        hintShown = true;
      }
      flashPreview(video);
    } finally {
      capturing = false;
    }
  }

  async function flip() {
    flipButton.disabled = true;
    shutter.disabled = true;
    const next = facingMode === "environment" ? "user" : "environment";
    // The old stream goes first: two live streams on one device is how a
    // second getUserMedia comes back with an OverconstrainedError on some
    // phones.
    release();
    try {
      const newStream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: next }, audio: false });
      if (closed) {
        for (const track of newStream.getTracks()) track.stop();
        return;
      }
      facingMode = next;
      await attach(newStream);
    } catch {
      // The other camera refused. Nothing is lost that a person can act on —
      // the shots already taken are in the list — but the viewfinder now has
      // no stream, so it closes rather than showing a dead preview.
      exit();
      return;
    } finally {
      flipButton.disabled = false;
    }
  }

  shutter.addEventListener("click", capture);
  doneButton.addEventListener("click", exit);
  flipButton.addEventListener("click", flip);
  dialog.addEventListener("click", (event) => {
    // A backdrop click: the dialog element itself is the event target only
    // outside its own content box.
    if (event.target === dialog) exit();
  });
  // Esc fires "cancel" then "close"; both converge here, so a dismissed
  // viewfinder releases the camera exactly like Done does.
  dialog.addEventListener("close", exit);

  document.body.append(dialog);
  dialog.showModal();
  await attach(stream);
  await offerFlipIfMultipleCameras();

  async function offerFlipIfMultipleCameras() {
    if (closed) return;
    try {
      const devices = (await navigator.mediaDevices.enumerateDevices?.()) || [];
      if (devices.filter((device) => device.kind === "videoinput").length > 1) {
        flipButton.hidden = false;
      }
    } catch {
      // No device list: the flip button simply stays hidden. The OS camera is
      // one tap away for a front-facing shot.
    }
  }
}

// activationIsAlive answers whether the tap that opened this is still allowed
// to open a file chooser. `navigator.userActivation` is the direct answer
// where it exists; on a browser without it — iOS before 16.4 — spec 37's rule
// is that a rejection within 1 s of the tap counts as alive.
function activationIsAlive(tappedAt) {
  if (typeof navigator !== "undefined" && navigator.userActivation) {
    return navigator.userActivation.isActive === true;
  }
  return Date.now() - tappedAt < 1000;
}

function makeImageCapture(track) {
  if (!track || typeof window.ImageCapture !== "function") return null;
  try {
    return new window.ImageCapture(track);
  } catch {
    // Some builds expose the constructor but refuse a track it cannot
    // photograph. The canvas grab covers it.
    return null;
  }
}

// The capture confirmation: the preview dims for ~150 ms, alongside the count
// incrementing. Enough to tell two presses apart without a sound or a
// vibration nobody asked for.
function flashPreview(video) {
  video.classList.add("viewfinder__video--flash");
  setTimeout(() => video.classList.remove("viewfinder__video--flash"), 150);
}
