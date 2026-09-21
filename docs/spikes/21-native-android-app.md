# 21 — Native Android app with on-device vision (Gemma)

*(Formerly `S-01`, inline in this folder's README; renumbered into the
shared spec/spike number space with no content decision implied.)*

**Status:** App deferred · **server-side contract accepted** (spec `12`)
· **barcode portion promoted** (spec `20`, 2026-09-20) · **Raised:** 2026-09-09
**Source:** external draft spec, "20 — Android App: On-Device Vision mit
Gemma 4". Held outside the repo deliberately — copying it in would read as
acceptance.

## Decision, 2026-09-09

The app will be **built in a separate repository by someone else**, in Android
Studio. It never enters this codebase, so the toolchain objection below no
longer applies: no Kotlin, no Gradle, no second build path here, and the
Docker-only constraint is untouched. The escalation-latency, model-download and
device-fragmentation risks become that team's problem, not this project's.

What this repo owes the app is a **stable API contract**, now specified in
[`docs/specs/12-client-api-contract.md`](../specs/12-client-api-contract.md):
QR device pairing, Bearer session transport, idempotent writes, delta sync, a
versioning policy, and the rules about what the server must never trust from a
client. That spec is accepted work; this spike entry is not.

## Decision, 2026-09-20 — barcode leaves this spike

The original containment plan kept barcode support on this spike's branch,
so a failed on-device experiment could be reverted cleanly. The project
owner has since decided the narrower, server-side form of the idea on its
own merits: **a barcode as a local recall key for already-identified
products** is now accepted work, specified in
[`docs/specs/20-barcode-recall.md`](../specs/20-barcode-recall.md), with the
matching amendment to the non-goal in `00-overview.md`. External
UPC/EAN-database identification remains a non-goal.

For this spike that means: the barcode fast path the draft wanted now
exists (or will exist) server-side and in the PWA regardless of whether the
app is ever built, the `products` schema question is settled by spec `20`'s
own tables, and the containment reasoning below is retained as history
only. A native app would consume spec `20`'s endpoints like any paired
client.

Everything below is retained as the record of why the app itself is still a
spike rather than accepted work.

## The idea

A native Android app (Jetpack Compose + CameraX) running Gemma on-device via
LiteRT-LM, with a deterministic router that keeps trivial single-item scans
local and sends only multi-object shelf scans to the cloud. Adds a barcode fast
path, a local SQLite catalog cache, and an offline queue for consumption
bookings.

## Why it is attractive

- Most scans are trivial — one roll of toilet paper, one apple — and each one
  currently costs a full cloud vision call.
- Sub-second local feedback instead of an upload round-trip on mobile data.
- Basic consumption logging would keep working with no connectivity.

The cost and latency arguments are sound in principle. The problems below are
about fit and evidence, not about the idea being bad.

## Conflicts with accepted specs — resolve these first

Each of these contradicts something `docs/specs/` currently states as settled.
Accepting the draft as written means reopening a decision that was made
deliberately.

1. ~~**Barcode scanning is an explicit non-goal.**~~ **Resolved 2026-09-20**
   by the decision above: the recall form is accepted (spec `20`), the
   barcode-database form remains a non-goal. The draft's *mandatory
   barcode-first* router should now be reconciled against spec `20`'s
   endpoints instead of inventing its own.
2. **Offline-first is an explicit non-goal.** `00-overview.md` rules out an
   "offline-first / conflict-resolution sync engine," and says the PWA's
   offline capability exists "for installability and camera access, not offline
   data editing." The draft's offline queue with idempotency keys and delta
   sync is precisely the engine that was ruled out.
3. **It declares no backend data-model changes, then requires several.**
   `idempotency_key`, `inference_source`, `raw_label` without a product id, and
   `updated_since` delta sync are all changes to `02-data-model.md` and
   `04-backend-api-conventions.md`.
4. **Integer product ids.** The draft's payload carries `"product_id": 4711`.
   This project uses UUIDv7 throughout, specifically because sequential ids
   leak the existence of other storages (`02-data-model.md`,
   `03-auth-and-multi-tenancy.md`). The data contract is incompatible as
   written.
5. **Numbering mismatch.** It declares a dependency on `specs/10-*` as
   "Inventory Core, REST API, Synology Backend." Here `10` is reorder and
   export; the real dependencies are `02`, `04`, `06` and `09`. The draft was
   written against a different spec set and has not been reconciled with this
   one.
6. **No telemetry concept exists here.** The draft's entire quality strategy
   rests on telemetry (its §9) that this project has never specified — in a
   system whose defining characteristic is that it is self-hosted and private.

## Overhead it would introduce

- ~~**A second toolchain, against the project's central constraint.**~~
  **Resolved** by the 2026-09-09 decision: the app lives in its own repository,
  so no Android toolchain enters this one.
- **A second client** to keep in step with every backend change, on top of the
  PWA, which the draft explicitly keeps. This one remains, and is the reason
  `12-client-api-contract.md` specifies a versioning policy: the app ships
  through an app store on its own schedule and cannot be updated in lockstep
  with a deploy to the NAS.
- **A second identification path** with different accuracy from the cloud one —
  a permanent source of "why did it say that?"
- **A 2.6 GB model download** per device before any benefit appears at all.

## UX degradation risks

- **Two paths, two behaviours.** The draft acknowledges this (US-05, surfacing
  `inference_source`), but telling the user which engine guessed is a
  disclosure, not a fix.
- **Escalation is slower than going straight to the cloud.** A local inference
  that runs up to 8 s and then escalates costs the user more time than a direct
  upload would have.
- **A high escalation rate makes it all cost and no benefit** — slower scans, a
  large download, and the cloud calls still happen. The draft's own risk table
  names this first.
- **Device fragmentation.** Under 8 GB RAM gets no local path at all, so the
  feature's value depends on which phone the household happens to own.

## What actually needs a native app — and what does not

The draft bundles several independent wins and attributes all of them to the
app. Only one genuinely requires Android:

| Want | Needs a native app? |
|---|---|
| On-device model inference | **Yes** — not realistically achievable in the PWA |
| Local catalog cache for instant lookup | No — IndexedDB in the existing PWA |
| Offline queue for consumption bookings | No — service worker + IndexedDB |
| Faster capture, smaller uploads | No — client-side downscale before upload |
| Barcode scanning | No — now accepted as spec `20`, PWA-side (`BarcodeDetector` + manual entry) |

**This is the most useful observation in the spike.** If the cheap rows deliver
most of the cost and latency win, the expensive row may never be worth
building. Measure that before starting an Android project, not after. With
spec `20` accepted, the barcode row — the cheapest of them all — no longer
even needs this spike.

## Gate conditions — all must hold before this is accepted

- [ ] Measure current cloud-vision spend and what share of real scans are
      genuinely single-item. If trivial scans are not the bulk of usage, stop.
      (Spec `20` will shrink this share further: repeat scans become free
      without any app.)
- [ ] Deliver the non-native rows above in the PWA first, then measure what is
      left. The remaining saving is the actual budget for the app.
- [ ] Run the draft's Phase 0: Gemma E2B on real household photos against the
      cloud baseline, to establish the escalation rate on real data. An
      escalation rate much above a third invalidates the cost case.
- [x] Decide explicitly, in `00-overview.md`, whether barcode scanning and
      offline editing stop being non-goals. **Barcode: decided 2026-09-20**
      (recall form accepted, spec `20`; database form stays a non-goal).
      **Offline editing: still a non-goal**, unchanged.
- [ ] Confirm an Android build can coexist with the Docker-only constraint — or
      accept the exception knowingly and write it down.
- [ ] Reconcile the data contract with UUIDv7 ids and the existing REST API.

## Current recommendation

**Do not start.** Not because the idea is wrong — the cost and latency
arguments hold up — but because it reopens settled non-goals, adds the
largest toolchain in the project, and its central benefit is still
unquantified. Measure the cheap PWA-side wins first — spec `20`'s barcode
recall chief among them: they may make the app unnecessary, and if they do
not, they leave a much smaller and better-defined app to build.
