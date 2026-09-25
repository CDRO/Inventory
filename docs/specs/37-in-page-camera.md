# 37 — In-Page Camera: a Viewfinder on the Capture Screen

Depends on: [`36-photo-source-picker.md`](36-photo-source-picker.md) (the
Camera control this replaces the behaviour of, the selection list captured
photos land in, and the OS-camera path this falls back to),
[`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(HTTPS/secure context, vendoring rule, tokens),
[`04-backend-api-conventions.md`](04-backend-api-conventions.md) (upload
limits, EXIF orientation-then-strip), [`09-consumption-logging.md`](09-consumption-logging.md)
(the mode is visible while the camera is open),
[`20-barcode-recall.md`](20-barcode-recall.md) (`js/barcode.js` already
opens a camera stream the same way; shared conventions, not shared code —
see "What is deliberately not shared").

Amends: `05`, "File layout" — gains `js/camera.js`. `36` stays the contract
for the two controls; this spec changes only what the **Camera** control
does when the browser can do better than hand off to the OS.

## Why this spec exists

With `36`, tapping Camera opens the phone's own camera app: the page is
left, a shot is taken, the OS camera closes, the page comes back with the
photo in the list. For one photo that is fine. For a run — twenty shelves,
one after the other, which is what `09` says the real usage is — it is the
same page-leave-and-return round trip twenty times, with the sticky mode
and the location field out of sight each time — and on Android browsers
that reload the page on return (reported, not measured here; `36`, "What a
page reload costs"), the selection list is gone with it.

A viewfinder **inside the page** never leaves it: the dialog is modal
(`showModal()`, like every dialog here), so it covers the form while open —
which is why the mode's name is repeated in its heading — but nothing is
navigated away from or reloaded, the growing list of shots is exactly
where it was when Done is pressed, and "take another" is one tap instead
of a round trip. The browser API for
it (`getUserMedia`) is the one `js/barcode.js` already uses for live
scanning, so the platform support and the secure-context requirement are
already known quantities here.

## What it does

Tapping **Camera** (`36`) on a browser that supports it opens a
`<dialog>` — the same `card stack` dialog idiom `barcode.js` and
`tree-modal.js` use — from a new module, `web/static/js/camera.js`,
containing:

- a `<video playsinline muted>` fed by
  `getUserMedia({video: {facingMode: "environment"}, audio: false})`,
  filling the dialog's width;
- a **shutter** button (large, centred below the preview, ≥ 56 CSS px),
  **disabled until the first frame exists** (`video.videoWidth > 0`, i.e.
  after `loadedmetadata`): a `<video>` is laid out and "visible" before it
  has a frame, and a canvas grab in that window yields a null blob;
- a **Done** button that closes the viewfinder and keeps everything taken;
- the running count of shots taken in this viewfinder session, and — so
  `09`'s "the current mode is always visible while the camera is open"
  holds here too — the current mode's name ("Shelf scan") in the dialog's
  heading;
- if the device reports more than one camera, a **flip** button that
  switches `facingMode` between `environment` and `user`; the old stream
  is stopped, the new one attached, the `ImageCapture` (below) re-created
  for the new track, and the shutter disabled again until the new stream's
  first frame. No zoom, no torch, no exposure controls: the OS camera is
  one tap away for anything fancier, and every control here is one more
  thing to test on hardware.

This spec applies to the picker in **multi-photo mode only** — the upload
form. In single-photo mode (the barcode scan sheet) the Camera control
keeps opening the OS input: that sheet is a modal dialog already, has no
list to append to, and a live camera for a barcode is `20`'s own scan path
(`36`, "Single-photo mode").

Each shutter press:

1. Grabs a full-resolution still. Where `ImageCapture` exists on the video
   track, `ImageCapture.takePhoto()` is used, because it returns the
   sensor's photo resolution rather than the preview stream's. What it
   returns is whatever the camera stack produces — a JPEG that may carry
   an EXIF orientation tag on Android, a **PNG** from Chromium's fake
   device in the E2E stack — and `takePhoto()` can reject on some Android
   devices (`UnknownError`, `InvalidStateError`); on rejection, that press
   falls back to the canvas grab below rather than doing nothing.
   Otherwise the current frame is drawn onto a `<canvas>` at
   `video.videoWidth × video.videoHeight` — the preview resolution, which
   on most phones is 1080p-class rather than the sensor's 12 MP. That is
   an accepted trade-off — the owner's decision, stated in the UI hint
   below rather than hidden. `06` and `04` accept shelf photos up to 50 MP
   and pass them to the model at upload resolution, so a 1080p-class still
   simply gives the model less to work with; nothing in `00`–`35` says how
   much that costs on a crowded shelf. The implementing package measures it
   on real shelves — the same shelf via the OS camera and via the
   viewfinder, reviewed side by side — and reports in its PR. Anyone who
   wants the full sensor uses the OS camera through `36`.
2. **Normalises every still to JPEG, whichever path produced it.** A
   `takePhoto()` Blob is decoded with
   `createImageBitmap(blob, {imageOrientation: "from-image"})` — which
   applies any EXIF orientation it carries to the pixels — drawn onto a
   canvas at the bitmap's own size (so the sensor resolution is kept), and
   encoded with `canvas.toBlob("image/jpeg", 0.92)`; the canvas path
   encodes its frame the same way. The result is wrapped in a `File` of
   type `image/jpeg` named `capture-<n>.jpg`, where `<n>` counts up for
   the lifetime of the page's picker, never restarting per viewfinder
   session, so no two captures in one selection share a name. The name is
   a label, not a key: `36`'s list keys rows by `File` identity.
3. **Appends** it to `36`'s selection list, exactly as a library pick
   would — nothing is uploaded from the viewfinder itself. The list is the
   one place a photo waits before upload; the viewfinder only produces
   files.
4. Shows a brief capture confirmation (the preview dims for ~150 ms and
   the count increments) and keeps the viewfinder open for the next shot.

There are three ways out and they all mean the same thing: **Done**, the
**Esc** key, and a **click on the backdrop**. Each closes the dialog and
keeps every shot already taken in the list, thumbnails and Remove buttons
included; Upload then sends them like any other selection. There is no
separate Cancel control and no "discard what I took": a photo taken is a
photo the person wanted, and losing three shelves to a mis-tap on the
backdrop is worse than one extra Remove.

Because of the normalisation in step 2, no still leaving the viewfinder
carries EXIF, from either path: orientation is already applied to the
pixels. The server's orientation-then-strip step
(`internal/images/strip.go`) needs nothing from that — it treats a JPEG
without an APP1 segment as upright and copies it through, and would
rotate-then-strip one that did carry a tag — so this is a statement about
what the client sends, not something the server relies on.

## Fallback — `36` is always underneath

The viewfinder is offered only when **all** of these hold, checked at the
moment Camera is tapped, never assumed from a page-load check:

- `navigator.mediaDevices?.getUserMedia` exists — optional chaining,
  because `navigator.mediaDevices` itself is `undefined` on an insecure
  origin (the PWA is served over HTTPS in the E2E stack and behind Traefik
  in production; a plain-HTTP origin on a home network simply gets the OS
  camera, silently, and that is correct);
- `getUserMedia` resolves — permission granted and a camera present.

If any of them fails, the person gets the OS camera, not an error, and a
denied permission is not reported as an error either: the OS camera is
the answer, and it needs no browser permission. **How** the fallback is
reached depends on one browser rule: a file input's `click()` only opens
the chooser while the tap's *transient user activation* is still alive
(about 5 s in Chromium, stricter on iOS), and `await getUserMedia()` can
outlive that — the person reads the permission prompt for ten seconds and
then taps Block. So:

- If `getUserMedia` is absent, the tap opens `36`'s hidden
  `capture="environment"` input **synchronously** — no `await` in between.
- If `getUserMedia` rejects while the activation is still alive
  (`navigator.userActivation?.isActive`, or the rejection was immediate),
  the same tap opens that input.
- If it rejects after the activation has expired, `click()` would be a
  silent no-op and the Camera button would look dead. The viewfinder then
  remembers `cameraUnavailable` in memory for the rest of the page's life,
  and the Camera button's **next** tap goes straight to the OS input,
  synchronously. One muted line appears under the two controls in that
  state — *"Using your phone's camera."* — which is a status, not an error.

The one other message the viewfinder shows on its own is the resolution
hint from step 1, once per viewfinder session, as muted text under the
preview: *"Preview resolution. For the full camera resolution, use your
phone's camera."* On browsers that use `ImageCapture.takePhoto()` the hint
is not shown.

The stream is stopped — every track's `stop()` — on Done, on Esc, on a
backdrop click, and on the dialog's `close` event, through one exit
function, the same single-exit shape `barcode.js` uses so the camera
indicator never stays lit after the dialog is gone.

## What is deliberately not shared

`barcode.js` opens its own stream for live scanning. It is tempting to
lift "open a rear-camera stream into a dialog" into one module for both.
Not in this spec: the barcode sheet's stream exists to be *read* by a
detector on a timer and closes on the first hit; this one exists to be
*captured* on demand and stays open across shots. The overlap is ten
lines of `getUserMedia` and `stop()`. A shared abstraction over two
different lifecycles would need options both callers have to reason
about; two small copies with the same conventions do not. If a third
camera user appears, that is the moment to share.

## Strings

New keys in **both** catalogs (`19`): the viewfinder heading (with the
mode name interpolated), Shutter, Done, Flip camera, the shot count
(`.one`/`.other`), the resolution hint, the "Using your phone's camera."
status line, and the `aria-label` of the `<video>` ("Camera preview").
`36`'s Camera caption is unchanged.

## Acceptance criteria

- On a browser where `getUserMedia` exists and resolves, tapping `36`'s
  Camera control on `ingest.html` opens an in-page dialog with a live
  `<video>` preview, a Shutter button and a Done button, and the current
  capture mode's name in the heading. The page behind it — mode selector,
  location field, selection list — is not navigated away from or reloaded.
- The Shutter is disabled until `video.videoWidth > 0` and enabled from
  then on.
- Each Shutter press appends one `File` of type `image/jpeg` to `36`'s
  selection list without closing the dialog, on **both** still paths;
  three presses give three list rows, each with a thumbnail and a Remove
  button, the dialog's shot count reads 3, and the three names are
  distinct (`capture-1.jpg` … `capture-3.jpg`, continuing rather than
  restarting if the viewfinder is opened again).
- Done, Esc and a backdrop click each close the dialog, keep every shot
  already taken in the list, and leave every track of the stream with
  `readyState === "ended"`.
- Where the video track exposes `ImageCapture`, the still comes from
  `takePhoto()`, normalised to JPEG at the bitmap's own size; if
  `takePhoto()` rejects, that press uses the canvas grab. Where
  `ImageCapture` is absent, the still comes from a canvas at the video's
  intrinsic size and the resolution hint is shown once per viewfinder
  session.
- The scan sheet's Camera control is unaffected: it opens the OS input.
- Fallback: if `getUserMedia` is absent, or rejects, the person reaches
  `36`'s `capture="environment"` input — on the same tap when the tap's
  activation is still alive, on the next tap (with the status line shown)
  when it is not. No error is shown for a denied permission.
- Uploading a viewfinder photo goes through the same endpoint, the same
  per-photo request and the same server-side handling as any other
  photo; an EXIF-less JPEG is accepted.
- No literal colours; the dialog reuses the existing `card`, `stack`,
  `row` and `btn` tokens (`05`). The camera `<video>` has an accessible
  name (`getByLabel` finds it).
- `en.json` and `de.json` carry every new key, with identical key sets
  (`19`). `web/static/js/camera.js` is in `SHELL_ASSETS` and
  `CACHE_VERSION` is bumped (`05`).
- E2E setup (`e2e/playwright.config.js`): the Chromium project's existing
  `launchOptions.args` gain `--use-fake-device-for-media-stream` and
  `--use-fake-ui-for-media-stream` (a synthetic camera, permission
  auto-granted — no hardware, no prompt). These are Chromium's documented
  flags, and this repo has once been caught by a documented flag that did
  not do what it said (that file's own note on
  `--unsafely-treat-insecure-origin-as-secure`), so the package confirms
  them live before relying on them and records the result in its PR.
  Known from probing them in this repo's Playwright image while writing
  this spec: Chromium there has `ImageCapture`, its fake device answers
  `takePhoto()` with an `image/png` Blob, and `videoWidth` stays 0 for
  tens of milliseconds after `srcObject` is set — which is why step 2
  normalises, and why the Shutter waits for the first frame.
- E2E, `takePhoto` path (`e2e/specs/ingestion.spec.js`, the default
  Chromium configuration): open Camera; assert the dialog and its
  `<video>` are visible; `expect.poll` until the Shutter is enabled (which
  is `videoWidth > 0` — `toBeVisible()` on the video proves nothing about a
  frame); press Shutter twice; assert two rows exist with distinct names;
  press Done; assert the dialog is gone and the rows remain; then with
  route interception, Upload produces two multipart requests whose parts,
  read from `postDataBuffer()`, have `Content-Type: image/jpeg` and a
  non-empty body — which fails if PNG bytes were merely relabelled, since
  the part's bytes start with the JPEG SOI marker `FF D8`, and the test
  asserts that.
- E2E, canvas path: the same journey with
  `page.addInitScript(() => { delete window.ImageCapture; })`, plus: the
  resolution hint is visible after the first shot and its text appears
  exactly once after the second.
- E2E, stream released: an init script wraps `getUserMedia` to keep every
  stream it hands out on `window`; for each of the three exits (Done,
  Esc, backdrop click at the viewport corner) the test opens the
  viewfinder, takes one shot, exits that way, and asserts every retained
  track has `readyState === "ended"` and the shot's row is still there.
- E2E, fallback: with `getUserMedia` stubbed to reject immediately
  (`page.addInitScript`), tapping Camera does not open the dialog and a
  `page.waitForEvent("filechooser")` resolves with a chooser whose
  `element()` is `#photos-camera` — the chooser event, not a raw click,
  because a hidden input can receive a `click` that opens nothing once
  activation has expired. The expired-activation branch (the "next tap"
  rule and its status line) cannot be driven from Playwright without
  waiting out the activation window and is implementation-reviewed.
