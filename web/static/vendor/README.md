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

## uplot-1.6.31.iife.min.js / uplot-1.6.31.min.css

- **Library:** [uPlot](https://github.com/leeoniya/uPlot), MIT license.
- **Version:** 1.6.31.
- **Fetched from:** `https://cdn.jsdelivr.net/npm/uplot@1.6.31/dist/uPlot.iife.min.js`
  and `https://cdn.jsdelivr.net/npm/uplot@1.6.31/dist/uPlot.min.css`.
- **Why this library:** a single dependency-free IIFE build small enough to
  vendor whole, satisfying `docs/specs/11-reporting-and-analytics.md`'s "no
  npm, no CDN at runtime" constraint — the app has to work on a LAN-only /
  Tailscale-only NAS with no outbound internet
  (`docs/specs/01-architecture-and-deployment.md`).
- **Usage:** `<script src="/vendor/uplot-1.6.31.iife.min.js"></script>` plus
  `<link rel="stylesheet" href="/vendor/uplot-1.6.31.min.css">` expose the
  `uPlot` global. Used by `web/static/js/pages/dashboard.js` for the
  turnover (purchased vs. consumed) trend chart. The location-distribution
  chart is hand-rolled CSS bars instead — one dependency shared by one chart
  that actually needs a time axis, not a library reused just to justify
  vendoring it.
