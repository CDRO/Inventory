// The photo source picker: two explicit controls, Photos (library) and
// Camera, that both feed one selection list (docs/specs/36-photo-source-picker.md).
//
// Replaces the old single `<input type="file" accept="image/*"
// capture="environment">` field, whose `capture` attribute opens the camera
// only, with no way back to the library, on the phone this PWA actually runs
// on. `multiple` was also ignored alongside `capture`.
//
// The two hidden inputs and the ids/data-roles below are part of the
// contract: e2e/specs/ingestion.spec.js and this module's own callers address
// them by these names, not by whatever an implementation happens to choose.

import { el, text, svgEl, clearChildren } from "./dom.js";
import { t, tCount, applyI18n } from "./i18n.js";

function folderIcon() {
  return svgEl(
    "svg",
    { viewBox: "0 0 24 24", width: "24", height: "24", fill: "none", stroke: "currentColor", "stroke-width": "2", "aria-hidden": "true", class: "photo-picker__icon" },
    [
      svgEl("path", {
        d: "M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6Z",
        "stroke-linejoin": "round",
      }),
    ],
  );
}

function cameraIcon() {
  return svgEl(
    "svg",
    { viewBox: "0 0 24 24", width: "24", height: "24", fill: "none", stroke: "currentColor", "stroke-width": "2", "aria-hidden": "true", class: "photo-picker__icon" },
    [
      svgEl("path", {
        d: "M4 8a1 1 0 0 1 1-1h2.5l1-1.5h7l1 1.5H19a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V8Z",
        "stroke-linejoin": "round",
      }),
      svgEl("circle", { cx: "12", cy: "13", r: "3.2" }),
    ],
  );
}

/**
 * mountPhotoPicker(container, opts) renders the two-control picker into
 * `container` and returns a handle to its selection. It replaces whatever
 * `container` already held — call it once per container, into an
 * otherwise-empty element.
 *
 * @param {Element} container
 * @param {{multiple?: boolean, onChange?: (files: File[]) => void, onCameraTap?: (openOsCamera: () => void) => void}} [opts]
 * @returns {{files: () => File[], add: (files: File[]) => void, clear: () => void, setStatus: (text: string) => void}}
 */
export function mountPhotoPicker(container, { multiple = true, onChange = () => {}, onCameraTap } = {}) {
  let selection = [];
  // Keyed by File identity, never by name (two rows can share a name — iOS
  // hands out generic ones, and docs/specs/37-in-page-camera.md's captures
  // are numbered per picker lifetime), so Remove and revocation always act on
  // the exact object a row was rendered for.
  const urlByFile = new Map();

  const libraryInput = el("input", {
    type: "file",
    id: multiple ? "photos-library" : undefined,
    accept: "image/jpeg,image/png",
    multiple: multiple ? true : undefined,
    hidden: true,
  });
  const cameraInput = el("input", {
    type: "file",
    id: multiple ? "photos-camera" : undefined,
    accept: "image/*",
    capture: "environment",
    hidden: true,
  });

  const libraryButton = el(
    "button",
    { type: "button", class: "btn photo-picker__control", "data-role": "pick-library", onclick: () => libraryInput.click() },
    [folderIcon(), el("span", { "data-i18n": "photoPicker.library.caption" }, [text(t("photoPicker.library.caption"))])],
  );
  const cameraButton = el(
    "button",
    {
      type: "button",
      class: "btn photo-picker__control",
      "data-role": "pick-camera",
      onclick: () => {
        if (onCameraTap) {
          onCameraTap(() => cameraInput.click());
        } else {
          cameraInput.click();
        }
      },
    },
    [cameraIcon(), el("span", { "data-i18n": "photoPicker.camera.caption" }, [text(t("photoPicker.camera.caption"))])],
  );

  const statusLine = el("p", { class: "muted", "data-role": "status", hidden: true });
  const controls = el("div", { class: "row photo-picker__controls" }, [libraryButton, cameraButton, libraryInput, cameraInput]);
  const rootChildren = [controls, statusLine];

  let countLine = null;
  let list = null;
  if (multiple) {
    countLine = el("p", { class: "muted", "data-role": "count" });
    list = el("ul", { class: "stack photo-picker__list", id: "photo-list" });
    rootChildren.push(countLine, list);
  }

  const root = el("div", { class: "stack photo-picker" }, rootChildren);
  clearChildren(container);
  container.append(root);
  applyI18n(root);

  function renderCount() {
    if (!countLine) return;
    countLine.textContent = selection.length === 0 ? t("photoPicker.empty") : tCount("photoPicker.count", selection.length);
  }

  function renderList() {
    if (!list) return;
    clearChildren(list);
    selection.forEach((file, index) => {
      const removeLabel = t("photoPicker.remove", { position: index + 1, count: selection.length, name: file.name });
      const row = el("li", { class: "row photo-picker__row" }, [
        el("img", { src: urlByFile.get(file), alt: "", class: "photo-picker__thumb" }),
        el("span", { class: "photo-picker__name" }, [text(file.name)]),
        el(
          "button",
          { type: "button", class: "btn btn--ghost", "aria-label": removeLabel, onclick: () => removeFile(file) },
          [text(t("photoPicker.removeButton"))],
        ),
      ]);
      list.append(row);
    });
  }

  function removeFile(file) {
    const index = selection.indexOf(file);
    if (index === -1) return;
    selection.splice(index, 1);
    const url = urlByFile.get(file);
    if (url) {
      URL.revokeObjectURL(url);
      urlByFile.delete(file);
    }
    renderList();
    renderCount();
    onChange(files());
  }

  function addFiles(newFiles) {
    if (newFiles.length === 0) return;
    if (multiple) {
      for (const file of newFiles) {
        // Guards `add()` (docs/specs/37-in-page-camera.md's viewfinder calls
        // it with a freshly captured File, but a caller handing back the
        // same object twice must not orphan its first object URL or render
        // it as two rows) as much as `handleChange` above, which cannot hit
        // this in practice — a picked File is never the same object twice.
        if (urlByFile.has(file)) continue;
        urlByFile.set(file, URL.createObjectURL(file));
        selection.push(file);
      }
      renderList();
      renderCount();
    } else {
      // Single-photo mode (the scan sheet): no list, so the caller acts on
      // the pick immediately and nothing here needs a thumbnail URL.
      selection = [newFiles[newFiles.length - 1]];
    }
    onChange(files());
  }

  function handleChange(input) {
    // Array.from() first: input.files is a live FileList that empties the
    // instant input.value is reset, and the reset itself is what makes
    // picking the same file again after removing it fire a change event —
    // a file input fires none while it still holds the file it already has.
    const picked = Array.from(input.files);
    input.value = "";
    addFiles(picked);
  }

  libraryInput.addEventListener("change", () => handleChange(libraryInput));
  cameraInput.addEventListener("change", () => handleChange(cameraInput));

  function files() {
    return selection.slice();
  }

  function clear() {
    for (const url of urlByFile.values()) URL.revokeObjectURL(url);
    urlByFile.clear();
    selection = [];
    renderList();
    renderCount();
  }

  function setStatus(message) {
    statusLine.textContent = message || "";
    statusLine.hidden = !message;
  }

  renderCount();

  return { files, add: addFiles, clear, setStatus };
}
