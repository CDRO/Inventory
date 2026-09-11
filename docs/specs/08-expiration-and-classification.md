# 08 — Expiration & Item Type Classification

Implements PRD Feature 4. Depends on: [`02-data-model.md`](02-data-model.md)
(`products.item_type`, `inventory_batches.expiration_date`).

## Classification

`products.item_type` (see `02-data-model.md`) is one of:

- `perishable` — e.g. fruits, dairy, fresh produce.
- `long_shelf_life` — e.g. canned goods, dry pasta, rice.
- `non_perishable` — e.g. plush toys, trading cards, collectibles,
  tools, clothes — items that don't meaningfully expire.

Set when a product is created (vision ingestion review, shopping-list New
Item flow, or manual product creation) — always user-editable afterward
via a product edit screen (`PATCH /api/storages/{storage_id}/products/{id}`).

## Default-expiry resolution

Shelf-life rules are **data, not code**: they live in
`categories.default_shelf_life_days` (`02-data-model.md`), so a user can
edit them in the category tree without a code change or redeploy — and,
for a specific product, in `catalog_products.default_shelf_life_days`,
which only an admin may set (see "Catalog shelf life", below).

Resolution for a new batch, in order — first non-`NULL` wins:

1. `products.default_shelf_life_days` — this storage's own override for
   this product.
2. `catalog_products.default_shelf_life_days` for the catalog entry the
   product came from (`products.catalog_id`) — the admin-curated value.
3. The product's own category (`products.category_id`).
4. Each ancestor category, walking up the tree toward the root.
5. The `item_type` fallback map in Go (`internal/expiry`), used when
   nothing above defines a value, or when the product has no category at
   all:

```go
// internal/expiry/defaults.go — final fallback only
var itemTypeFallbackDays = map[string]*int{
    "perishable":      intPtr(7),
    "long_shelf_life": intPtr(365),
    "non_perishable":  nil, // no expiration
}
```

**Seed data:** creating a storage seeds it with a starter category tree with
sensible values (`Food` → 365; `Food → Dairy` → 10; `Food → Produce` → 7;
`Food → Meat` → 4; `Food → Canned` → 730; `Household` → NULL;
`Collectibles` → NULL), written in the same transaction as the storage row
so a storage never exists without its tree. These durations are a starting
default the operator should review and adjust in-app — they are not a hard
requirement, and editing them is expected.

The seeding lives at storage creation rather than in a migration, because a
migration cannot do it: storages are created at runtime, long after
migrations have run, and the tree is per storage.

`expiration_date = batch.created_at (date) + resolved_days`, computed
server-side at batch-creation time (in the confirm endpoints of
`06-vision-shelf-ingestion.md` and `07-shopping-list-reconciliation.md`),
and always shown as an editable field in review UIs before the batch is
created — never silently applied without the user seeing it. A resolved
value of "no expiration" leaves `expiration_date` `NULL`.

Batches record where their date came from in
`inventory_batches.expiration_source` (`02-data-model.md`): `'user'` when
a person typed or cleared it, `'derived'` when they accepted the computed
default. That distinction is what makes the cascade below safe.

## Catalog shelf life (admin-only, cascading)

Category rules are coarse — "dairy: 10 days" is wrong for both fresh milk
and hard cheese. So a shelf life may also be set **per product** on the
global catalog entry, by an admin only:

- `PATCH /api/admin/catalog/{id}` with `{default_shelf_life_days}` is the
  **only** permitted update to a `catalog_products` row
  (`02-data-model.md` explains why this single bounded integer does not
  reopen the covert-channel problem that makes the table insert-only).
- Ordinary users never edit the catalog. A household that disagrees with
  the curated value sets `products.default_shelf_life_days` locally, which
  outranks it.

**Cascade against current state.** Changing that value is a correction —
it means the old number was wrong — so it applies to data already in the
database, not only to future batches:

1. Find every product with `catalog_id = {id}`, across all storages.
2. Recompute `expiration_date` for their existing batches **where
   `expiration_source = 'derived'`**, as
   `created_at (date) + newly_resolved_days`.
3. **Never touch a batch with `expiration_source = 'user'`.** A date a
   person read off a package and typed in outranks any rule, forever.
4. Skip products that set their own `products.default_shelf_life_days`,
   since the catalog value does not apply to them.
5. Run it as a background job (`04-backend-api-conventions.md`) — the
   admin gets a count of affected batches, and the work is logged. It
   writes no `inventory_logs` rows: quantities do not change.

The background job applies to **this** cascade, the admin one, because it
crosses every storage and is unbounded in size.

The same cascade runs, scoped to one storage, when a user changes
`categories.default_shelf_life_days` or `products.default_shelf_life_days`,
or re-files a product into a different category: derived dates are
recomputed, user-set dates are left alone. Those run **inline, before the
response**, and return the number of batches they touched. They are bounded
by one household's rows, and running them inline means a user who changes a
rule and then looks at their inventory sees the new dates rather than the
old ones.

## Editing / removing expiry

- `PATCH /api/storages/{storage_id}/inventory-batches/{id}` accepts
  `{expiration_date: "YYYY-MM-DD" | null}` to edit or clear a batch's
  expiration independent of its product's default rule. Either way, the
  write also sets `expiration_source = 'user'`.
- Clearing (`null`) is a valid, first-class state — e.g. a canned good the
  user knows has no printed date — not an error state.

### Two kinds of "no expiry", and why the difference matters

`expiration_date IS NULL` can mean two different things, and
`expiration_source` is what tells them apart:

| State | Meaning | Behavior on a rule change or cascade |
|---|---|---|
| `NULL` + `derived` | No rule produced a date (e.g. a `non_perishable` item) | Recomputed like any derived value — if a rule later supplies a shelf life, this batch gets a date |
| `NULL` + `user` | **A person deliberately said this has no expiry** | Never touched again by any cascade, rule edit, category reassignment, or admin correction |

So removing an expiration date is a **sticky, deliberate statement**, not
an empty field waiting to be refilled. Without this distinction, a user who
clears the date on a jar with no printed date would find the system
helpfully putting one back the next time an admin adjusted a shelf-life
rule — and would reasonably conclude the app ignores them.

- The UI must reflect this: a batch showing "no expiry" that a person set
  reads as *"No expiry (set by you)"*, not as a blank field.
- **Getting back to automatic:** a `{expiration_source: "derived"}` reset
  on the same `PATCH` clears the manual flag and immediately recomputes
  the date from the current rules. Without this, an accidental edit would
  permanently opt a batch out of the rules with no way back — so this
  action is required, not optional, and is surfaced in the batch editor as
  "Use the automatic date again".

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
- A batch whose `expiration_source = 'user'` never has its
  `expiration_date` changed by any cascade, rule edit, category
  reassignment, or admin action — only by a person editing that batch.
- Changing a shelf-life rule (`categories.default_shelf_life_days`,
  `products.default_shelf_life_days`, or the admin-only catalog value)
  recomputes `expiration_date` on existing `derived` batches, and takes
  effect immediately for new ones, with no redeploy.
- Re-assigning a product to a different category likewise recomputes
  `derived` dates for its existing batches: `derived` means "follows the
  current rules", and a stale derived date is simply a wrong one.
- Clearing a batch's expiration date sets `expiration_source = 'user'`,
  and no later cascade, rule change, or admin action ever gives that batch
  a date again — only the explicit "use the automatic date again" reset
  does.
- Changing a product's `item_type` affects resolution only where it is
  actually consulted — the final fallback — and never overrides a more
  specific rule.
