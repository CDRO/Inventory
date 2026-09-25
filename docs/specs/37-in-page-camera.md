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
- a **shutter** button (large, centred below the preview, ≥ 56 CSS px);
- a **Done** button that closes the viewfinder and keeps everything taken;
- the running count of shots taken in this viewfinder session, and — so
  `09`'s "the current mode is always visible while the camera is open"
  holds here too — the current mode's name ("Shelf scan") in the dialog's
  heading;
- if the device reports more than one camera, a **flip** button that
  switches `facingMode` between `environment` and `user`. No zoom, no
  torch, no exposure controls: the OS camera is one tap away for anything
  fancier, and every control here is one more thing to test on hardware.

Each shutter press:

1. Grabs a full-resolution still. Where `ImageCapture` exists on the video
   track, `ImageCapture.takePhoto()` is used, because it returns the
   sensor's photo resolution rather than the preview stream's. Otherwise
   the current frame is drawn onto a `<canvas>` at
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
2. Encodes it as JPEG (`canvas.toBlob("image/jpeg", 0.92)`, or the Blob
   `takePhoto()` returns) and wraps it in a `File` named
   `capture-<n>.jpg`.
3. **Appends** it to `36`'s selection list, exactly as a library pick
   would — nothing is uploaded from the viewfinder itself. The list is the
   one place a photo waits before upload; the viewfinder only produces
   files.
4. Shows a brief capture confirmation (the preview dims for ~150 ms and
   the count increments) and keeps the viewfinder open for the next shot.

Done closes the dialog; the shots are in the list, thumbnails and Remove
buttons included, and Upload sends them like any other selection. Cancel
(Esc, backdrop) also keeps every shot already taken: a photo taken is a
photo the person wanted, and losing three shelves to a mis-tap on the
backdrop is worse than one extra Remove.

A captured JPEG has no EXIF; orientation is already baked into the pixels
by the browser. The server's orientation-then-strip step (`04`) therefore
has nothing to do for these files and must not break on that — it already
handles EXIF-less uploads, since a stripped re-upload is one.

## Fallback — `36` is always underneath

The viewfinder is offered only when **all** of these hold, checked at the
moment Camera is tapped, never assumed from a page-load check:

- `navigator.mediaDevices.getUserMedia` exists (which implies a secure
  context — the PWA is served over HTTPS in the E2E stack and behind
  Traefik in production; a plain-HTTP origin on a home network simply
  gets the OS camera, silently, and that is correct);
- `getUserMedia` resolves — permission granted and a camera present.

If any of them fails, **the same tap** falls through to `36`'s hidden
`capture="environment"` input, so the person sees the OS camera open,
not an error. A denied permission is not reported as an error either:
the OS camera is the answer, and it needs no browser permission. The one
message the viewfinder shows on its own is the resolution hint above,
once per viewfinder session, as muted text under the preview: *"Preview
resolution. For the full camera resolution, use your phone's camera."*
On browsers that use `ImageCapture.takePhoto()` the hint is not shown.

The stream is stopped — every track's `stop()` — on Done, on Cancel, on
Esc, and on the dialog's `close` event, through one exit function, the
same single-exit shape `barcode.js` uses so the camera indicator never
stays lit after the dialog is gone.

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
(`.one`/`.other`), the resolution hint, and the `aria-label` of the
`<video>` ("Camera preview"). `36`'s Camera caption is unchanged.

## Acceptance criteria

- On a browser where `getUserMedia` exists and resolves, tapping `36`'s
  Camera control opens an in-page dialog with a live `<video>` preview,
  a Shutter button and a Done button, and the current capture mode's name
  in the heading. The page behind it — mode selector, location field,
  selection list — is not navigated away from.
- Each Shutter press appends one `image/jpeg` `File` to `36`'s selection
  list without closing the dialog; three presses give three list rows,
  each with a thumbnail and a Remove button, and the dialog's shot count
  reads 3.
- Done, Cancel, Esc and a backdrop click all close the dialog, keep every
  shot already taken in the list, and stop every track of the stream.
- Where the video track exposes `ImageCapture`, the still comes from
  `takePhoto()`; otherwise from a canvas at the video's intrinsic size,
  and the resolution hint is shown once.
- If `getUserMedia` is absent, or rejects (permission denied, no camera),
  the same tap activates `36`'s `capture="environment"` input instead. No
  error is shown for a denied permission.
- Uploading a viewfinder photo goes through the same endpoint, the same
  per-photo request and the same server-side handling as any other
  photo; an EXIF-less JPEG is accepted.
- No literal colours; the dialog reuses the existing `card`, `stack`,
  `row` and `btn` tokens (`05`). The camera `<video>` has an accessible
  name.
- `en.json` and `de.json` carry every new key, with identical key sets
  (`19`). `web/static/js/camera.js` is in `SHELL_ASSETS` and
  `CACHE_VERSION` is bumped (`05`).
- E2E (`e2e/specs/ingestion.spec.js`): the Chromium project's
  `launchOptions.args` gain `--use-fake-device-for-media-stream` and
  `--use-fake-ui-for-media-stream` (a synthetic camera, permission
  auto-granted — no hardware, no prompt). These are Chromium's documented
  flags, and this repo has once been caught by a documented flag that did
  not do what it said (`playwright.config.js`'s own note on
  `--unsafely-treat-insecure-origin-as-secure`), so the package confirms
  them live before relying on them and records the result in its PR. The
  journey proves whichever still path desktop Chromium takes with the
  fake device — it asserts the files produced, not the branch: it opens
  Camera, asserts the dialog and its `<video>` are visible, presses
  Shutter twice, asserts two list rows exist, presses Done, asserts the
  dialog is gone and the rows remain; then with route interception,
  Upload produces two multipart requests whose files are `image/jpeg` and
  non-empty. The once-only resolution hint is asserted only when the test
  finds `ImageCapture` absent (`page.evaluate(() => "ImageCapture" in
  window)`), since that is the only case in which it appears.
- E2E, fallback: with `getUserMedia` stubbed to reject
  (`page.addInitScript`), tapping Camera does not open the dialog, and
  the `capture="environment"` input receives a `click` (asserted through
  a listener installed by the same init script).
