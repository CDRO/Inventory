# 36 — Photo Source Picker: Library or Camera, Never One Forced on the Other

Depends on: [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
(`ingest.html`, the vendoring rule, `data-i18n` markup),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md) (the upload
endpoints, one photo per job), [`09-consumption-logging.md`](09-consumption-logging.md)
(the one camera entry point and its sticky mode),
[`19-localization.md`](19-localization.md) (every new string in both
catalogs), [`20-barcode-recall.md`](20-barcode-recall.md) (the scan sheet's
"photograph the barcode" path, the second call site).

Amends: `05`, "PWA" — the bullet that prescribes
`<input type="file" accept="image/*" capture="environment">` is replaced by
a pointer to this spec; `05`, "File layout" — gains `js/photo-picker.js`.
`20`, "Server-side decode fallback" — the scan
sheet's photo input becomes this spec's picker in single-photo mode. `09`
is unchanged: the mode selector, the sticky mode and the "one camera entry
point" all stay exactly as they are; this spec only changes how a photo
gets into that entry point.

## Why this spec exists

`ingest.html`'s photo field is one `<input type="file" accept="image/*"
capture="environment" multiple>`. `05` says `capture` "opens the rear camera
directly while still allowing a gallery pick". On the phone this PWA is
actually used on, the second half is false. **Observed** by the owner on
his own phone on 2026-09-25: tapping the field opens the camera and offers
no way to the photo library. That matches the documented behaviour of iOS
Safari, where `capture` means camera-only, and of Android Chrome. The
`multiple` attribute is ignored alongside `capture` (documented for iOS
Safari; the implementing package confirms it on Android when it verifies on
hardware, and records browser and OS versions for both claims in its PR so
the next person can tell whether they still hold). A photo already on the
phone — a shelf photographed earlier, a receipt-style shot someone sent, a
screenshot — cannot be uploaded at all.

Dropping `capture` is not the fix either: without it, every tap opens the
OS chooser ("Take Photo / Photo Library / Browse") and the frequent case —
you are standing in front of the shelf — costs one extra tap every time,
which is the friction `09` was written to remove.

Both cases are frequent enough to deserve their own control. This spec
replaces the single field with **two explicit controls**: one for the
photo library, one for the camera. It also fixes what the `multiple`
attribute was only pretending to do, by holding the selected photos in a
visible list that both controls add to, so one camera shot and two library
picks can be uploaded together.

The camera control here still opens the **OS camera** through
`capture="environment"`. An in-page viewfinder is a separate spec,
[`37-in-page-camera.md`](37-in-page-camera.md), which builds on this one and
falls back to it.

## Scope

A shared module, `web/static/js/photo-picker.js`, and two call sites:

1. `ingest.html` — the photo field of the upload form. Multi-photo mode.
2. The barcode scan sheet in `web/static/js/barcode.js` — the "photograph
   the barcode" path of `20`. Single-photo mode. It has the same defect
   for the same reason, and a barcode photographed earlier is as good a
   source as one taken now.

No backend change. No migration. The upload endpoints, their limits
(`04-backend-api-conventions.md`, `06`) and the EXIF handling are
untouched; a library JPEG or PNG goes through exactly the same
orientation-then-strip step as a camera photo, because the server cannot
tell them apart and must not try.

**Formats.** The server accepts JPEG and PNG only and answers anything
else with `422` ("Only JPEG and PNG images are supported",
`internal/httpapi/upload.go`). The old `capture` field only ever produced
camera JPEGs; a library pick can offer HEIC, WebP or AVIF. This spec keeps
"no backend change" and handles it at the input: the library input's
`accept` is `image/jpeg,image/png`, not `image/*`. On iOS that is what
makes Safari transcode a HEIC library photo to JPEG on pick (documented
iOS behaviour; the package confirms it on hardware and records the
version). On Android a HEIC or WebP pick still reaches the server and gets
the `422`; that photo's upload card then shows the server's message as
"Not uploaded", exactly as any rejected upload does today, while the other
photos of the run upload normally. That is the accepted behaviour, stated
here so nobody reads the error card as a bug. The camera input keeps
`image/*`: a camera produces JPEG.

## The picker

`mountPhotoPicker(container, {multiple, onChange})` renders, inside
`container`:

- **Two controls, side by side**, each a `<button type="button">` with an
  inline SVG icon and a short visible caption beneath it:
  - **Library** — a folder icon, caption "Photos", `data-role="pick-library"`.
    Activates a hidden `<input type="file" id="photos-library"
    accept="image/jpeg,image/png">` with `multiple` when the picker is in
    multi-photo mode and **without `capture`**. The browser shows its
    ordinary chooser; on iOS that is the library and Files, on Android the
    system picker.
  - **Camera** — a camera icon, caption "Camera", `data-role="pick-camera"`.
    Activates a hidden `<input type="file" id="photos-camera"
    accept="image/*" capture="environment">`, always without `multiple`:
    `capture` takes one shot, and the list below is what makes a second
    shot possible. (In multi-photo mode, [`37-in-page-camera.md`](37-in-page-camera.md)
    may put a viewfinder in front of this input; in single-photo mode the
    control always opens this input.)
  - The ids and `data-role`s above are part of the contract: the E2E
    below and any later spec address the inputs and buttons by them, not
    by whatever an implementation happens to choose. The selection list
    is `#photo-list`.
- Both buttons meet a 44×44 CSS-pixel touch target. `05` only says
  "usable one-handed on a phone"; 44 px is this repo's own `.btn` minimum
  height (`components.css`, `min-height: 2.75rem`), written down here so
  it is a number a test can check rather than a judgement. The icons are
  inline `<svg aria-hidden="true">`
  elements in the page or module — no icon font, no image file, no CDN
  (`05`'s vendoring rule). Their colour is `currentColor`, so they follow
  the button's tokens; no literal colour values (`05`).
- Each button's accessible name is its caption, set with
  `data-i18n`/`data-i18n-aria-label` per `19`, so it is announced as
  "Photos, button" / "Camera, button". The hidden inputs carry no label of
  their own; they are implementation detail.
- The two hidden inputs are `hidden` (not visually hidden off-screen):
  they are never the thing a person interacts with, and a screen reader
  must not find a bare "file" field beside the two named buttons.

### The selection list (multi-photo mode)

- Every file that arrives through either input is **appended** to the
  picker's selection. Selecting again — from either source — adds, never
  replaces. This is the only way "one camera shot, then two from the
  library" can reach the same upload.
- **Each input is drained on every `change`**: the handler copies
  `Array.from(input.files)` into the selection and then sets
  `input.value = ""` at once, so the input never holds anything between
  picks. This is not tidiness. A file input fires no `change` when the
  same file is chosen again while it still holds it, so without the reset
  "remove `shelf.jpg`, pick `shelf.jpg` again" would silently do nothing
  until the next Upload. The copy comes first because `input.files` is a
  live `FileList` that empties with the reset. (The same trap covers a
  second camera shot on a platform that names every capture `image.jpg`.)
- **Rows are keyed by `File` identity, never by name.** Names are not
  unique — iOS hands out generic ones, and `37`'s captures are numbered
  per picker lifetime — so Remove removes the object it was rendered for,
  not "the row called `shelf.jpg`".
- The selection is rendered as a list (`#photo-list`) under the two
  controls: one row per photo with a small thumbnail
  (`URL.createObjectURL`, revoked when the row is removed or the list
  cleared), the file name, and a **Remove** button whose accessible name
  states the position as well as the name ("Remove photo 2 of 3,
  shelf.jpg"), since two rows can share a name. Names are user text:
  `textContent` only.
- A count line ("3 photos selected") replaces the old field label as the
  thing that tells the user what will be uploaded. The upload button is
  disabled while the selection is empty, replacing the input's
  `required` attribute.
- `onChange(files)` is called with the current array after every change.
  The caller reads the selection from the picker (`picker.files()`), never
  from either input's `.files`, which are empty between picks by the rule
  above.
- `picker.clear()` empties the selection and revokes every object URL.
  `ingest.js` calls it where it calls `form.reset()` today.

### Single-photo mode

`multiple: false` (the scan sheet): no list. Picking from either source
calls `onChange([file])` at once and the caller acts on it, exactly as the
sheet acts on its input's `change` event today. The two buttons stay; the
selection UI does not appear. `37`'s viewfinder does not apply here: the
sheet is itself a modal dialog, has no list to append to, and a live
camera for a barcode is `20`'s own scan path — the sheet's Camera control
always opens the OS camera input.

### What a page reload costs

The selection lives in memory. Some Android browsers reload the page on
return from the OS camera (reported by the owner, not measured here); a
reload loses whatever was selected before that shot. This spec accepts
that: keeping `File` objects across a reload would mean writing blobs to
IndexedDB and reading them back, and the frequent case — pick, upload —
never reloads. The Camera control's own next step,
[`37-in-page-camera.md`](37-in-page-camera.md), removes the round trip that
causes it. What the picker does guarantee is that nothing is uploaded
without an explicit Upload press, so a reload can only lose a selection,
never send half of one.

### Behaviour that does not change

- `ingest.js` still uploads one request per photo, one after another, and
  still resets to the sticky mode afterwards (`09`).
- The barcode miss path (`onUnknownCode` in `ingest.js`) moves keyboard
  focus to the picker's **Camera** button instead of the old input: "take
  a photo of the item" is what the message says, so the camera is the
  right control to land on.
- The scan sheet's decode call, its in-memory-only handling and its
  `422` on nothing-decodable (`20`) are untouched; only the way the photo
  is chosen changes.

## Visual placement on `ingest.html`

The picker replaces the "Photos" field in place — below the location
field and the barcode affordance, above the Upload button — so `20`'s
"beside the shutter" placement of the barcode button still reads
correctly: the two picker buttons are the shutter now.

## Strings

New keys in **both** `en.json` and `de.json` (`19`): the two captions, the
count line (with `.one`/`.other` forms), the Remove button's accessible
name (position and file name: "Remove photo 2 of 3, shelf.jpg"), and the empty
state ("No photos selected yet"). `ingest.photos.label` is removed with the
field it labelled. `barcode.photographLabel` ("Or photograph the barcode")
is kept: it becomes the caption above the sheet's two controls instead of
the `<label>` of an input that no longer exists.

## What the implementing package touches besides the above

So nothing is left citing the old rule: the comment above the input in
`web/static/ingest.html` (it cites `05`'s replaced bullet), that input's
`<label for="photos">` and `required`, `barcode.js`'s input and its label,
`ingest.js`'s reads of `photosInput` (`onSubmit`, `form.reset()`, the
`.focus()` in `onUnknownCode`), and `e2e/specs/ingestion.spec.js`, whose
upload journey addresses `#photos` and must address the picker's library
input instead.

## Acceptance criteria

- On `ingest.html`, the photo field is two buttons, "Photos" and
  "Camera", each with a visible icon and caption and a 44×44 CSS-pixel
  minimum hit area. There is no visible bare `<input type="file">`.
- `#photos-library` has `accept="image/jpeg,image/png"` and `multiple`,
  and has **no** `capture` attribute. `#photos-camera` has
  `accept="image/*"` and `capture="environment"`, and has **no** `multiple`
  attribute. The two buttons carry `data-role="pick-library"` and
  `data-role="pick-camera"`.
- Photos chosen through either input are appended to one selection list;
  choosing again from either source adds to it and never replaces it. Each
  row shows a thumbnail, the file name, and a Remove button that removes
  exactly that photo — the one it was rendered for, even when two rows
  share a name.
- Immediately after every pick, the input that was used reports
  `files.length === 0` (it was drained into the selection and reset), so a
  file removed from the list can be picked again from the same input and
  is added again.
- The Upload button is disabled while the selection is empty and enabled
  once it holds at least one photo.
- Submitting uploads exactly the photos in the selection, one request per
  photo, in list order, to the endpoint the current mode selects (`09`).
  Neither input's own `.files` is what gets uploaded.
- After a submission the selection is empty (`#photo-list` has no rows,
  Upload is disabled again, both inputs report `files.length === 0`).
  That every thumbnail's object URL was revoked is implementation-reviewed,
  not E2E-observable.
- The barcode scan sheet offers the same two controls for its photograph
  path; either source hands the photo to `POST …/barcodes/decode`
  unchanged.
- `barcode.js` and `ingest.html` contain no `<input type="file">` other
  than the ones the picker renders, and no comment in `web/static` still
  says `capture` allows a gallery pick.
- `en.json` and `de.json` carry every new key, with identical key sets
  (`19`).
- `web/static/js/photo-picker.js` is in `SHELL_ASSETS` in `sw.js` and
  `CACHE_VERSION` is bumped (`05`) — an installed client that kept the old
  shell would keep the old single field, silently.
- E2E (`e2e/specs/ingestion.spec.js`; its existing upload journey
  addresses `#photos`, which no longer exists, and is migrated to
  `#photos-library`): Upload is disabled on load; `setInputFiles` on
  `#photos-library` with two files enables it and gives two rows;
  `setInputFiles` on `#photos-camera` with one file gives three; after each
  pick the used input reports `files.length === 0`
  (`toHaveJSProperty`); removing the middle row leaves two; with route
  interception, pressing Upload produces exactly two multipart requests —
  counted only once the uploads list has settled (two cards, selection
  empty), since uploads are sequential and a regression that sent a third
  file would send it after the second completes — whose file names, read
  from `route.request().postDataBuffer()` (multipart `Content-Disposition`
  `filename=`; `postData()` mangles binary bodies), are the two remaining
  ones in list order. The test also asserts the two inputs' attributes as
  stated above, so a regression to a single `capture` input fails it.
  Re-selecting the same file through `setInputFiles` is **not** evidence of
  anything: Playwright fires `change` for it regardless, unlike a browser;
  the `files.length === 0` assertion is what guards the reset.
- E2E: on the scan sheet, `setInputFiles` on the library input sends the
  file to the decode route (route-intercepted); the response's code reaches
  the same lookup as a typed one.
