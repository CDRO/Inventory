# 25 — Per-storage barcode cache for full offline-speed recall

**Status:** Under evaluation · **Raised:** 2026-09-20
**Source:** natural follow-up to
[`24-barcode-hot-cache.md`](../specs/24-barcode-hot-cache.md), noted there
as deliberately out of scope pending evidence it is needed.

## The idea

Spec 24 caches the **instance-wide top 500** most-scanned barcodes
client-side, so those resolve with zero network calls. Everything else —
including every one of *this storage's own* products that simply is not
instance-wide-popular — still costs the one round-trip to spec 20's
`GET .../barcodes/{code}`. This spike asks whether a household's own
full `product_barcodes` set (typically a few dozen to a few hundred rows,
per `00-overview.md`'s household scale) should also be cached
client-side, so that literally every repeat scan of *anything the
household has ever scanned before* is instant, not just the globally
common items.

## Why it is attractive

- A household's own pantry is exactly the set of barcodes it scans
  *most*, in absolute terms — more than any instance-wide popularity list
  captures, since "instance-wide top 500" is diluted across every
  storage on the deployment, however many that is.
- Removes the one remaining round-trip for the actual common case: not
  "a popular product anywhere," but "a product this specific household
  already has."

## What it would cost / conflicts with accepted specs

- **It needs delta sync for a new entity type.** `product_barcodes` is
  not in `12-client-api-contract.md`'s cacheable-entity list (`products`,
  `categories`, `locations`, `shopping_lists`, `shopping_list_items`).
  Adding it means: an `updated_at` column (barcodes today have only
  `created_at`, and are never updated — only inserted/deleted, so
  `created_at` might suffice for the "changed since" half, but
  deletions still need a `tombstones` entry type added, exactly the
  same shape spec 12 already built for the other five entities).
- **It reopens the offline-editing boundary, if built carelessly.**
  `00-overview.md`'s non-goal is explicit: no offline-first sync engine.
  Spec 24 stays clearly inside that line because its one network call is
  never skipped. A per-storage cache used only for read-side
  recognition (never for confirming a write without the network) can
  stay inside the same line — but it is a sharper edge to stay on the
  right side of than spec 24's, and deserves the same explicit scrutiny
  spec 24 gave it, not an inherited pass.
- **Multi-storage membership complicates cache scope.** A user in two
  storages would need two separate cached sets (barcodes are
  `(storage_id, barcode)`-scoped, never merged across storages per
  `20-barcode-recall.md`) — client storage and cache-invalidation logic
  roughly doubles in complexity for a user with more than one storage,
  the exact case `05-frontend-pwa-foundations.md`'s storage switcher
  already has to handle carefully elsewhere.

## Gate conditions — all must hold before this is accepted

- [ ] Ship spec 24 and measure real usage: what fraction of scans are
      instance-wide-hot-cache hits vs. this-storage-only misses that
      still cost a round-trip? If the miss rate is already low in
      practice, this spike is solving a problem that barely exists.
- [ ] Decide the delta-sync shape explicitly: extend
      `12-client-api-contract.md`'s cacheable-entity list with
      `product_barcodes` (a real, reviewed amendment to that spec, not
      an ad hoc endpoint), including its tombstone type and its
      `updated_since` semantics.
- [ ] Confirm the read-only boundary holds under review: no code path
      in this feature may let a client confirm a write, an association,
      or a quantity change without the authoritative network call spec
      20 already requires for every other write in the system.
- [ ] Decide multi-storage cache scope (per-storage IndexedDB stores,
      keyed and invalidated independently) before any implementation
      starts, not as a fix afterward.

## Current recommendation

**Wait for spec 24's real-world numbers.** If the instance-wide top-500
cache already resolves the overwhelming majority of repeat scans fast
enough in practice, the added client-storage and delta-sync complexity
here is not worth it. If it does not — because a given household's own
staples rarely coincide with what is popular instance-wide — this
becomes a well-scoped, low-risk follow-up built on machinery spec 12
already established, not a new pattern.
