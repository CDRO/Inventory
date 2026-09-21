# Spikes — candidate work, not yet accepted

This folder holds **ideas under evaluation**. Nothing here is a
commitment, a requirement, or an implementation contract.

## Instructions for implementing agents

**Do not implement anything in this folder.** The only implementation
contract is `docs/specs/`. A spike becomes work by being promoted into a
numbered spec under `docs/specs/` and then into the issue queue — never
by being read from here.

If a spike contradicts an accepted spec, the accepted spec wins until a
human decides otherwise. Contradictions are recorded in the entry on
purpose; they are the open questions, not oversights to fix on your own.

## Numbering — one number space with `docs/specs/`

Specs and spikes share the `00`–`49` number space defined in
`docs/specs/00-overview.md`. A number is claimed by **exactly one**
file, in exactly one of the two folders:

- A number in `docs/specs/` is accepted work.
- A number in `docs/spikes/` is a candidate. **Promotion keeps the
  number**: the file moves from `docs/spikes/NN-*.md` to
  `docs/specs/NN-*.md` (rewritten as a contract, with the spike's
  history condensed into a "why" section), so references never dangle
  and history stays greppable.
- A rejected spike keeps its file and number, marked rejected — numbers
  are never reused.

The former `S-01` entry (previously inline in this README) is now
[`21-native-android-app.md`](21-native-android-app.md) under this
scheme.

## Current spikes

| # | Spike | Status |
|---|---|---|
| 21 | [Native Android app with on-device vision (Gemma)](21-native-android-app.md) | App deferred; server-side contract accepted as spec `12`; barcode portion promoted to spec `20` on 2026-09-20 |
| 22 | [Units & partial quantities](22-units-and-partial-quantities.md) | Under evaluation |
| 23 | [Cross-brand product groups](23-cross-brand-product-groups.md) | Under evaluation — gated in part on 22 |
| 25 | [Per-storage barcode cache for full offline-speed recall](25-per-storage-barcode-cache.md) | Under evaluation — gated on real usage data from spec `24` |
| 31 | [Expiry reminders from the installed PWA, without a push service](31-on-device-expiry-reminders.md) | Under evaluation — gated on measuring `periodicsync` and badging on the owner's own devices; author recommends at most the on-open part |

## Entry format

Each spike records what it is, why it is attractive, what it would cost,
what it conflicts with, and **the conditions that must be true before it
is accepted**. A spike with no gate conditions is not ready to be a
spike.
