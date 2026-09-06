# 11 — Reporting & Analytics

Implements PRD Feature 7. Depends on: [`02-data-model.md`](02-data-model.md)
(`inventory_logs` as the primary data source), [`10-reorder-and-shopping-export.md`](10-reorder-and-shopping-export.md)
(shares dashboard screen real estate).

## Dashboard metrics

`GET /api/storages/{storage_id}/dashboard/analytics` returns:

```json
{
  "total_items": 128,
  "location_distribution": [
    { "location_id": 4, "location_name": "Basement > Right Shelf", "item_count": 37 }
  ],
  "turnover": [
    { "period": "2026-08", "purchased": 42, "consumed": 35 }
  ]
}
```

- **Total items stored:** `SUM(inventory_batches.quantity)` across the
  storage.
- **Distribution per location:** `SUM(quantity)` grouped by `location_id`,
  with the full ancestor path (e.g. `"Basement > Right Shelf"`) resolved
  server-side for display, joined against `locations`
  (`02-data-model.md`). Rendered as a simple bar chart or heatmap-style
  grid in the frontend — a literal geographic heatmap is not required,
  a magnitude-colored grid/list satisfies "heatmap" from the PRD.
- **Turnover / change log over time:** aggregate `inventory_logs` by
  month (or a `?granularity=week|month` query param), summing
  `change_qty` separately for positive (`purchase` + `vision_ingestion`)
  and negative (`consumption`) reasons, so the frontend can render an
  in/out trend chart.

## Frontend

`dashboard.html` (`05-frontend-pwa-foundations.md`), sharing the page with
`10-reorder-and-shopping-export.md`'s widgets:

- A total-items stat tile.
- A location-distribution chart (bar chart, sorted descending by
  `item_count`; a true 3D/heatmap visualization is not required for a
  first pass).
- A turnover line/bar chart (purchased vs. consumed per period).

**Charting under the no-toolchain constraint:** there is no npm, so a
charting library may only be used if it ships as a **single self-contained
file committed to `web/static/vendor/`** and loadable directly by the
browser (uPlot and Chart.js both qualify; pin the version in the filename
and note its origin in a `vendor/README.md`). It must be **vendored, never
loaded from a CDN at runtime** — the app has to work on a LAN-only /
Tailscale-only NAS with no outbound internet
(`01-architecture-and-deployment.md`). If no suitable single-file library
is available, hand-rolled inline SVG for these three simple charts is an
acceptable fallback — but do not introduce a build step to get a nicer
chart library.

## Acceptance criteria

- All three metrics derive from `inventory_batches`/`inventory_logs`
  directly at query time (or from a materialized/cached rollup refreshed
  on write, if query performance ever warrants it — not required at the
  household scale defined in `00-overview.md`); there is no separate
  hand-maintained analytics table to keep in sync.
- Turnover figures reconcile with the reorder dashboard: a product that
  moves from in-stock to out-of-stock in a period shows a corresponding
  `consumed` delta in that period's turnover data.
- Analytics are scoped to the active storage only, consistent with the
  multi-tenancy rules in `03-auth-and-multi-tenancy.md` — never aggregate
  across storages.
