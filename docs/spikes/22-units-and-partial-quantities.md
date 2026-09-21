# 22 — Units & partial quantities

**Status:** Under evaluation · **Raised:** 2026-09-20
**Source:** project owner; also a prerequisite for the aggregation goal in
[`23-cross-brand-product-groups.md`](23-cross-brand-product-groups.md)
("how many kg of penne, regardless of brand").

## The idea

Today every quantity in the system is a **unit count**:
`inventory_batches.quantity INT`, log deltas in whole units, reorder
arithmetic in units. That models cans and rolls perfectly and models
flour, rice, oil, and opened packages poorly. This spike evaluates
adding measurement to products:

- A per-product **unit** (`piece` | `g` | `kg` | `ml` | `l`) and/or a
  per-product **content size** ("500 g per package"), so a count of
  packages can be projected to a weight/volume total.
- Optionally, **partial quantities** — an opened package tracked at
  "0.4 remaining" — which is the far more invasive half.

## Why it is attractive

- "How much rice do I actually have" is a weight question, not a
  package-count question; today the honest answer requires mental
  arithmetic across batches.
- Spike `23` (brand-agnostic groups) wants totals in kg; without a
  content size per product, cross-brand aggregation can only ever count
  packages of different sizes as equal, which is wrong in exactly the
  way that matters.
- Shopping-list lines already arrive with quantities like "2kg" that
  the parser currently reduces to a bare count.

## What it would cost

This touches nearly every spec below the waterline, which is why it is
a spike and not a PR:

- `02-data-model.md` — `products` gains unit/content columns;
  `inventory_batches.quantity` either stays INT-of-packages (cheap) or
  becomes NUMERIC to allow partial packages (expensive: every CHECK,
  every sum, every API shape).
- `06`/`09` — the Gemini response schemas and both review UIs grow a
  unit dimension ("3 × 500 g"); the consumption flow needs "used half a
  package" input if partials are in scope.
- `07` — quantity parsing from list lines ("2kg flour") must resolve
  against the product's unit.
- `10` — `min_stock` semantics fork: is the threshold 2 packages or
  1 kg? Both are defensible; one must be chosen.
- `11` — turnover charts need a unit axis or must stay package-based.
- `12` — additive API change at best; a type change to `quantity` is a
  breaking change under the versioning policy.

## Conflicts with accepted specs

- `02-data-model.md` fixes `quantity INT NOT NULL DEFAULT 1
  CHECK (quantity >= 0)` and every log delta as a signed integer.
- `09-consumption-logging.md`'s decrement arithmetic, batch-zero
  deletion, and over-decrement `422` all assume integers.
- `10-reorder-and-shopping-export.md` defines suggested reorder
  quantity as integer subtraction.

None of these are wrong today; they are what makes this spike's cheap
form attractive and its full form expensive.

## The cheap form worth evaluating first

Add **`unit` and `content_per_unit`** to products as *display/derived
metadata only*: batches keep counting integer packages, and every
weight/volume figure is a computed projection
(`count × content_per_unit`) shown in product detail, dashboard, and
group aggregates (`23`). No schema change to batches or logs, no
breaking API change, no review-UI redesign. Partial packages stay out.

This delivers the "how many kg" answer and unblocks spike `23` at a
fraction of the cost, and leaves partial-quantity tracking as a
separately gated follow-up.

## Gate conditions — all must hold before this is accepted

- [ ] Decide the scope split explicitly: metadata-only projection (cheap
      form) vs. NUMERIC partial quantities — and if the latter, name
      the `12-client-api-contract.md` versioning consequence.
- [ ] Decide `min_stock` semantics under units (packages vs. measure),
      with `10`'s arithmetic rewritten on paper first.
- [ ] Confirm the vision prompt can reliably read content sizes off
      labels ("500g") or that the field is user/catalog-supplied only —
      no silent wrong-by-10× data.
- [ ] Decide whether `catalog_products` carries `content_per_unit`
      (same insert-only rules would apply) so the second household gets
      it for free.
- [ ] A worked migration plan for existing integer-only data.

## Current recommendation

Evaluate the **cheap form** for promotion into a numbered spec; keep
partial quantities parked until a real household need (tracked opened
packages) is demonstrated rather than assumed. If `23` is wanted soon,
the cheap form is the fastest honest path to its kg totals.
