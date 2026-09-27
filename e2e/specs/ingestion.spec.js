// Photo ingestion in the browser (docs/specs/06-vision-shelf-ingestion.md),
// required journey 4 of docs/specs/05-frontend-pwa-foundations.md as far as a
// stack without a vision model allows.
//
// The E2E stack's GEMINI_API_KEY is a placeholder, so no photo is ever really
// analysed here. What that leaves testable, and tested: the upload screen
// answering an unavailable model as a configuration problem, and — from
// proposals seeded in e2e/fixtures/seed.sql in exactly the shape the analysis
// job writes — the inbox, the review screen, confirming into real inventory,
// and discarding.

import { test, expect } from "@playwright/test";

const HOUSEHOLD = "00000000-0000-7000-8000-000000000010";
const SHELF_JOB = "00000000-0000-7000-8000-000000000070";
const PRODUCT_JOB = "00000000-0000-7000-8000-000000000071";
const LOOK_ONLY_JOB = "00000000-0000-7000-8000-000000000072";
const LOCATION_MODAL_JOB = "00000000-0000-7000-8000-000000000074";
const ZERO_LOCATIONS_JOB = "00000000-0000-7000-8000-000000000075";
const ZERO_LOCATIONS_HOUSEHOLD = "00000000-0000-7000-8000-000000000012"; // "E2E Admin Household"
const TOMATOES = "00000000-0000-7000-8000-000000000040";
const PANTRY = "00000000-0000-7000-8000-000000000020";
const CANNED_GOODS = "00000000-0000-7000-8000-000000000030";

// Stand-in photo bytes. The upload is refused on the model check, which runs
// before the body is read, so these never need to be a decodable image.
const TINY_JPEG = Buffer.from(
  "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=",
  "base64",
);

// A real, decodable 1×1 PNG, served as a job's photo where the review screen
// draws its crops from it.
const TINY_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
  "base64",
);

async function logInAsBob(page) {
  const res = await page.request.post("/api/auth/login", {
    data: { username: "e2e-bob", password: "e2e-fixture-password" },
  });
  expect(res.status()).toBe(200);
}

test("uploading without a usable model says so, as a configuration problem", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  await page.locator("#photos-library").setInputFiles({ name: "shelf.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG });
  await page.getByRole("button", { name: "Upload" }).click();

  const card = page.locator("#uploads .card").first();
  // The model check lists models with a placeholder key, which can take a
  // moment to fail; the answer is still a 503, whatever the network does.
  await expect(card).toContainText("Not uploaded", { timeout: 20_000 });
  await expect(card).toContainText("AI model is unavailable");
});

// docs/specs/36-photo-source-picker.md's own E2E acceptance criteria, plus
// #205 item 10 (the 44x44 hit area and one-thumbnail-per-row rule, folded in
// here rather than as a separate journey since it needs the same picker
// already on screen).
test("the two-control picker appends photos from both sources into one list, drains each input on pick, and uploads exactly what survives a removal", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const libraryInput = page.locator("#photos-library");
  const cameraInput = page.locator("#photos-camera");
  const uploadButton = page.getByRole("button", { name: "Upload" });
  const rows = page.locator("#photo-list li");

  // The contract's own shape — a regression to a single `capture` input, or
  // to `multiple` on the wrong one, fails here rather than downstream.
  await expect(libraryInput).toHaveAttribute("accept", "image/jpeg,image/png");
  expect(await libraryInput.getAttribute("multiple")).not.toBeNull();
  expect(await libraryInput.getAttribute("capture")).toBeNull();
  await expect(cameraInput).toHaveAttribute("accept", "image/*");
  await expect(cameraInput).toHaveAttribute("capture", "environment");
  expect(await cameraInput.getAttribute("multiple")).toBeNull();
  // "There is no visible bare <input type='file'>" — the acceptance
  // criterion is about what a person can see, not just that the two named
  // ids exist; both real inputs carry the `hidden` attribute.
  await expect(page.locator('input[type="file"]:not([hidden])')).toHaveCount(0);

  await expect(uploadButton).toBeDisabled();

  await libraryInput.setInputFiles([
    { name: "shelf-a.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG },
    { name: "shelf-b.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG },
  ]);
  await expect(uploadButton).toBeEnabled();
  await expect(rows).toHaveCount(2);
  // Drained into the selection and reset — not merely emptied by Playwright
  // re-firing change, which it does regardless of whether the reset happened.
  expect(await libraryInput.evaluate((el) => el.files.length)).toBe(0);

  await cameraInput.setInputFiles({ name: "shelf-c.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG });
  await expect(rows).toHaveCount(3);
  expect(await cameraInput.evaluate((el) => el.files.length)).toBe(0);

  // Removing the middle row (shelf-b.jpg) leaves shelf-a.jpg and shelf-c.jpg,
  // in that order. The exact accessible name — not a "Remove" substring —
  // pins the position-and-name contract ("Remove photo 2 of 3, shelf-b.jpg").
  await rows.nth(1).getByRole("button", { name: "Remove photo 2 of 3, shelf-b.jpg", exact: true }).click();
  await expect(rows).toHaveCount(2);

  // #205 item 10, first half: both controls meet the 44x44 CSS-pixel minimum
  // hit area — this repo's own `.btn` floor (components.css), written down as
  // a number here rather than a judgement call.
  for (const role of ["pick-library", "pick-camera"]) {
    const box = await page.locator(`[data-role="${role}"]`).boundingBox();
    expect(box.width, `${role} width`).toBeGreaterThanOrEqual(44);
    expect(box.height, `${role} height`).toBeGreaterThanOrEqual(44);
  }
  // #205 item 10, second half: one thumbnail per row, never two sharing one —
  // the row count above already proved two rows, this proves each has
  // exactly one <img>.
  await expect(page.locator("#photo-list img[src^='blob:']")).toHaveCount(2);

  // Counted only once the uploads list has settled (two cards, selection
  // empty): uploads are sequential, and a regression that sent a third file
  // would send it only after the second completes, which a count taken right
  // after the click would still pass.
  const requests = [];
  await page.route("**/ingest/shelf-photos", async (route) => {
    requests.push(route.request());
    await route.fulfill({ json: { job_id: "00000000-0000-7000-8000-0000000000aa" } });
  });

  await uploadButton.click();
  await expect(page.locator("#uploads .card")).toHaveCount(2);
  await expect(rows).toHaveCount(0);
  await expect(uploadButton).toBeDisabled();
  expect(await libraryInput.evaluate((el) => el.files.length)).toBe(0);
  expect(await cameraInput.evaluate((el) => el.files.length)).toBe(0);

  expect(requests).toHaveLength(2);
  // Multipart Content-Disposition filename= — postData() mangles a binary
  // body, postDataBuffer() does not.
  const filenames = requests.map((req) => {
    const match = req.postDataBuffer().toString("latin1").match(/filename="([^"]*)"/);
    return match ? match[1] : null;
  });
  expect(filenames).toEqual(["shelf-a.jpg", "shelf-c.jpg"]);

  await page.unroute("**/ingest/shelf-photos");
});

// docs/specs/36-photo-source-picker.md and issue #206's own "Watch for"
// section both call this out by name: "Rows are keyed by File identity,
// never by name" — iOS hands out generic filenames, so two rows can share
// one. The main journey above never exercises this because its three fixture
// files all have distinct names; a regression to name-keyed removal (finding
// by `.indexOf`/`.filter` on `file.name` instead of the `File` reference) would
// pass every assertion there while silently removing the wrong photo, or
// both, or neither, whenever two picks happen to share a name.
test("removing a photo acts on File identity, not filename — two rows can share a name", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const libraryInput = page.locator("#photos-library");
  const rows = page.locator("#photo-list li");

  // Same name, different bytes (a JPEG and a PNG) — indistinguishable by
  // filename, easy to tell apart by content once uploaded.
  await libraryInput.setInputFiles([
    { name: "shelf.jpg", mimeType: "image/jpeg", buffer: TINY_JPEG },
    { name: "shelf.jpg", mimeType: "image/png", buffer: TINY_PNG },
  ]);
  await expect(rows).toHaveCount(2);

  // Removing the *second* "shelf.jpg" row on purpose, not the first: a
  // regression to name-keyed removal (e.g. `findIndex(f => f.name ===
  // file.name)`) finds the first match regardless of which row's button was
  // clicked, so asking it to remove row 2 would silently remove row 1
  // instead and this test would still see "one row left, named shelf.jpg" —
  // indistinguishable from correct behaviour unless the survivor's actual
  // content is checked below. Its accessible name is itself proof identity
  // is in play — "1 of 2" and "2 of 2" differ even though the visible
  // filename does not.
  await rows.nth(1).getByRole("button", { name: "Remove photo 2 of 2, shelf.jpg", exact: true }).click();
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("shelf.jpg");
  await expect(rows.first().getByRole("button")).toHaveAccessibleName("Remove photo 1 of 1, shelf.jpg");

  // The strongest proof: uploading now sends the surviving (first-picked)
  // File's own bytes — the JPEG — rather than the removed second one's — the
  // PNG. A name-keyed regression that removed row 1 instead would upload the
  // PNG here, which the assertions below would catch.
  let request = null;
  await page.route("**/ingest/shelf-photos", async (route) => {
    request = route.request();
    await route.fulfill({ json: { job_id: "00000000-0000-7000-8000-0000000000ac" } });
  });
  await page.getByRole("button", { name: "Upload" }).click();
  await expect(page.locator("#uploads .card")).toHaveCount(1);

  expect(request, "expected the surviving photo to upload").not.toBeNull();
  const body = request.postDataBuffer().toString("latin1");
  expect(body).toContain("Content-Type: image/jpeg");
  expect(body).not.toContain("Content-Type: image/png");

  await page.unroute("**/ingest/shelf-photos");
});

// docs/specs/36-photo-source-picker.md's E2E acceptance criteria for the scan
// sheet: the same picker's library input, feeding the decode route, must
// reach the same code lookup a typed digit string does.
test("the barcode scan sheet's photograph control decodes through the same lookup as a typed code", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);
  // Shelf scan (the default mode) has no barcode affordance at all — a shelf
  // is not a barcode — so a mode that has one has to be picked first.
  await page.locator('input[name="mode"][value="stocking_up"]').check();

  let decodeRequest = null;
  await page.route("**/barcodes/decode", async (route) => {
    decodeRequest = route.request();
    await route.fulfill({ json: { barcode: "4006381333931" } });
  });

  await page.getByRole("button", { name: "Scan a barcode" }).click();
  const dialog = page.locator("dialog[aria-labelledby='barcode-sheet-title']");
  await expect(dialog).toBeVisible();

  // Only the sheet's library input has no `capture` attribute — its own
  // camera input does, exactly as the multi-photo picker's does.
  await dialog.locator('input[type="file"]:not([capture])').setInputFiles({
    name: "code.jpg",
    mimeType: "image/jpeg",
    buffer: TINY_JPEG,
  });

  expect(decodeRequest, "expected the photo to reach the decode route").not.toBeNull();

  // HOUSEHOLD has never associated this code, so the decoded string reaches
  // exactly the miss path a typed one would (docs/specs/20-barcode-recall.md;
  // e2e/specs/barcode-recall.spec.js covers that lookup's own contract at the
  // API level) — proof the photo path hands its code through openScanSheet's
  // one finish() exit rather than a parallel one.
  await expect(dialog).toHaveCount(0);
  await expect(page.locator("#error")).toContainText("not known here yet");

  await page.unroute("**/barcodes/decode");
});

// --- docs/specs/37-in-page-camera.md: the in-page viewfinder -----------------
//
// These journeys run under the Chromium fake camera that
// e2e/playwright.config.js turns on (--use-fake-device-for-media-stream and
// --use-fake-ui-for-media-stream: a synthetic camera, permission auto-granted,
// no hardware and no prompt). Both flags were confirmed live before anything
// here was written to depend on them, and that file records the measurements.
//
// The one that matters most for reading the assertions below: the fake device
// answers `takePhoto()` with an **image/png** Blob. That is what makes the
// JPEG SOI checks real rather than tautological — the source bytes genuinely
// are not JPEG, so a `type: "image/jpeg"` label slapped onto them without a
// re-encode fails here rather than downstream on a phone.
//
// Not verifiable in this stack, and left as #205 records them: sensor
// resolution kept through normalisation (the fake device gives 640x480 on both
// paths, so canvas-at-bitmap-size and canvas-at-preview-size are
// indistinguishable), EXIF orientation via `imageOrientation: "from-image"`
// (the fake PNG carries none), and the expired-activation branch of the
// fallback (it cannot be driven without waiting out the activation window).
//
// The flip button is NOT on that list, though #205 puts it there. The fake
// device does report one videoinput, but `enumerateDevices()` is as
// overridable as `getUserMedia` — so the flip *logic* is driven by a journey
// below, and only the question of whether the second stream is a genuinely
// different physical camera is device-only. That journey's own comment says
// which half is which.

const VIEWFINDER = 'dialog[data-role="viewfinder"]';
const PICKER_STATUS = '#photo-picker-field [data-role="status"]';

// retainStreams keeps every MediaStream getUserMedia hands out on `window`, so
// the tracks can be inspected after the dialog is gone. It must be an init
// script: the page's own module graph is what calls getUserMedia, so wrapping
// it after the page has loaded would wrap nothing that the viewfinder uses.
async function retainStreams(page) {
  await page.addInitScript(() => {
    window.__streams = [];
    const original = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = async (constraints) => {
      const stream = await original(constraints);
      window.__streams.push(stream);
      return stream;
    };
  });
}

// openViewfinder taps Camera and waits until the Shutter is actually usable.
//
// The wait is on the Shutter's enabled state, not on the video being visible:
// a <video> is laid out and "visible" tens of milliseconds before it has a
// frame (~59 ms, measured), and `toBeVisible()` on it proves nothing about
// one. The Shutter's enabled state IS `videoWidth > 0`, which is the only
// observable proof that a canvas grab would now produce pixels rather than a
// null blob.
async function openViewfinder(page) {
  await page.locator('[data-role="pick-camera"]').click();
  const dialog = page.locator(VIEWFINDER);
  await expect(dialog).toBeVisible();
  await expect(dialog.locator('[data-role="shutter"]')).toBeEnabled({ timeout: 10_000 });
  return dialog;
}

// multipartParts reads each part's declared Content-Type and the first two
// bytes of its body out of a captured request.
//
// latin1 throughout: postDataBuffer() holds raw bytes, and latin1 is the only
// encoding that round-trips an arbitrary byte to one JS character and back.
// Decoding as utf-8 would mangle 0xFF 0xD8 into a replacement character and
// the SOI assertions would then be checking the decoder, not the upload.
function multipartParts(request) {
  const body = request.postDataBuffer().toString("latin1");
  const parts = [];
  const header = /Content-Type: (\S+)\r\n\r\n/g;
  let match;
  while ((match = header.exec(body)) !== null) {
    const start = match.index + match[0].length;
    const end = body.indexOf("\r\n--", start);
    parts.push({
      contentType: match[1],
      magic: [...body.slice(start, start + 2)].map((c) => c.charCodeAt(0).toString(16).padStart(2, "0")).join(" "),
      length: end === -1 ? body.length - start : end - start,
    });
  }
  return parts;
}

// expectJpegUpload asserts one captured request carries exactly one image
// part, that it declares image/jpeg, and that its bytes actually begin with
// the JPEG SOI marker FF D8 and are not empty.
function expectJpegUpload(request, label) {
  const parts = multipartParts(request);
  expect(parts, `${label}: one image part per request`).toHaveLength(1);
  expect(parts[0].contentType, `${label}: declared type`).toBe("image/jpeg");
  // The assertion PNG-relabelled-as-JPEG fails on. The fake camera's
  // takePhoto() hands back PNG (magic 89 50), so this is the check that the
  // normalisation in js/camera.js genuinely re-encoded rather than renamed.
  expect(parts[0].magic, `${label}: JPEG SOI bytes`).toBe("ff d8");
  expect(parts[0].length, `${label}: non-empty body`).toBeGreaterThan(0);
}

test("the viewfinder captures through takePhoto(), numbers its shots, keeps the page, and uploads real JPEG bytes", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  // A marker any navigation or reload would wipe. Spec 37's first acceptance
  // criterion is that the page behind the dialog is not navigated away from or
  // reloaded — the whole reason the viewfinder exists — and nothing else here
  // would notice a regression that re-entered the page between shots.
  await page.evaluate(() => {
    window.__pageIdentity = "not-reloaded";
  });

  const dialog = await openViewfinder(page);

  // The mode's name in the heading, so docs/specs/09-consumption-logging.md's
  // "the current mode is always visible while the camera is open" holds even
  // though a modal dialog is covering the selector itself.
  await expect(dialog.locator("h2")).toContainText("Shelf scan");
  // getByLabel, per the accessibility criterion: the <video> has an accessible
  // name rather than being an unlabelled black rectangle to a screen reader.
  await expect(dialog.getByLabel("Camera preview")).toBeVisible();

  const rows = page.locator("#photo-list li");
  const shutter = dialog.locator('[data-role="shutter"]');

  await shutter.click();
  await expect(rows).toHaveCount(1);
  // The dialog stays open across shots — that is the feature.
  await expect(dialog).toBeVisible();
  await shutter.click();
  await expect(rows).toHaveCount(2);
  await expect(dialog.locator('[data-role="shot-count"]')).toHaveText("2 photos taken");

  // Distinct names, in sequence, each row with a thumbnail and a Remove
  // button.
  await expect(rows.nth(0)).toContainText("capture-1.jpg");
  await expect(rows.nth(1)).toContainText("capture-2.jpg");
  await expect(page.locator("#photo-list img[src^='blob:']")).toHaveCount(2);
  await expect(rows.nth(0).getByRole("button", { name: "Remove photo 1 of 2, capture-1.jpg", exact: true })).toBeVisible();

  // On the takePhoto() path the resolution hint is never shown: nothing was
  // downscaled to the preview size, so there is no trade-off to state.
  await expect(dialog.locator('[data-role="resolution-hint"]')).toBeHidden();

  await dialog.locator('[data-role="done"]').click();
  await expect(page.locator(VIEWFINDER)).toHaveCount(0);
  await expect(rows).toHaveCount(2);

  // The page itself survived: same document, same sticky mode, location field
  // still where it was.
  expect(await page.evaluate(() => window.__pageIdentity)).toBe("not-reloaded");
  await expect(page.locator('input[name="mode"][value="shelf_scan"]')).toBeChecked();
  await expect(page.locator("#location-field")).toBeVisible();

  // Upload: the same endpoint and the same one-request-per-photo shape as a
  // library pick, which is the criterion about server-side handling being
  // identical.
  const requests = [];
  await page.route("**/ingest/shelf-photos", async (route) => {
    requests.push(route.request());
    await route.fulfill({ json: { job_id: "00000000-0000-7000-8000-0000000000b1" } });
  });

  await page.getByRole("button", { name: "Upload" }).click();
  await expect(page.locator("#uploads .card")).toHaveCount(2);
  await expect(rows).toHaveCount(0);

  expect(requests).toHaveLength(2);
  expectJpegUpload(requests[0], "first capture");
  expectJpegUpload(requests[1], "second capture");

  await page.unroute("**/ingest/shelf-photos");
});

test("with ImageCapture absent the still comes from the canvas, and the resolution hint appears exactly once per session", async ({
  page,
}) => {
  await page.addInitScript(() => {
    delete window.ImageCapture;
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const dialog = await openViewfinder(page);
  const rows = page.locator("#photo-list li");
  const shutter = dialog.locator('[data-role="shutter"]');
  const hint = dialog.locator('[data-role="resolution-hint"]');

  // Not shown before there is anything to qualify.
  await expect(hint).toBeHidden();

  await shutter.click();
  await expect(rows).toHaveCount(1);
  await expect(hint).toBeVisible();

  await shutter.click();
  await expect(rows).toHaveCount(2);
  // "Once per viewfinder session" as a count, not just as a visible element: a
  // regression that appended a fresh hint per shot would leave this dialog
  // showing the same sentence twice, which `toBeVisible()` alone would not
  // notice.
  // Scoped to the whole page, not to the dialog: a regression that appended a
  // fresh hint per shot would most likely put it next to the first, but one
  // that appended it to the picker or the body instead would escape a
  // dialog-scoped count entirely.
  await expect(page.locator('[data-role="resolution-hint"]')).toHaveCount(1);
  // The assertion that does the real work, and the one #205's "appears exactly
  // once" is about: a duplicated hint node and a single node whose text was
  // concatenated twice both fail here, where a count of elements catches only
  // the first.
  const shown = (await dialog.textContent()).split("Preview resolution.").length - 1;
  expect(shown, "the hint's text appears exactly once after the second shot").toBe(1);

  // The canvas path normalises too — same JPEG guarantee, different source.
  let request = null;
  await page.route("**/ingest/shelf-photos", async (route) => {
    request = route.request();
    await route.fulfill({ json: { job_id: "00000000-0000-7000-8000-0000000000b2" } });
  });
  await dialog.locator('[data-role="done"]').click();
  await page.getByRole("button", { name: "Upload" }).click();
  await expect(page.locator("#uploads .card")).toHaveCount(2);
  expectJpegUpload(request, "canvas-path capture");

  await page.unroute("**/ingest/shelf-photos");
});

// Spec 37's "stream released" criterion, plus #205 item 5 (capture numbering
// continuing across viewfinder sessions), which the same three sessions prove
// without a fourth journey: the numbering claim is about what happens when the
// viewfinder is closed and reopened, which is exactly what this does.
test("Done, Esc and a backdrop click each release the stream and keep the shots, and numbering continues across sessions", async ({
  page,
}) => {
  await retainStreams(page);
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const rows = page.locator("#photo-list li");
  const exits = [
    ["Done", async (dialog) => dialog.locator('[data-role="done"]').click()],
    ["Esc", async () => page.keyboard.press("Escape")],
    // The viewport corner: a modal dialog's backdrop is the dialog element
    // itself outside its content box, so this is a genuine backdrop click and
    // not a click on a control.
    ["backdrop", async () => page.mouse.click(2, 2)],
  ];

  let taken = 0;
  for (const [exitName, exit] of exits) {
    const dialog = await openViewfinder(page);
    await dialog.locator('[data-role="shutter"]').click();
    taken += 1;
    await expect(rows).toHaveCount(taken);
    // Each session counts its own shots, unlike the file numbering below.
    await expect(dialog.locator('[data-role="shot-count"]')).toHaveText("1 photo taken");

    await exit(dialog);
    await expect(page.locator(VIEWFINDER)).toHaveCount(0);

    // Every track of every stream handed out so far — not just this session's
    // — because a leak in an earlier session would otherwise be invisible
    // once its dialog was gone.
    const states = await page.evaluate(() =>
      window.__streams.flatMap((stream) => stream.getTracks().map((track) => track.readyState)),
    );
    expect(states.length, `${exitName}: expected at least one retained track`).toBeGreaterThan(0);
    expect(
      states.every((state) => state === "ended"),
      `${exitName}: every retained track must be ended, got ${JSON.stringify(states)}`,
    ).toBe(true);

    // "Keeps every shot already taken in the list": exiting is not discarding.
    await expect(rows).toHaveCount(taken);
  }

  // #205 item 5: the numbering continues rather than restarting per session.
  // Three sessions, one shot each, so a per-session counter would produce
  // three rows all named capture-1.jpg — and docs/specs/36's list keys rows by
  // File identity, so it would render them without complaint.
  await expect(rows.nth(0)).toContainText("capture-1.jpg");
  await expect(rows.nth(1)).toContainText("capture-2.jpg");
  await expect(rows.nth(2)).toContainText("capture-3.jpg");
});

// #205 item 1: the takePhoto() rejection path had no journey at all, though
// spec 37 requires that press to fall back to the canvas grab rather than do
// nothing. Measured as cheap to drive, and it is.
test("a takePhoto() rejection falls back to the canvas grab for that press, with no resolution hint", async ({
  page,
}) => {
  await page.addInitScript(() => {
    // The rejection real Android devices produce (UnknownError,
    // InvalidStateError). The constructor still exists and still succeeds —
    // only the call fails, which is the case the canvas fallback is for.
    window.ImageCapture.prototype.takePhoto = () =>
      Promise.reject(new DOMException("takePhoto refused", "UnknownError"));
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const dialog = await openViewfinder(page);
  const rows = page.locator("#photo-list li");

  await dialog.locator('[data-role="shutter"]').click();
  // One row, not zero: the press produced a photo by another route.
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("capture-1.jpg");

  // #205 item 7's first nit. The hint describes the ImageCapture-absent
  // trade-off; this press's fallback is not that, and the next press will try
  // takePhoto() again — so showing it here would be telling the person
  // something untrue about their camera.
  await expect(dialog.locator('[data-role="resolution-hint"]')).toBeHidden();

  // And it is a real JPEG, not a relabelled anything.
  let request = null;
  await page.route("**/ingest/shelf-photos", async (route) => {
    request = route.request();
    await route.fulfill({ json: { job_id: "00000000-0000-7000-8000-0000000000b3" } });
  });
  await dialog.locator('[data-role="done"]').click();
  await page.getByRole("button", { name: "Upload" }).click();
  await expect(page.locator("#uploads .card")).toHaveCount(1);
  expectJpegUpload(request, "takePhoto-rejection fallback");

  await page.unroute("**/ingest/shelf-photos");
});

// #205 item 2: the real plain-HTTP home-network case, which the rejecting stub
// does not cover. `navigator.mediaDevices` is `undefined` there — not an
// object with a missing method — so a guard written without optional chaining
// throws a TypeError, the Camera button is dead on HTTP, and every other
// journey in this file stays green.
test("with navigator.mediaDevices absent, Camera opens the OS input on the same tap and says nothing", async ({
  page,
}) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "mediaDevices", { value: undefined, configurable: true });
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  // The chooser event, not a raw click: a hidden file input can receive a
  // click that opens nothing at all once the tap's activation has expired, so
  // only the event proves the chooser actually opened. Awaiting the event and
  // the click together is what makes this a same-tap assertion — a handler
  // that awaited anything before click() would not produce the event.
  const [chooser] = await Promise.all([
    page.waitForEvent("filechooser"),
    page.locator('[data-role="pick-camera"]').click(),
  ]);
  expect(await chooser.element().getAttribute("id")).toBe("photos-camera");

  await expect(page.locator(VIEWFINDER)).toHaveCount(0);
  // Silently. An insecure origin simply has no in-page camera, and a status
  // line here would be reporting the platform as a fault.
  await expect(page.locator(PICKER_STATUS)).toBeHidden();
  await expect(page.locator("#error")).toBeHidden();
});

// Spec 37's own fallback journey: getUserMedia present but rejecting, which is
// a denied permission or a camera in use. The rejection is immediate, so the
// tap's activation is still alive and the same tap reaches the OS input.
test("with getUserMedia rejecting, Camera reaches the OS input on the same tap and shows no error", async ({
  page,
}) => {
  await page.addInitScript(() => {
    navigator.mediaDevices.getUserMedia = () =>
      Promise.reject(new DOMException("Permission denied", "NotAllowedError"));
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const [chooser] = await Promise.all([
    page.waitForEvent("filechooser"),
    page.locator('[data-role="pick-camera"]').click(),
  ]);
  expect(await chooser.element().getAttribute("id")).toBe("photos-camera");

  await expect(page.locator(VIEWFINDER)).toHaveCount(0);
  // "No error is shown for a denied permission" — the OS camera is the
  // answer, and it needs no browser permission.
  await expect(page.locator("#error")).toBeHidden();
});

// #205 item 4: the journeys above all wait for the Shutter to *become*
// enabled, so a regression that enabled it the moment the dialog opened would
// pass every one of them. This pins videoWidth at 0 and asserts the gate
// holds, which is the only way to test a guard rather than its eventual
// release.
test("the Shutter stays disabled while videoWidth is 0, and enables only once a frame exists", async ({ page }) => {
  await page.addInitScript(() => {
    // The real getter is captured before the override, and releasing restores
    // it. Without that capture the pin could never be lifted, the Shutter
    // would never enable, and the second half of this test would be asserting
    // that a permanently broken video never works.
    const real = Object.getOwnPropertyDescriptor(HTMLVideoElement.prototype, "videoWidth");
    window.__releaseVideoWidth = () => {
      Object.defineProperty(HTMLVideoElement.prototype, "videoWidth", real);
    };
    Object.defineProperty(HTMLVideoElement.prototype, "videoWidth", {
      configurable: true,
      get() {
        return 0;
      },
    });
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  await page.locator('[data-role="pick-camera"]').click();
  const dialog = page.locator(VIEWFINDER);
  await expect(dialog).toBeVisible();
  const shutter = dialog.locator('[data-role="shutter"]');

  // The video is laid out and visible with videoWidth still 0 — which is
  // exactly why spec 37 says toBeVisible() on it proves nothing about a frame.
  await expect(dialog.getByLabel("Camera preview")).toBeVisible();
  await expect(shutter).toBeDisabled();
  // Held, not merely disabled at the instant the dialog opened. A fixed wait
  // is the point here rather than a smell: the claim is that a state persists
  // while a condition holds, and only elapsed time can demonstrate that. The
  // module's own first-frame poller runs every 30 ms, so this is ~16 chances
  // for a wrong implementation to enable the Shutter.
  await page.waitForTimeout(500);
  await expect(shutter).toBeDisabled();

  await page.evaluate(() => window.__releaseVideoWidth());
  await expect(shutter).toBeEnabled({ timeout: 10_000 });

  // And the frame it waited for is real: the press produces a photo rather
  // than the null blob a grab before the first frame would have yielded.
  await shutter.click();
  await expect(page.locator("#photo-list li")).toHaveCount(1);
});

// #205 item 3: the scan sheet journey above covers only its library input, so
// nothing caught a regression that wired the viewfinder into the sheet too.
// Spec 37 is explicit that the sheet keeps the OS input — it is already a
// modal dialog, has no list to append to, and a live camera for a barcode is
// docs/specs/20-barcode-recall.md's own scan path.
test("the barcode scan sheet's Camera control opens the OS input, never the viewfinder", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);
  // Shelf scan has no barcode affordance at all, so a mode that has one first.
  await page.locator('input[name="mode"][value="stocking_up"]').check();

  await page.getByRole("button", { name: "Scan a barcode" }).click();
  const sheet = page.locator("dialog[aria-labelledby='barcode-sheet-title']");
  await expect(sheet).toBeVisible();

  const [chooser] = await Promise.all([
    page.waitForEvent("filechooser"),
    sheet.locator('[data-role="pick-camera"]').click(),
  ]);
  // The sheet's picker is id-less on purpose — the multi-photo picker holds
  // #photos-camera while the sheet is open, and a document may not have two
  // elements sharing an id — so the proof it is the OS camera input is the
  // attribute that makes it one.
  expect(await chooser.element().getAttribute("capture")).toBe("environment");
  await expect(page.locator(VIEWFINDER)).toHaveCount(0);
});

// review-tests round 1, blocking: flip() had zero execution, and the "not
// verifiable in this stack" defense did not hold. #205 does record the flip
// button as device-only, but that is about which *physical camera* the second
// stream comes from — the fake device has one. The flip *logic* is a different
// thing and is perfectly drivable here, because the button's visibility and the
// whole flip path are gated on enumerateDevices(), which is as overridable as
// getUserMedia — which this file already overrides in four other journeys.
//
// What this pins is exactly what would otherwise have shipped untested: the old
// stream released before the new one is attached (or the camera indicator stays
// lit for a camera nobody is using), a new ImageCapture built for the new track
// (or takePhoto() keeps photographing a stopped one), and the Shutter re-gated
// until the new stream's first frame (or a canvas grab in that window yields a
// null blob).
//
// Still not verifiable here, and unchanged from #205: that the second stream is
// genuinely a different camera. Nothing in this stack can tell.
test("flipping the camera releases the old stream, re-creates ImageCapture and re-gates the Shutter", async ({
  page,
}) => {
  await page.addInitScript(() => {
    // Two videoinputs, so the flip button is offered at all. js/camera.js reads
    // nothing but `kind` off these, so plain objects are enough and are clearer
    // than doctoring the real list.
    navigator.mediaDevices.enumerateDevices = async () => [
      { kind: "videoinput", deviceId: "fake_device_0", label: "fake_device_0", groupId: "g0" },
      { kind: "videoinput", deviceId: "fake_device_1", label: "fake_device_1", groupId: "g1" },
      { kind: "audioinput", deviceId: "fake_audio_0", label: "fake_audio_0", groupId: "g2" },
    ];

    window.__streams = [];
    window.__constraints = [];
    const realGetUserMedia = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = async (constraints) => {
      window.__constraints.push(JSON.stringify(constraints?.video ?? null));
      // The second call is held open until the test releases it. That is what
      // makes "the Shutter is disabled again after a flip" a deterministic
      // assertion rather than a race against a ~60 ms stream handover.
      if (window.__streams.length === 1) {
        await new Promise((resolve) => {
          window.__releaseSecondStream = resolve;
        });
      }
      const stream = await realGetUserMedia(constraints);
      window.__streams.push(stream);
      return stream;
    };

    // Counting constructions is the only way to see the re-creation from
    // outside: a stale ImageCapture holds a stopped track and would photograph
    // nothing, which no visible state reveals until a press fails.
    window.__imageCaptures = 0;
    const RealImageCapture = window.ImageCapture;
    window.ImageCapture = class extends RealImageCapture {
      constructor(track) {
        super(track);
        window.__imageCaptures += 1;
      }
    };
  });

  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const dialog = await openViewfinder(page);
  const shutter = dialog.locator('[data-role="shutter"]');
  const flip = dialog.locator('[data-role="flip"]');
  const rows = page.locator("#photo-list li");

  // Offered only because two cameras were reported: the other journeys, which
  // report one, assert nothing about this button and it stays hidden there.
  await expect(flip).toBeVisible();
  expect(await page.evaluate(() => window.__imageCaptures), "one ImageCapture for the first stream").toBe(1);

  await shutter.click();
  await expect(rows).toHaveCount(1);

  await flip.click();

  // The second getUserMedia is still pending, so this is the re-gated state
  // itself and not a snapshot taken before the handover finished.
  await page.waitForFunction(() => typeof window.__releaseSecondStream === "function");
  await expect(shutter).toBeDisabled();
  // And the old stream is already released at this point — the spec stops it
  // before asking for the new one, because two live streams on one device is
  // how the second request comes back overconstrained on some phones.
  const oldStreamStates = await page.evaluate(() =>
    window.__streams[0].getTracks().map((track) => track.readyState),
  );
  expect(oldStreamStates.length, "expected the first stream to have tracks").toBeGreaterThan(0);
  expect(
    oldStreamStates.every((state) => state === "ended"),
    `the old stream must be stopped before the new one is requested, got ${JSON.stringify(oldStreamStates)}`,
  ).toBe(true);

  await page.evaluate(() => window.__releaseSecondStream());
  await expect(shutter).toBeEnabled({ timeout: 10_000 });

  // facingMode actually flipped, rather than the same camera being reopened.
  expect(await page.evaluate(() => window.__constraints)).toEqual([
    JSON.stringify({ facingMode: "environment" }),
    JSON.stringify({ facingMode: "user" }),
  ]);
  expect(await page.evaluate(() => window.__streams.length), "a second stream was opened").toBe(2);
  expect(
    await page.evaluate(() => window.__imageCaptures),
    "a new ImageCapture must be built for the new track",
  ).toBe(2);

  // The viewfinder still works after the flip, and the numbering carried across
  // it: a shot taken on the new stream is capture-2.jpg, and the shot taken
  // before the flip is still in the list.
  await shutter.click();
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toContainText("capture-1.jpg");
  await expect(rows.nth(1)).toContainText("capture-2.jpg");
  await expect(dialog.locator('[data-role="shot-count"]')).toHaveText("2 photos taken");

  // Exiting still releases everything, both streams included.
  await dialog.locator('[data-role="done"]').click();
  await expect(page.locator(VIEWFINDER)).toHaveCount(0);
  const allStates = await page.evaluate(() =>
    window.__streams.flatMap((stream) => stream.getTracks().map((track) => track.readyState)),
  );
  expect(
    allStates.every((state) => state === "ended"),
    `every track of both streams must be ended, got ${JSON.stringify(allStates)}`,
  ).toBe(true);
});

// #205 item 6, which the issue flags as unmeasured and asks the package to
// decide: what happens when the normalisation itself fails. The decision, and
// what this journey pins:
//
//   * `createImageBitmap` rejecting → fall back to the raw video frame. It is
//     the same fallback a takePhoto() rejection already takes, it needs no new
//     concept, and it keeps the press producing a photo.
//   * `toBlob` yielding null on both paths → show the picker's status line. A
//     press that silently does nothing is the failure mode spec 37's fallback
//     rules exist to prevent, and there is no third path left to try.
//   * No canvas size cap. A 50 MP re-encode at q0.92 approaching the 25 MB
//     MaxUploadBytes (internal/httpapi/upload.go) is refused by the server
//     with payload_too_large, which js/pages/ingest.js already renders as its
//     own message — a client-side cap would silently downscale the photo the
//     person chose the full sensor for, and would need a second number kept in
//     step with the server's.
test("a createImageBitmap rejection falls back to the raw frame; a toBlob that yields nothing says so instead of doing nothing", async ({
  page,
}) => {
  await page.addInitScript(() => {
    // Both stubs in one journey, because the interesting part is the order:
    // the first press must still produce a photo, the second must not produce
    // one silently, and the third proves the numbering did not advance on the
    // press that produced nothing.
    window.createImageBitmap = () => Promise.reject(new Error("no decoder for this blob"));
    window.__breakToBlob = false;
    const realToBlob = HTMLCanvasElement.prototype.toBlob;
    HTMLCanvasElement.prototype.toBlob = function (callback, ...rest) {
      if (window.__breakToBlob) {
        // What a canvas too large for the platform's limits does: null, not a
        // throw.
        callback(null);
        return undefined;
      }
      return realToBlob.call(this, callback, ...rest);
    };
  });
  await logInAsBob(page);
  await page.goto(`/ingest.html?storage=${HOUSEHOLD}`);

  const dialog = await openViewfinder(page);
  const rows = page.locator("#photo-list li");
  const shutter = dialog.locator('[data-role="shutter"]');
  const status = page.locator(PICKER_STATUS);

  // createImageBitmap is broken, so the takePhoto() Blob cannot be normalised
  // — and the press still produces a photo, from the raw frame.
  await shutter.click();
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("capture-1.jpg");
  await expect(status).toBeHidden();

  // Now no encode can succeed at all. The press must say so.
  await page.evaluate(() => {
    window.__breakToBlob = true;
  });
  await shutter.click();
  await expect(status).toBeVisible();
  await expect(status).toContainText("could not be saved");
  await expect(rows).toHaveCount(1);

  // And the numbering did not advance on a press that produced no file: the
  // next successful shot is capture-2.jpg, not capture-3.jpg. A counter
  // incremented before the encode instead of after it would leave a gap here.
  await page.evaluate(() => {
    window.__breakToBlob = false;
  });
  await shutter.click();
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(1)).toContainText("capture-2.jpg");
});

test("the inbox lists waiting proposals with their size, and the badge counts them", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/storages.html?storage=${HOUSEHOLD}`);

  const badge = page.locator("#inbox-link .badge");
  await expect(badge).toBeVisible();
  await expect(badge).not.toHaveText("0");

  await page.locator("#inbox-link a").click();
  await expect(page).toHaveURL(/\/inbox\.html/);
  const shelf = page.locator(`[data-job-id="${LOOK_ONLY_JOB}"]`);
  await expect(shelf).toContainText("Shelf photo");
  await expect(shelf).toContainText("2 items");
  await expect(shelf.getByRole("link", { name: "Review" })).toBeVisible();
});

test("a proposal is reviewed and confirmed into inventory", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${SHELF_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(3);

  // Row 0: the matched product on the matched shelf — accepted as proposed.
  const tomatoes = rows.nth(0);
  await expect(tomatoes).toContainText("Canned Tomatoes 400g");
  await expect(tomatoes.locator('[data-role="product"]')).toHaveValue(`product:${TOMATOES}`);

  // Row 1: a new product on a shelf the photo proposed; the reviewer fixes the
  // quantity and sets the date.
  const oatMilk = rows.nth(1);
  await expect(oatMilk.locator('[data-role="location"]')).toHaveValue("new");
  await expect(oatMilk.locator('[data-role="new-product-name"]')).toHaveValue("Oat Milk 1L");
  // This job has no photo, so there is nothing to take a picture from and the
  // choice is not offered at all.
  await expect(oatMilk.locator('[data-role="new-product-image-field"]')).toBeHidden();
  await oatMilk.locator('[data-role="quantity"]').fill("4");
  await oatMilk.locator('[data-role="expiry"]').fill("2027-02-01");

  // Row 2: a false positive. Rejected rows stay visible and struck through.
  const mystery = rows.nth(2);
  await mystery.getByRole("button", { name: "Reject" }).click();
  await expect(mystery).toHaveClass(/review-row--rejected/);

  // The confirm is passed through a route handler so its body can be read:
  // the page navigates to the inbox the moment it succeeds, and a response
  // observed from outside loses its body with the page that made it.
  let sent = null;
  let written = null;
  let status = 0;
  await page.route(`**/ingest/${SHELF_JOB}/confirm`, async (route) => {
    sent = route.request().postDataJSON();
    const response = await route.fetch();
    status = response.status();
    written = await response.json();
    await route.fulfill({ response });
  });
  await page.getByRole("button", { name: "Confirm" }).click();

  // Row 1 creates a product ("Oat Milk 1L") with no barcode, so the
  // capture-time offer of docs/specs/20-barcode-recall.md appears between the
  // confirm and the navigation. Declining it is one tap and writes nothing —
  // e2e/specs/barcode-recall.spec.js asserts that offer's own contract; here
  // it is dismissed so this journey goes on testing the confirm.
  const barcodeOffer = page.locator("dialog[aria-labelledby='barcode-offer-title']");
  await expect(barcodeOffer).toBeVisible();
  await barcodeOffer.getByRole("button", { name: "Not this time" }).click();

  await expect(page).toHaveURL(/\/inbox\.html/);

  // What the screen sent: every row decided, and the reviewer's edits intact —
  // the edited quantity and date, the proposed shelf as new_location below
  // Pantry, the existing product by id, and the rejection with nothing else.
  expect(sent.items).toEqual([
    { row_id: "0", decision: "accept", product_id: TOMATOES, quantity: 2, location_id: PANTRY },
    {
      row_id: "1",
      decision: "accept",
      new_product: { name: "Oat Milk 1L", item_type: "long_shelf_life" },
      quantity: 4,
      new_location: { parent_id: PANTRY, names: ["Top Shelf"] },
      expiration_date: "2027-02-01",
    },
    { row_id: "2", decision: "reject" },
  ]);

  // What the server wrote: two batches (the rejection wrote none), one new
  // product, one new location. How those rows persist quantity, date and
  // expiration_source is pinned against PostgreSQL in internal/store's
  // ingestion tests.
  expect(status).toBe(200);
  expect(written.batch_ids).toHaveLength(2);
  expect(written.products_created).toBe(1);
  expect(written.locations_created).toBe(1);

  await expect(page).toHaveURL(/\/inbox\.html\?.*confirmed=2/);
  await expect(page.locator("#notice")).toHaveText(
    "Proposal applied: 2 items added to your inventory, 1 new product, 1 new location.",
  );
  await expect(page.locator("#notice")).toHaveClass(/\balert--success\b/);
  // Resolved --color-success (#15803d), not the --color-danger red every
  // #error box renders in: a broken token reference inside .alert--success
  // would leave this box either red or uncoloured, and only a computed-style
  // check — not the static class assertion above — would catch that.
  await expect(page.locator("#notice")).toHaveCSS("color", "rgb(21, 128, 61)");
  await expect(page.locator(`[data-job-id="${SHELF_JOB}"]`)).toHaveCount(0);

  // The proposed shelf now exists under Pantry.
  const tree = await (await page.request.get(`/api/storages/${HOUSEHOLD}/locations`)).json();
  const pantry = tree.items.find((n) => n.name === "Pantry");
  expect(pantry.children.map((c) => c.name)).toContain("Top Shelf");

  // And the job cannot be applied a second time.
  const again = await page.request.post(`/api/storages/${HOUSEHOLD}/ingest/${SHELF_JOB}/confirm`, {
    data: { items: [{ row_id: "0", decision: "reject" }, { row_id: "1", decision: "reject" }, { row_id: "2", decision: "reject" }] },
  });
  expect(again.status()).toBe(409);
});

test("a proposal can be discarded from the inbox", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/inbox.html?storage=${HOUSEHOLD}`);

  const card = page.locator(`[data-job-id="${PRODUCT_JOB}"]`);
  await expect(card).toBeVisible();

  // A failed discard must still surface #error in its ordinary, unchanged
  // danger styling — #68 gave #notice its own success variant but must not
  // have touched what an actual error box looks like.
  const errorBox = page.locator("#error");
  await expect(errorBox).toBeHidden();
  await page.route(`**/jobs/${PRODUCT_JOB}`, async (route) => {
    if (route.request().method() === "DELETE") {
      await route.fulfill({ status: 500, contentType: "application/json", body: "{}" });
    } else {
      await route.continue();
    }
  });
  page.once("dialog", (dialog) => dialog.accept());
  await card.getByRole("button", { name: "Discard" }).click();
  await expect(errorBox).toBeVisible();
  await expect(errorBox).not.toHaveClass(/alert--success/);
  // Resolved --color-danger (#b91c1c), the same red it always was.
  await expect(errorBox).toHaveCSS("color", "rgb(185, 28, 28)");
  await expect(card).toBeVisible();
  await page.unroute(`**/jobs/${PRODUCT_JOB}`);

  page.once("dialog", (dialog) => dialog.accept());
  await card.getByRole("button", { name: "Discard" }).click();
  await expect(card).toHaveCount(0);

  const gone = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${PRODUCT_JOB}`);
  expect(gone.status()).toBe(404);
});

test("a new product's picture can be taken from the reviewed photo", async ({ page }) => {
  // No seeded job has a photo on disk, and the E2E stack has no step to put
  // one there. So the job is read from the server as it really is and then
  // shown to the page as a photo job, with a box on its first row only — the
  // two cases the picture choice must tell apart. What is under test is the
  // review screen: that it offers the right choices and sends the right field.
  // Cutting and storing the picture is covered by the Go tests
  // (internal/httpapi/productimages_test.go).
  //
  // LOOK_ONLY_JOB, because no other test consumes it and this one must not
  // either: the confirm below is answered here, never sent to the server.
  await logInAsBob(page);

  await page.route(`**/jobs/${LOOK_ONLY_JOB}`, async (route) => {
    const response = await route.fetch();
    const job = await response.json();
    job.has_image = true;
    job.payload.rows[0].bounding_box = { x: 0.1, y: 0.2, width: 0.4, height: 0.5 };
    job.payload.rows[1].bounding_box = null;
    await route.fulfill({ response, json: job });
  });
  await page.route(`**/jobs/${LOOK_ONLY_JOB}/image`, (route) =>
    route.fulfill({ contentType: "image/png", body: TINY_PNG }),
  );

  let sent = null;
  await page.route(`**/ingest/${LOOK_ONLY_JOB}/confirm`, async (route) => {
    sent = route.request().postDataJSON();
    await route.fulfill({ json: { batch_ids: [], products_created: 2, locations_created: 0 } });
  });

  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOOK_ONLY_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const rice = rows.nth(0);
  const lentils = rows.nth(1);
  await expect(rice).toContainText("Rice 1kg");
  await expect(lentils).toContainText("Lentils 500g");

  // Both are new products on a photo job, so both are offered a picture —
  // and neither has one until the reviewer picks it.
  const riceChoice = rice.locator('[data-role="new-product-image"]');
  const lentilsChoice = lentils.locator('[data-role="new-product-image"]');
  await expect(rice.locator('[data-role="new-product-image-field"]')).toBeVisible();
  await expect(lentils.locator('[data-role="new-product-image-field"]')).toBeVisible();
  await expect(riceChoice).toHaveValue("");
  await expect(lentilsChoice).toHaveValue("");

  // Only a row with a box can offer its own crop.
  await expect(riceChoice.locator("option")).toHaveText(["No picture", "This item, cut from the photo", "The whole photo"]);
  await expect(lentilsChoice.locator("option")).toHaveText(["No picture", "The whole photo"]);

  await riceChoice.selectOption("crop");
  await rice.locator('[data-role="location"]').selectOption(PANTRY);
  await lentils.locator('[data-role="location"]').selectOption(PANTRY);

  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/inbox\.html/);

  // The chosen picture travels as new_product.image; the row left on "No
  // picture" sends no image key at all.
  expect(sent.items).toEqual([
    {
      row_id: "0",
      decision: "accept",
      new_product: { name: "Rice 1kg", item_type: "long_shelf_life", image: "crop" },
      quantity: 1,
      location_id: PANTRY,
    },
    {
      row_id: "1",
      decision: "accept",
      new_product: { name: "Lentils 500g", item_type: "long_shelf_life" },
      quantity: 2,
      location_id: PANTRY,
    },
  ]);

  // And the job is still waiting on the server, untouched.
  const job = await page.request.get(`/api/storages/${HOUSEHOLD}/jobs/${LOOK_ONLY_JOB}`);
  expect((await job.json()).status).toBe("done");
});

// docs/specs/26-location-quick-create.md: the "+ New location" escape hatch
// on every location field, in a storage that already has locations — Pantry
// and Fridge here — so this also proves the trigger is not conditional on an
// empty tree.
test("a location can be created from the review screen without leaving it, refreshing every row from one GET", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const first = rows.nth(0);
  const second = rows.nth(1);

  // Edits on the row that never opens the modal — proof they survive the
  // other row's use of it. The location is a real, non-default choice: a
  // refresh that rebuilt the selects from scratch would snap it back to the
  // placeholder, which a placeholder-vs-placeholder comparison cannot see.
  await second.locator('[data-role="quantity"]').fill("9");
  await second.locator('[data-role="location"]').selectOption(PANTRY);

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Pantry");

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Loft Shelf");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Loft Shelf");

  // Closing the modal once must refresh both rows' option lists from exactly
  // one GET — never one request per open field.
  let getCalls = 0;
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "GET") getCalls++;
    await route.continue();
  });
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Preselected in the field whose trigger opened the modal — the new
  // location itself, not merely "something other than the placeholder"…
  await expect(first.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Loft Shelf");
  // …offered, but not forced, on the row that never opened it, whose own
  // choices are exactly as they were left.
  await expect(second.locator('[data-role="location"] option', { hasText: "Loft Shelf" })).toHaveCount(1);
  await expect(second.locator('[data-role="location"]')).toHaveValue(PANTRY);
  await expect(second.locator('[data-role="quantity"]')).toHaveValue("9");

  // Only now that both rows are confirmed refreshed: exactly one GET did it,
  // never one request per open field.
  expect(getCalls).toBe(1);
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  // Cancelling with nothing created leaves every selection exactly as it was:
  // the opened field's own (Pantry, a non-default choice) and the other row's
  // (Loft Shelf), by both dismissals the spec names. Esc is the browser's own
  // cancel; the backdrop is a click on the ::backdrop, which lands well
  // outside the centred dialog's box, so it exercises tree-modal.js's
  // backdrop handler and not a click on the dialog's own padding.
  for (const dismiss of [() => page.keyboard.press("Escape"), () => page.mouse.click(5, 5)]) {
    await second.locator('[data-role="location-add"]').click();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeVisible();
    await dismiss();
    await expect(page.getByRole("dialog", { name: "Locations" })).toBeHidden();
    await expect(second.locator('[data-role="location"]')).toHaveValue(PANTRY);
    await expect(first.locator('[data-role="location"] option:checked')).toContainText("Loft Shelf");
  }
});

// #154 item 1: .card puts its padding on the <dialog> element itself
// (css/components.css), so event.target === dialog is also true for a click
// that never left the dialog's own box. tree-modal.js must tell that apart
// from an actual backdrop click, or a misclick this close to the edge closes
// the modal and loses whatever the user was mid-typing in the add-root form.
test("a click inside the location modal's own padding does not close it or lose an unsubmitted root name", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  const input = dialog.locator('input[aria-label="Name of the new top-level location"]');
  await input.fill("Garage Shelf");

  // Two pixels in from the dialog's own top-left corner: still inside its
  // rendered box (--space-5 padding is 24px, css/tokens.css), on no child
  // element, so event.target is the <dialog> itself — the exact shape of a
  // click on the padding rather than the backdrop.
  const box = await dialog.boundingBox();
  await page.mouse.click(box.x + 2, box.y + 2);

  await expect(dialog).toBeVisible();
  await expect(input).toHaveValue("Garage Shelf");
});

// #154 item 2: close() must wait for a create POST that's still in flight
// before resolving createdIds, or Done/Esc pressed early loses the new
// location from that resolve and the caller's own refresh races the commit.
test("closing the location modal while its create POST is still in flight still preselects the new location", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  // Held open until released below, so Done can be clicked while the create
  // POST this test cares about is still unresolved.
  let releasePost;
  const postHeld = new Promise((resolve) => {
    releasePost = resolve;
  });
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "POST") await postHeld;
    await route.continue();
  });

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Cellar Rack");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();

  // Done, clicked before the POST above returns: the dialog must stay open
  // rather than resolving createdIds without the id that POST will carry.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  releasePost();
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  await expect(dialog).toBeHidden();
  await expect(first.locator('[data-role="location"] option', { hasText: "Cellar Rack" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Cellar Rack");
});

// #154 item 2, round 2: pendingMutation was a single slot when this test was
// written, so a *second* create POST started while Done is already waiting on
// the first one must not be dropped when the first settles — tree-modal.js has
// to keep waiting for whichever one is still outstanding, not just the one
// requestClose originally latched onto. #221 has since replaced that slot with
// a set of every outstanding attempt; the promise this test guards is the same
// one either way.
test("closing the location modal while two creates overlap waits for both before resolving", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  // Each POST this test creates is held open independently, released in an
  // order the test controls below.
  const releases = [];
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    await new Promise((resolve) => releases.push(resolve));
    await route.continue();
  });

  const addRoot = async (name) => {
    await dialog.getByRole("button", { name: "Add top-level location" }).click();
    await dialog.locator('input[aria-label="Name of the new top-level location"]').fill(name);
    await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  };

  // The first create starts, then Done is clicked while only it is pending —
  // requestClose captures *that* attempt. Only then does the second create
  // start (the add-root form removes itself the instant it's submitted,
  // before its own POST returns, so it's free to reopen while the first is
  // still in flight and while Done is already waiting on it). This is the
  // interleaving that actually distinguishes the fix: latching onto "the
  // pending attempt at the moment Done was clicked" and waiting on it
  // unconditionally (submitting both creates before Done, then waiting on
  // whichever is current) exercises the same code path old and new code
  // already agreed on, and would pass against either.
  await addRoot("Attic Bin");

  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  await addRoot("Basement Nook");
  await expect.poll(() => releases.length).toBe(2);

  // The first create — the one requestClose actually latched onto — settles;
  // the second, started after Done was already clicked, is still
  // outstanding. The dialog must still be waiting on it rather than treating
  // the first settling as license to finalize. Waiting for the first
  // create's own effect to actually land (its reload redrawing the dialog's
  // tree) — rather than just checking visibility the instant releases[0] is
  // called — is what actually gives a premature close time to happen if the
  // fix is missing: an immediate visibility check would still read "visible"
  // even under the bug, since the real close only follows a moment later,
  // after the network round trip.
  releases[0]();
  await expect(dialog).toContainText("Attic Bin");
  await expect(dialog).toBeVisible();

  releases[1]();
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  await expect(dialog).toBeHidden();
  await expect(first.locator('[data-role="location"] option', { hasText: "Attic Bin" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option', { hasText: "Basement Nook" })).toHaveCount(1);
});

// #221: the test above releases its two creates in *start* order, so the
// single-slot pendingMutation was always cleared by whichever attempt
// requestClose was currently latched onto. Nothing orders POST responses by
// request order, though, and a single slot only ever named "the most recently
// started mutation" — not the set of ones still outstanding. This test is that
// test with the release order reversed, which is the case a single slot could
// not survive:
//
//   A starts, B starts (so the slot then named B), Done is clicked (so
//   requestClose latched onto B), then B settles *before* A. runMutation(B)
//   found the slot still naming B and nulled it, requestClose woke to an empty
//   slot and finalized — while A's POST was still in flight. A's id never
//   reached createdIds and its late answer landed in a dialog already removed,
//   which is exactly what #154 item 2 was filed to close.
//
// tree-modal.js tracks the set of outstanding attempts now, so the mechanism
// described above no longer exists; this stays as its regression guard, in the
// past tense that says so (#282).
//
// Note the deliberate difference from the test above, whose comment warns
// that submitting both creates before Done "would pass against either"
// implementation: that warning is about the *round-2* fix (latching onto a
// stale attempt), which both-before-Done genuinely cannot distinguish. It is
// not an objection to this interleaving, which needs both creates in flight
// before Done precisely so the slot could be overwritten and then cleared by
// the wrong one.
test("closing the location modal resolves both creates even when they settle out of start order", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();

  const releases = [];
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    await new Promise((resolve) => releases.push(resolve));
    await route.continue();
  });

  const addRoot = async (name) => {
    await dialog.getByRole("button", { name: "Add top-level location" }).click();
    await dialog.locator('input[aria-label="Name of the new top-level location"]').fill(name);
    await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  };

  // Both creates start before Done, so a single slot would have ended up
  // naming the *second* one — and releases[0] is the first-started POST,
  // releases[1] the second.
  //
  // These two names are new roots in the *shared* "E2E Household", so they
  // have to be unique to this test and must not collide with any other
  // spec's assertions on that tree — including the negative ones:
  // e2e/specs/storage-switching.spec.js proves storage isolation by asserting
  // this tree never contains "Garage", so a name like "Garage Crate" would
  // fail that file rather than this one. Creating new rows here is the right
  // pattern (nothing else reads these two), but pick names no other spec
  // mentions in either direction.
  await addRoot("Boiler Nook");
  await addRoot("Utility Closet");
  await expect.poll(() => releases.length).toBe(2);

  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  // The second-started create settles first. Its reload redrawing the tree is
  // what gives a premature close time to actually happen: checking visibility
  // the instant releases[1] is called would read "visible" even under the bug,
  // since the real close only follows a network round trip later.
  releases[1]();
  await expect(dialog).toContainText("Utility Closet");
  await expect(dialog).toBeVisible();

  releases[0]();
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  await expect(dialog).toBeHidden();
  // The first-started create is the one a single slot dropped. Both ids have to
  // reach createdIds for both options to be here.
  await expect(first.locator('[data-role="location"] option', { hasText: "Boiler Nook" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option', { hasText: "Utility Closet" })).toHaveCount(1);
});

// #280: #221 made requestClose wait on a *set* of outstanding mutations, but
// runMutation deleted its attempt from that set in a `finally` that fired
// before `await reload()` — so the set could be empty while the post-mutation
// reload GET was still in flight, and a Done or Esc inside that window
// finalized. The dialog was then removed from the DOM while its own reload was
// still coming, and that reload's view.render — or its showError — landed on a
// dialog already gone: the identical failure shape #221 fixed for the create
// itself, one step further along.
//
// Holding the *reload* rather than the create is what makes this test
// discriminating. The create POST is allowed through and settles normally, so
// under the old ordering the attempt had already left pendingMutations by the
// time Done is clicked, and finalize then ran synchronously inside that click —
// the dialog was gone before the click returned. That is why an immediate
// visibility check is enough here and no settle barrier is needed, unlike the
// two-dismissal test below.
test("closing the location modal while its post-create reload GET is still in flight waits for that reload", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  // The dialog's own opening reload has rendered, so the next GET on this
  // endpoint is unambiguously the one the create below triggers.
  await expect(dialog).toContainText("Pantry");

  let releaseReload;
  const reloadHeld = new Promise((resolve) => {
    releaseReload = resolve;
  });
  let heldReloads = 0;
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "GET" && heldReloads === 0) {
      heldReloads = 1;
      await reloadHeld;
    }
    await route.continue();
  });

  // A new root in the *shared* "E2E Household", so its name has to be unique to
  // this test and must not be a substring or a superstring of a name any spec
  // asserts on, in either direction: a `hasText` filter matches substrings, so
  // "Boiler Nook Two" would break the reversed-settle test above on its own
  // toHaveCount(1), and anything containing "Garage" is asserted *absent* from
  // this tree by e2e/specs/storage-switching.spec.js.
  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Broom Cupboard");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();

  // The create POST has answered — the request now held is its reload, which is
  // exactly the window this issue is about.
  await expect.poll(() => heldReloads).toBe(1);

  // Done, clicked with nothing but that reload outstanding: the dialog has to
  // keep waiting rather than finalizing and letting the reload land on a
  // dialog already removed.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  releaseReload();
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);

  await expect(dialog).toBeHidden();
  await expect(first.locator('[data-role="location"] option', { hasText: "Broom Cupboard" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Broom Cupboard");
});

// #275: finalize's failure latch was one consume-once boolean, so of two
// dismissals asked for before the same mutation failed, the first correctly
// kept the dialog open and the second found the flag already cleared and closed
// it — both inside the settle's own microtask batch, with no repaint in
// between, so the dialog was removed without showError's message ever having
// been painted.
//
// Done then Esc, both before the create answers, is that interleaving: each
// registers its own independent waiter on the same in-flight attempt, and both
// reach finalize when it settles. Nothing here holds the *reload* open, on
// purpose — #280's fix alone (keeping an attempt tracked across its reload)
// would make a held reload keep the dialog open all by itself, and this test
// would then pass without the per-attempt failure tracking it is actually
// about.
test("two dismissals racing one failing create leave the location modal open with its error shown", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Pantry");

  // Held, then failed: the create has to still be in flight while both
  // dismissals are asked for, and then answer with an error. An aborted
  // request never reaches the server, so this test writes nothing at all to the
  // shared "E2E Household" — the name below exists only in the browser.
  let releasePost;
  const postHeld = new Promise((resolve) => {
    releasePost = resolve;
  });
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    await postHeld;
    await route.abort("failed");
  });

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Sunken Crate");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();

  // Two independent waiters on the one in-flight attempt, neither of which can
  // have seen an error — there is none yet to see.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toBeVisible();

  // The create now fails and its own reload follows. Waiting for that reload's
  // response and then for two animation frames puts the assertions below
  // strictly after the point at which the old latch closed the dialog: that
  // close happened in the microtask batch right after this GET was *issued*,
  // before its response came back and certainly before the next paint — which
  // is the whole shape of the finding, a dialog removed before the error was
  // ever painted.
  const reloadDone = page.waitForResponse(
    (response) =>
      response.request().method() === "GET" && response.url().includes(`/api/storages/${HOUSEHOLD}/locations`),
  );
  releasePost();
  await reloadDone;
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));

  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("alert")).toContainText("Could not reach the server");

  // The failure must not trap the dialog either: a dismissal made with the
  // banner in front of the user closes it, exactly as the second Done did
  // under the consume-once flag.
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

// #281: pendingMutationFailed was one shared boolean, and runMutation reset it
// on entry together with a clearMessages() that wiped the banner with it. So a
// first mutation's failure could be erased by a second mutation merely starting
// — easy, because the add-root form reopens the instant it is submitted — and
// if that second mutation succeeded, Done then closed the dialog with the user
// never told that the first one silently did not happen.
//
// What distinguishes fixed from broken here is NOT whether the dialog closes.
// It closes on the first Done either way: with the fix, that Done is made with
// the banner already in front of the user, so it carries that failure in its own
// snapshot and is entitled to close. The observable is the banner itself —
// that it survives the second mutation's entry and its success. Reverting
// runMutation's `if (unacknowledgedFailures.size === 0) clearError()` to an
// unconditional clear reinstates #281's bug, and this test is the one that
// fails when it does.
test("a failed create's error survives a second create starting and succeeding", async ({ page }) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Pantry");

  // The first create fails, every later one succeeds. An aborted request never
  // reaches the server, so the failing name below is written nowhere.
  let posts = 0;
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    posts += 1;
    if (posts === 1) {
      await route.abort("failed");
      return;
    }
    await route.continue();
  });

  const addRoot = async (name) => {
    await dialog.getByRole("button", { name: "Add top-level location" }).click();
    await dialog.locator('input[aria-label="Name of the new top-level location"]').fill(name);
    await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  };

  // A fails, and says so.
  await addRoot("Warped Trunk");
  await expect(dialog.getByRole("alert")).toContainText("Could not reach the server");

  // B starts — the point of the issue is that this alone used to wipe A's
  // banner — and succeeds, which is what used to leave Done free to close over
  // a failure nobody had read.
  await addRoot("Tiled Alcove");
  await expect(dialog).toContainText("Tiled Alcove");

  // A's error is still on screen. This is the assertion #281 is about; at rest,
  // not in a transient window — B's reload has already redrawn the tree above,
  // and B cleared the banner (if it was going to) synchronously on submit, long
  // before that.
  await expect(dialog.getByRole("alert")).toContainText("Could not reach the server");
  await expect(dialog).not.toContainText("Warped Trunk");

  // Having now been shown it, the user can close: this Done carries A's failure
  // in its own snapshot.
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  await expect(first.locator('[data-role="location"] option', { hasText: "Tiled Alcove" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option', { hasText: "Warped Trunk" })).toHaveCount(0);
});

// The other half of the failure ledger: a mutation whose *reload* fails latches
// that failure too, exactly as a failed mutation does. reload() reports instead
// of throwing (it is also the initial render's path), so runMutation latches a
// false return — without which a dismissal that was already waiting when the
// reload failed would close over a banner nobody had seen, which is #275's
// finding one function along. Dropping runMutation's
// `if (!(await reload())) unacknowledgedFailures.add(token)` is what this test
// fails against.
//
// The create itself succeeds here, so its id still reaches createdIds and is
// still preselected after the close — the failed redraw costs the user the
// redraw and nothing else.
test("a create whose own reload fails keeps the dialog open on a dismissal that was already waiting", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const first = page.locator("#rows .review-row").first();

  await first.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toBeVisible();
  // The opening reload has rendered, so the next GET is the create's own.
  await expect(dialog).toContainText("Pantry");

  // The create is held so Done can be asked for while it is still in flight —
  // the dismissal has to be *waiting* when the failure appears, which is the
  // whole point — and then allowed through. Its reload is the request that
  // fails.
  let releasePost;
  const postHeld = new Promise((resolve) => {
    releasePost = resolve;
  });
  let failedReloads = 0;
  await page.route(`**/api/storages/${HOUSEHOLD}/locations`, async (route) => {
    if (route.request().method() === "POST") {
      await postHeld;
      await route.continue();
      return;
    }
    if (failedReloads === 0) {
      failedReloads = 1;
      await route.abort("failed");
      return;
    }
    await route.continue();
  });

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Chalk Niche");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();

  // Asked for before anything has failed, so its snapshot of seen failures is
  // empty.
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeVisible();

  // The create succeeds, its reload fails, and the waiting Done must not close
  // over that. Waiting for the page to see the failed request and then for two
  // animation frames puts the assertions strictly after the point at which an
  // unlatched reload failure closed the dialog.
  const reloadFailed = page.waitForEvent(
    "requestfailed",
    (request) => request.method() === "GET" && request.url().includes(`/api/storages/${HOUSEHOLD}/locations`),
  );
  releasePost();
  await reloadFailed;
  await page.unroute(`**/api/storages/${HOUSEHOLD}/locations`);
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));

  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("alert")).toContainText("Could not reach the server");

  // And a dismissal made with the banner in front of the user closes, with the
  // location that really was created still preselected.
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(first.locator('[data-role="location"] option', { hasText: "Chalk Niche" })).toHaveCount(1);
  await expect(first.locator('[data-role="location"] option:checked')).toContainText("Chalk Niche");
});

// docs/specs/26-location-quick-create.md's first acceptance criterion, for
// `06` review specifically: "in a storage with zero locations ... the user
// opens the modal, creates a root location, closes the modal, and that
// location is immediately selectable — no page reload." The test above
// covers the *second* bullet (an already-populated tree); this one starts
// from "E2E Admin Household", which — despite the name — also has no
// locations at all (the same as "E2E Zero-Locations Household" does), so
// the placeholder-only select, the empty-tree message inside the modal, and
// setupLocation's option handling with nothing already appended are all
// exercised from a genuine cold start.
test("a location can be created from the review screen in a storage with zero locations", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin-2", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/review.html?storage=${ZERO_LOCATIONS_HOUSEHOLD}&job=${ZERO_LOCATIONS_JOB}`);
  const row = page.locator("#rows .review-row").first();
  await expect(row).toBeVisible();
  await expect(row.locator('[data-role="location"] option')).toHaveCount(1); // the placeholder alone

  await row.locator('[data-role="location-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Locations" });
  await expect(dialog).toContainText("No locations yet");

  await dialog.getByRole("button", { name: "Add top-level location" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level location"]').fill("Hall Closet");
  await dialog.locator("#location-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Hall Closet");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Immediately selectable — no reload.
  await expect(row.locator('[data-role="location"] option', { hasText: "Hall Closet" })).toHaveCount(1);
  await expect(row.locator('[data-role="location"]')).not.toHaveValue("");
});

// docs/specs/27-category-quick-create.md: js/tree-modal.js generalized to a
// "categories" kind, with the same "+ New category" escape hatch beside a
// new product's category field. Reuses LOCATION_MODAL_JOB — never
// confirmed, so it can be revisited by this test however many times it is
// run, same as the location test above — and adds the new category as a
// child of the seeded "Canned Goods" node, matching the acceptance
// criterion's own example ("adds it as a child of an existing node").
test("a category can be created from the review screen's new-product form, as a child of an existing node", async ({
  page,
}) => {
  await logInAsBob(page);
  await page.goto(`/review.html?storage=${HOUSEHOLD}&job=${LOCATION_MODAL_JOB}`);
  const rows = page.locator("#rows .review-row");
  await expect(rows).toHaveCount(2);

  const first = rows.nth(0);
  const second = rows.nth(1);

  // Neither row matched an existing product, so the new-product fields —
  // category included — are already visible.
  await expect(first.locator('[data-role="new-product"]')).toBeVisible();

  // Edits on the row that never opens the modal — proof they survive the
  // other row's use of it. The category is a real, non-default choice, for the
  // same reason as the location in the test above.
  await second.locator('[data-role="new-product-name"]').fill("Ground Cumin");
  await second.locator('[data-role="new-product-category"]').selectOption(CANNED_GOODS);

  await first.locator('[data-role="new-product-category-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Categories" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("Canned Goods");

  const cannedGoods = dialog.locator(`.tree-node[data-id="${CANNED_GOODS}"]`);

  // The shelf-life-rule detail beside "Canned Goods" is categories.html's
  // own renderer (js/category-shelf-life.js), not a stripped-down copy: its
  // label and editor behave here exactly as there. No save — mutating this
  // shared household's shelf life would ripple into every other test that
  // reads Canned Tomatoes' batches.
  await expect(cannedGoods).toContainText("730 days");
  await cannedGoods.getByRole("button", { name: "730 days" }).click();
  await expect(cannedGoods.locator('input[type="number"]')).toHaveValue("730");
  await cannedGoods.getByRole("button", { name: "Cancel" }).click();
  await expect(cannedGoods).toContainText("730 days");

  await cannedGoods.getByRole("button", { name: "Add" }).click();
  await dialog.locator("li.row input[type=text]").fill("Frozen Goods");
  await dialog.locator("li.row").getByRole("button", { name: "Add" }).click();
  await expect(dialog).toContainText("Frozen Goods");

  // Closing the modal once must refresh both rows' category lists from exactly
  // one GET, as spec 26 requires of the location fields — openCategoryField is
  // its own copy of that loop, so the location test's count does not cover it.
  let categoryGets = 0;
  const countCategoryGets = async (route) => {
    if (route.request().method() === "GET") categoryGets++;
    await route.continue();
  };
  const categoriesUrl = (url) => url.pathname === `/api/storages/${HOUSEHOLD}/categories`;
  await page.route(categoriesUrl, countCategoryGets);
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Preselected in the field whose trigger opened the modal…
  await expect(first.locator('[data-role="new-product-category"] option', { hasText: "Frozen Goods" })).toHaveCount(1);
  const categoryId = await first.locator('[data-role="new-product-category"]').inputValue();
  expect(categoryId).not.toBe("");
  // …offered, but not forced, on the row that never opened it, whose own
  // choices are exactly as they were left.
  await expect(second.locator('[data-role="new-product-category"] option', { hasText: "Frozen Goods" })).toHaveCount(1);
  await expect(second.locator('[data-role="new-product-category"]')).toHaveValue(CANNED_GOODS);
  await expect(second.locator('[data-role="new-product-name"]')).toHaveValue("Ground Cumin");

  // Only now that both rows are confirmed refreshed: exactly one GET did it.
  expect(categoryGets).toBe(1);
  await page.unroute(categoriesUrl, countCategoryGets);

  // Completing the row's confirm against it: the category created above
  // travels through unchanged, on the same POST .../confirm review.js
  // already sends — no second creation path. The confirm itself is
  // intercepted rather than let through for real, so this job stays
  // "never confirmed" for any other test that reuses it; the second row is
  // rejected so it needs no location of its own.
  await first.locator('[data-role="location"]').selectOption(PANTRY);
  await second.locator('[data-action="reject"]').click();

  let confirmBody;
  await page.route(`**/api/storages/${HOUSEHOLD}/ingest/${LOCATION_MODAL_JOB}/confirm`, async (route) => {
    confirmBody = route.request().postDataJSON();
    await route.fulfill({
      json: { batch_ids: ["00000000-0000-7000-8000-0000000000ff"], products_created: 1, locations_created: 0 },
    });
  });
  await page.click("#confirm");
  await expect(page).toHaveURL(/\/inbox\.html/);

  const accepted = confirmBody.items.find((item) => item.row_id === "0");
  expect(accepted.decision).toBe("accept");
  expect(accepted.new_product.category_id).toBe(categoryId);
});

// docs/specs/27-category-quick-create.md's first acceptance criterion, the
// same "zero" scenario the location test above covers, for categories: "E2E
// Admin Household" has none seeded either, so the placeholder-only select
// and the modal's empty-tree message are both exercised from a genuine cold
// start.
test("a category can be created from the review screen in a storage with zero categories", async ({ page }) => {
  const login = await page.request.post("/api/auth/login", {
    data: { username: "e2e-admin-2", password: "e2e-fixture-password" },
  });
  expect(login.status()).toBe(200);

  await page.goto(`/review.html?storage=${ZERO_LOCATIONS_HOUSEHOLD}&job=${ZERO_LOCATIONS_JOB}`);
  const row = page.locator("#rows .review-row").first();
  await expect(row).toBeVisible();
  await expect(row.locator('[data-role="new-product-category"] option')).toHaveCount(1); // "No category" alone

  await row.locator('[data-role="new-product-category-add"]').click();
  const dialog = page.getByRole("dialog", { name: "Categories" });
  await expect(dialog).toContainText("No categories yet");

  await dialog.getByRole("button", { name: "Add top-level category" }).click();
  await dialog.locator('input[aria-label="Name of the new top-level category"]').fill("Games");
  await dialog.locator("#category-modal-add-root-form button[type=submit]").click();
  await expect(dialog).toContainText("Games");
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toBeHidden();

  // Immediately selectable — no reload.
  await expect(row.locator('[data-role="new-product-category"] option', { hasText: "Games" })).toHaveCount(1);
  await expect(row.locator('[data-role="new-product-category"]')).not.toHaveValue("");
});
