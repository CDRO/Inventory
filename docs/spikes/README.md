# Spikes — candidate work, not yet accepted

This folder holds **ideas under evaluation**. Nothing here is a commitment, a
requirement, or an implementation contract.

## Instructions for implementing agents

**Do not implement anything in this folder.** The only implementation contract
is `docs/specs/`. A spike becomes work by being promoted into a numbered spec
under `docs/specs/` (the `12`–`49` range is reserved for exactly that) and then
into the issue queue — never by being read from here.

If a spike contradicts an accepted spec, the accepted spec wins until a human
decides otherwise. Contradictions are recorded in the entry on purpose; they
are the open questions, not oversights to fix on your own.

## Entry format

Each spike records what it is, why it is attractive, what it would cost, what
it conflicts with, and **the conditions that must be true before it is
accepted**. A spike with no gate conditions is not ready to be a spike.

---

# S-01 — Native Android app with on-device vision (Gemma)

**Status:** App deferred · **server-side contract accepted** · **Raised:** 2026-09-09
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

**Barcode stays here, deliberately.** It is not in the contract and not in the
core schema. If it is built, it arrives on this spike's branch together with
its `products` column and lookup endpoint — so that if the on-device experiment
fails, reverting the branch leaves a clean schema with no orphaned column and
no half-used concept. The barcode non-goal in `00-overview.md` therefore stands
until evidence justifies changing it, rather than being amended in advance.

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

1. **Barcode scanning is an explicit non-goal.** `00-overview.md` states that
   identification is "exclusively via vision-LLM photo analysis and
   text/semantic matching, not barcode databases." The draft makes barcode the
   mandatory first-checked path. Either that non-goal changes, or the spike
   loses its cheapest path — and much of its cost argument with it.
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
| Barcode scanning | No — `BarcodeDetector`, or a vendored decoder |

**This is the most useful observation in the spike.** If the cheap rows deliver
most of the cost and latency win, the expensive row may never be worth
building. Measure that before starting an Android project, not after.

## Gate conditions — all must hold before this is accepted

- [ ] Measure current cloud-vision spend and what share of real scans are
      genuinely single-item. If trivial scans are not the bulk of usage, stop.
- [ ] Deliver the non-native rows above in the PWA first, then measure what is
      left. The remaining saving is the actual budget for the app.
- [ ] Run the draft's Phase 0: Gemma E2B on real household photos against the
      cloud baseline, to establish the escalation rate on real data. An
      escalation rate much above a third invalidates the cost case.
- [ ] Decide explicitly, in `00-overview.md`, whether barcode scanning and
      offline editing stop being non-goals. Until that decision exists, the
      spike cannot be specified coherently.
- [ ] Confirm an Android build can coexist with the Docker-only constraint — or
      accept the exception knowingly and write it down.
- [ ] Reconcile the data contract with UUIDv7 ids and the existing REST API.

## Current recommendation

**Do not start.** Not because the idea is wrong — the cost and latency
arguments hold up — but because it reopens three settled non-goals, adds the
largest toolchain in the project, and its central benefit is still
unquantified. Measure the cheap PWA-side wins first: they may make the app
unnecessary, and if they do not, they leave a much smaller and better-defined
app to build.
