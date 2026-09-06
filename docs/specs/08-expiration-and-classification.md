# 08 — Expiration & Item Type Classification

Implements PRD Feature 4. Depends on: [`02-data-model.md`](02-data-model.md)
(`products.item_type`, `inventory_batches.expiration_date`).

## Classification

`products.item_type` (see `02-data-model.md`) is one of:

- `perishable` — e.g. fruits, dairy, fresh produce.
- `long_shelf_life` — e.g. canned goods, dry pasta, rice.
- `non_perishable` — e.g. plush toys, trading cards, collectibles,
  tools — items that don't meaningfully expire.

Set when a product is created (vision ingestion review, shopping-list New
Item flow, or manual product creation) — always user-editable afterward
via a product edit screen (`PATCH /api/storages/{storage_id}/products/{id}`).

## Default-expiry resolution

Shelf-life rules are **data, not code**: they live in
`categories.default_shelf_life_days` (`02-data-model.md`), so a user can
edit them in the category tree without a code change or redeploy.

Resolution for a new batch, in order — first non-`NULL` wins:

1. The product's own category (`products.category_id`).
2. Each ancestor category, walking up the tree toward the root.
3. The `item_type` fallback map in Go (`internal/expiry`), used when no
   category in the chain defines a value, or when the product has no
   category at all:

```go
// internal/expiry/defaults.go — final fallback only
var itemTypeFallbackDays = map[string]*int{
    "perishable":      intPtr(7),
    "long_shelf_life": intPtr(365),
    "non_perishable":  nil, // no expiration
}
```

**Seed data:** the initial migration creates a starter category tree per
new storage with sensible values (`Food` → 365; `Food → Dairy` → 10;
`Food → Produce` → 7; `Food → Meat` → 4; `Food → Canned` → 730;
`Household` → NULL; `Collectibles` → NULL). These durations are a starting
default the operator should review and adjust in-app — they are not a hard
requirement, and editing them is expected.

`expiration_date = batch.created_at (date) + resolved_days`, computed
server-side at batch-creation time (in the confirm endpoints of
`06-vision-shelf-ingestion.md` and `07-shopping-list-reconciliation.md`),
and always shown as an editable field in review UIs before the batch is
created — never silently applied without the user seeing it. A resolved
value of "no expiration" leaves `expiration_date` `NULL`.

## Editing / removing expiry

- `PATCH /api/storages/{storage_id}/inventory-batches/{id}` accepts
  `{expiration_date: "YYYY-MM-DD" | null}` to edit or clear a batch's
  expiration independent of its product's default rule.
- Clearing (`null`) is a valid, first-class state — e.g. a canned good the
  user knows has no printed date — not an error state.

## Sorting & filtering by urgency

The inventory list view (part of `05-frontend-pwa-foundations.md`'s
product/inventory screens) supports sorting/filtering by urgency, computed
client-side from `expiration_date` relative to "today":

| Urgency | Condition |
|---|---|
| Expired | `expiration_date < today` |
| Critical | `today <= expiration_date < today + 3 days` |
| Soon | `today + 3 days <= expiration_date < today + 14 days` |
| OK | `expiration_date >= today + 14 days` |
| No expiry | `expiration_date IS NULL` |

- Default sort on the dashboard's "expiring soon" widget (see
  `11-reporting-and-analytics.md`) is ascending by `expiration_date`, nulls
  last.
- Urgency-to-color mapping lives in the CSS custom properties in
  `web/static/css/tokens.css` (`--urgency-expired`, `--urgency-critical`,
  `--urgency-soon`, `--urgency-ok`, `--urgency-none`; see
  `05-frontend-pwa-foundations.md`) so it is consistent everywhere urgency
  is shown, with no literal colors in component CSS.

## Acceptance criteria

- Creating a batch without an explicit expiration date always applies the
  resolved default (or `NULL` when the resolution yields "no expiration"),
  never leaves it unset by omission.
- Changing a product's `item_type` or `category_id` after creation does
  not retroactively change `expiration_date` on already-existing batches —
  it only affects defaults for batches created afterward.
- Editing `default_shelf_life_days` on a category takes effect immediately
  for subsequently created batches, with no redeploy.
