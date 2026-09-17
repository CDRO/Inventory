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
5. Runs **inline, before the response** — the admin gets a real count of
   affected batches back in `{default_shelf_life_days, recomputed_batches}`,
   the same shape `PATCH .../categories/{id}/shelf-life` returns. It writes
   no `inventory_logs` rows: quantities do not change.
6. **The new value and every recompute share one transaction.** A
   correction that stops part-way — the request cancelled, a timeout, an
   error on one product — rolls back whole: the entry keeps its old value
   and no batch in any storage has moved, so a retry is the complete fix.
   Committing per product would instead leave some households on the new
   date and some on the old, with the entry already showing the new value
   and nothing recording how far the cascade got.

This crosses every storage, unlike the category- and product-scoped cascades
below, but it is still the same shape of work: a bounded set of local
`UPDATE`s against this server's own database, not a call to anything
external. The background-jobs machinery in `04-backend-api-conventions.md`
exists for slow *external* calls (the vision API) that must not block a
request; recomputing rows already on disk is not that, so this cascade runs
inline like its storage-scoped siblings rather than through the jobs table.

The same cascade runs, scoped to one storage, when a user changes
`categories.default_shelf_life_days` or `products.default_shelf_life_days`,
re-files a product into a different category, or moves a category to a
different parent — which re-files every product in the moved subtree at
once, since their category chains now climb through different ancestors:
derived dates are recomputed, user-set dates are left alone. Those run
**inline, before the response**, because they are bounded by one household's
rows and because a user who changes a rule and then looks at their inventory
should see the new dates rather than the old ones. Re-filing a product and
moving a category recompute in the same transaction as the change itself, so
a product's category and its batches' dates are never seen disagreeing.

The category-rule change and the admin catalog change above both report a
count, since both are performed *in order to* change dates. Re-filing a
product, or moving a category, is a change of category that happens to move
dates as a consequence, so the recompute is silent. Changing
`products.default_shelf_life_days` has no endpoint yet.

## The category tree

Categories are where the storage-scoped rules live, so a household edits
them in its own category tree. The tree has the same shape and the same
invariants as the location tree (`02-data-model.md`), and its routes mirror
`06-vision-shelf-ingestion.md`'s location routes, with the same rules:
every route is storage-scoped and behind `RequireStorageMember`, and **every
category id in a request — the path id, `parent_id`, a move target — must
belong to the storage in the URL**, a foreign id answering exactly like a
nonexistent one (`404 not_found`, never `403`;
`03-auth-and-multi-tenancy.md`).

- `GET /api/storages/{storage_id}/categories` — the tree of that storage
  only, nested: `{id, name, default_shelf_life_days, children: [...]}`.
  `default_shelf_life_days` is always present; `null` means the node sets no
  rule and inherits.
- `POST /api/storages/{storage_id}/categories` — body `{name, parent_id,
  default_shelf_life_days}`, the last two optional. A given `parent_id` must
  be in this storage, or `404`. A shelf life set here needs no cascade: no
  product can be filed under a category that did not exist. Records a
  `category_created` contribution (`51-gamification-scoring.md`).
- `PATCH /api/storages/{storage_id}/categories/{id}` — rename, or move by
  changing `parent_id` (absent leaves the parent alone; `null` makes the node
  a root). `404` if `{id}` or the new parent is not in this storage; `409
  conflict` if the move would create a cycle. A move recomputes the moved
  subtree's derived dates, as above. It does **not** change the shelf-life
  rule: a body carrying `default_shelf_life_days` is refused with `422`
  rather than silently ignored.
- `PATCH /api/storages/{storage_id}/categories/{id}/shelf-life` — body
  `{default_shelf_life_days}` (a whole number of days, `0`–`36500`, or `null`
  to inherit). Runs the cascade and returns `{default_shelf_life_days,
  recomputed_batches}`.
- `DELETE /api/storages/{storage_id}/categories/{id}` — removes the node and
  its subtree. `409 conflict` while any product is filed anywhere in that
  subtree (`02-data-model.md`).

The frontend is `categories.html`, using the shared tree component
(`05-frontend-pwa-foundations.md`) exactly as `locations.html` does, plus
each node's rule. The rule label always states which rule is in force — the
node's own ("10 days"), the nearest ancestor's ("365 days (from Food)"), or
"By item type" when nothing above sets one — so an inheriting node is never
mistaken for one with no expiry. Saving a rule shows the returned count of
dates it moved.

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
  current rules", and a stale derived date is simply a wrong one. So does
  moving a category to a different parent, for every product in its
  subtree.
- An admin catalog correction that does not finish leaves the entry's value
  and every batch exactly as they were before it started.
- Clearing a batch's expiration date sets `expiration_source = 'user'`,
  and no later cascade, rule change, or admin action ever gives that batch
  a date again — only the explicit "use the automatic date again" reset
  does.
- Changing a product's `item_type` affects resolution only where it is
  actually consulted — the final fallback — and never overrides a more
  specific rule.
