# Vendored libraries

Single-file builds only, loaded with a plain `<script>` tag — no CDN at
runtime (`docs/specs/01-architecture-and-deployment.md`: the app has to work
on a LAN-only / Tailscale-only NAS with no outbound internet) and no build
step (`docs/specs/05-frontend-pwa-foundations.md`). Each file's pinned
version is in its filename; this document records where it came from.

## jspdf-2.5.1.umd.min.js

- **Library:** [jsPDF](https://github.com/parallax/jsPDF), MIT license.
- **Version:** 2.5.1.
- **Fetched from:** `https://cdnjs.cloudflare.com/ajax/libs/jspdf/2.5.1/jspdf.umd.min.js`
- **Why this version:** the last 2.x release before jsPDF's 3.x/4.x API and
  packaging changes; pairs with jspdf-autotable 3.x below, which targets the
  2.x `window.jspdf.jsPDF` UMD global.
- **Usage:** `<script src="/vendor/jspdf-2.5.1.umd.min.js"></script>` exposes
  `window.jspdf.jsPDF`.

## jspdf-autotable-3.8.4.umd.min.js

- **Library:** [jsPDF-AutoTable](https://github.com/simonbengtsson/jsPDF-AutoTable), MIT license.
- **Version:** 3.8.4.
- **Fetched from:** `https://cdnjs.cloudflare.com/ajax/libs/jspdf-autotable/3.8.4/jspdf.plugin.autotable.min.js`
- **Why this version:** the last 3.x release, built against jsPDF 2.x's UMD
  global (`window.jspdf`) rather than the 4.x/5.x ESM-first packaging.
- **Usage:** loaded after jspdf-2.5.1.umd.min.js, it patches
  `jsPDF.API.autoTable` directly — call `doc.autoTable({...})` on a `jsPDF`
  instance. Handles its own pagination (repeats the header row, breaks pages
  automatically), which is why the reorder PDF export
  (`docs/specs/10-reorder-and-shopping-export.md`) does not paginate itself.

Used by `web/static/js/pages/dashboard.js` for the client-side PDF export of
the reorder list.
