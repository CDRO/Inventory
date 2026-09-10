// Registers the service worker (docs/specs/05-frontend-pwa-foundations.md).
// Imported once by every page module for its side effect; not part of the
// spec's named shared-module list, but every page needs this one line and
// duplicating it per page is the wrong kind of copy-paste.

if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch((err) => {
      // A failed registration must never block the app — it degrades to
      // "not installable / not cached", not "broken".
      console.warn("Service worker registration failed:", err);
    });
  });
}
