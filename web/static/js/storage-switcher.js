// The header storage switcher (docs/specs/05-frontend-pwa-foundations.md).
//
// A user in exactly one storage never sees this control at all; one with
// several sees a plain select. Choosing a different storage navigates to the
// same page with the new `?storage=` value — a fresh page load, not a
// client-side swap — so the URL always describes what is on screen.

import { el, clearChildren } from "./dom.js";
import { rememberStorageId, withStorageParam } from "./session.js";

/**
 * renderStorageSwitcher renders (or hides) the switcher into `container`.
 *
 * With one storage or fewer, the control is hidden entirely — not merely
 * empty — per docs/specs/05-frontend-pwa-foundations.md: "Exactly one storage
 * → use it, and hide the switcher entirely."
 *
 * @param {Element} container
 * @param {Object} params
 * @param {Array<{id: string, name: string}>} params.storages
 * @param {string} params.currentId
 */
export function renderStorageSwitcher(container, { storages, currentId }) {
  clearChildren(container);

  if (storages.length <= 1) {
    container.hidden = true;
    return;
  }

  container.hidden = false;
  container.classList.add("storage-switcher");

  const select = el(
    "select",
    {
      "aria-label": "Switch storage",
      onchange: (event) => {
        const id = event.target.value;
        rememberStorageId(id);
        location.assign(withStorageParam(id));
      },
    },
    storages.map((storage) =>
      el(
        "option",
        { value: storage.id, selected: storage.id === currentId },
        [storage.name],
      ),
    ),
  );

  container.append(select);
}
