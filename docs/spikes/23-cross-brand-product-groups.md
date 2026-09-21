# 23 — Cross-brand product groups

**Status:** Under evaluation · **Raised:** 2026-09-20
**Source:** project owner: *"wenn ich z. B. Barilla Penne habe und
Buitoni Penne, dass beides als Penne gekennzeichnet wird — was mir
erlaubt zu wissen, wie viele kg Penne ich habe, egal von welcher
Marke."*

## The idea

A storage-local **product group** ("Penne") that several concrete
products (Barilla Penne 500g, Buitoni Penne 1kg) belong to, so that
stock, reorder, and reporting can be viewed at the *kind-of-thing* level
across brands:

```
product_groups: id (UUIDv7), storage_id, name, created/updated_at
products:       + group_id UUID NULL REFERENCES product_groups(id)
```

- Group membership is optional — ungrouped products behave exactly as
  today, and the whole feature is invisible until a group is created.
- A product belongs to at most one group (a group is "what this thing
  is", not a tag system).
- Aggregation: group stock = sum over member products' `current_stock`;
  **in kg/l only via the unit metadata from spike
  [`22`](22-units-and-partial-quantities.md)** — without `22`, a group
  can honestly aggregate package counts, but 500 g and 1 kg packages
  would count as "2", which is precisely the wrong answer the owner's
  example names. `23`'s headline benefit therefore *depends on at least
  `22`'s cheap form*.

## Why this is not the same thing as `catalog_products.base_id`

The catalog's variant graph (`02-data-model.md`) looks similar and is a
different axis on purpose:

- `base_id` links **names of the same product** ("cherry tomatoes" →
  "tomatoes") in the **global, anonymous** catalog, built as a side
  effect of matching, to make abbreviated shopping-list lines
  resolvable (`07-shopping-list-reconciliation.md`).
- A product group links **different products that are interchangeable
  for the household's purposes**, is **storage-local**, deliberately
  curated by the user, and never global — whether Barilla and Buitoni
  penne are "the same" is a household judgment, not a fact about the
  world, and a global grouping would be a cross-storage channel.

A promotion of this spike should state that distinction in the spec, or
the two mechanisms will be merged by a well-meaning refactor into one
wrong thing.

## Why it is attractive

- Answers the real pantry question ("do we have pasta?") instead of the
  database's question ("do we have this SKU?").
- Makes reorder thresholds meaningful for interchangeable goods: the
  household needs *some* penne on hand, not specifically Barilla.
- Gives reporting (`11`) a level of aggregation that matches how people
  actually think about stock.

## What it would cost / conflicts with accepted specs

- **`10-reorder-and-shopping-export.md` is the hard part.** If a group
  gets its own `min_stock`, the low-stock rules fork: does a product's
  own threshold still apply inside a group? Does the export list the
  group or the members? Today's spec is written entirely per-product,
  and both semantics changing and coexisting need real design.
- **`07`/matching:** should a shopping-list line "Penne" match the
  group (and then which member is being bought?), or keep matching
  products only, with groups as a pure viewing layer? The second is far
  cheaper and probably right for a first version.
- **`11-reporting-and-analytics.md`:** group-level rollups are additive
  but need `22`'s unit projection to be truthful.
- **UI:** group management (create, name, assign from product edit
  screen `16`), a grouped view on the product list, a group card in the
  dashboard. All additive, none free.
- **`12-client-api-contract.md`:** additive fields/endpoints only —
  no breaking change is inherent to the idea, which is a point in its
  favor.

## Gate conditions — all must hold before this is accepted

- [ ] Spike `22` (at least its cheap form: `unit` +
      `content_per_unit`) accepted or promoted together with this —
      otherwise the kg aggregation the spike exists for cannot be
      delivered honestly.
- [ ] Decide reorder semantics: group-level `min_stock` (and its
      interaction with member thresholds) **or** explicitly defer
      reorder integration to a second iteration, keeping groups
      view-only at first.
- [ ] Decide matching semantics: groups invisible to `07` matching in
      v1 (recommended), or specified interaction.
- [ ] Decide whether group assignment is manual-only (recommended: it
      is a judgment call) or AI-suggested — an AI suggestion path would
      need its own review-and-confirm shape per house rules.
- [ ] Confirm the one-group-per-product model survives contact with the
      owner's real pantry (vs. wanting overlapping tags).

## Current recommendation

Attractive and well-contained **if** it lands as a *view-layer first*:
groups + membership + aggregated display (with `22`'s cheap unit
projection), no reorder or matching changes in the first cut. Promote
in that shape once `22`'s scope decision is made; treat group-level
reorder as its own follow-up spike so the hard `10` questions don't
block the easy win.
