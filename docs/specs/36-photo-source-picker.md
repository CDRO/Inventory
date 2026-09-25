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
a pointer to this spec. `20`, "Server-side decode fallback" — the scan
sheet's photo input becomes this spec's picker in single-photo mode. `09`
is unchanged: the mode selector, the sticky mode and the "one camera entry
point" all stay exactly as they are; this spec only changes how a photo
gets into that entry point.

## Why this spec exists

`ingest.html`'s photo field is one `<input type="file" accept="image/*"
capture="environment" multiple>`. `05` says `capture` "opens the rear camera
directly while still allowing a gallery pick". On the phones this PWA is
actually used on, the second half is false: with `capture` present, iOS
Safari opens the camera and offers **no** way to the photo library, and
Android Chrome does the same. The `multiple` attribute is ignored in that
mode as well, so the field cannot even take several shots in one go. A
photo already on the phone — a shelf photographed earlier, a receipt-style
shot someone sent, a screenshot — cannot be uploaded at all.

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
untouched; a library photo goes through exactly the same
orientation-then-strip step as a camera photo, because the server cannot
tell them apart and must not try.

## The picker

`mountPhotoPicker(container, {multiple, onChange})` renders, inside
`container`:

- **Two controls, side by side**, each a `<button type="button">` with an
  inline SVG icon and a short visible caption beneath it:
  - **Library** — a folder icon, caption "Photos". Activates a hidden
    `<input type="file" accept="image/*">` with `multiple` when the picker
    is in multi-photo mode and **without `capture`**. The browser shows
    its ordinary chooser; on iOS that is the library and Files, on Android
    the system picker.
  - **Camera** — a camera icon, caption "Camera". Activates a hidden
    `<input type="file" accept="image/*" capture="environment">`, always
    without `multiple`: `capture` takes one shot, and the list below is
    what makes a second shot possible.
- Both buttons meet a 44×44 CSS-pixel touch target (`05`: usable
  one-handed on a phone). The icons are inline `<svg aria-hidden="true">`
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
- The selection is rendered as a list under the two controls: one row per
  photo with a small thumbnail (`URL.createObjectURL`, revoked when the row
  is removed or the list cleared), the file name, and a **Remove** button.
  Names are user text: `textContent` only.
- A count line ("3 photos selected") replaces the old field label as the
  thing that tells the user what will be uploaded. The upload button is
  disabled while the selection is empty, replacing the input's
  `required` attribute.
- `onChange(files)` is called with the current array after every change.
  The caller reads the selection from the picker (`picker.files()`), never
  from either input's `.files`, which only ever hold the last pick.
- `picker.clear()` empties the selection, revokes every object URL and
  resets both inputs so the same file can be chosen again. `ingest.js`
  calls it where it calls `form.reset()` today.

### Single-photo mode

`multiple: false` (the scan sheet): no list. Picking from either source
calls `onChange([file])` at once and the caller acts on it, exactly as the
sheet acts on its input's `change` event today. The two buttons stay; the
selection UI does not appear.

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
name (which includes the file name: "Remove shelf.jpg"), and the empty
state ("No photos selected yet"). `ingest.photos.label` is removed with the
field it labelled.

## Acceptance criteria

- On `ingest.html`, the photo field is two buttons, "Photos" and
  "Camera", each with a visible icon and caption and a 44×44 CSS-pixel
  minimum hit area. There is no visible bare `<input type="file">`.
- The library input has `accept="image/*"` and `multiple`, and has **no**
  `capture` attribute. The camera input has `accept="image/*"` and
  `capture="environment"`, and has **no** `multiple` attribute.
- Photos chosen through either input are appended to one selection list;
  choosing again from either source adds to it and never replaces it. Each
  row shows a thumbnail, the file name, and a Remove button that removes
  exactly that photo.
- The Upload button is disabled while the selection is empty and enabled
  once it holds at least one photo.
- Submitting uploads exactly the photos in the selection, one request per
  photo, in list order, to the endpoint the current mode selects (`09`).
  Neither input's own `.files` is what gets uploaded.
- After a submission the selection is empty, its thumbnails' object URLs
  are revoked, and the same file can be selected again.
- The barcode scan sheet offers the same two controls for its photograph
  path; either source hands the photo to `POST …/barcodes/decode`
  unchanged.
- `barcode.js` and `ingest.html` contain no `<input type="file">` other
  than the ones the picker renders. `docs/specs/05` no longer prescribes a
  single `capture` input.
- `en.json` and `de.json` carry every new key, with identical key sets
  (`19`).
- `web/static/js/photo-picker.js` is in `SHELL_ASSETS` in `sw.js` and
  `CACHE_VERSION` is bumped (`05`) — an installed client that kept the old
  shell would keep the old single field, silently.
- E2E (`e2e/specs/ingestion.spec.js`, the existing upload journey
  extended): `setInputFiles` on the library input with two files, then on
  the camera input with one, gives a list of three rows; removing the
  middle one leaves two; with Playwright route interception, pressing
  Upload produces exactly two multipart requests whose file names are the
  two remaining ones, in list order. The test also asserts the two inputs'
  attributes as stated above, so a regression to a single `capture` input
  fails it.
- E2E: on the scan sheet, `setInputFiles` on the library input sends the
  file to the decode route (route-intercepted); the response's code reaches
  the same lookup as a typed one.
