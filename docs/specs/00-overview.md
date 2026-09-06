# 00 — System Overview

## Audience and purpose of this document set

`docs/specs/` is the complete, authoritative specification for an AI-powered
spatial home inventory system. It is written for an **agentic coding AI** to
implement directly. Read the files in filename order (`00-`, `01-`, `02-`,
…) — each file assumes everything defined in the files before it and will
cross-reference them by filename. Do not read `docs/explanations/` for
implementation guidance; see that folder's `README.md` for why.

If any spec is ambiguous or internally inconsistent, stop and surface the
conflict rather than guessing — do not silently pick an interpretation for a
functional requirement (data shapes, API contracts, business rules).
Formatting/naming micro-decisions not covered by a spec may be made using
ordinary engineering judgment and the conventions already established
elsewhere in these specs.

## What this system is

A **self-hosted, full-stack household inventory application**. Users
photograph physical storage spaces (shelves, cupboards, boxes); a vision LLM
identifies items, quantities, and their position within a 3D location
hierarchy. The system tracks stock levels, expiration dates, minimum-stock
thresholds, and reconciles shopping lists against existing inventory. It
runs entirely in Docker containers, self-hosted (target: a home NAS), and
requires **no language runtime or toolchain on the operator's machine** —
only Docker. The backend is a single static Go binary; the frontend is
vanilla JavaScript with no build step — see
[`01-architecture-and-deployment.md`](01-architecture-and-deployment.md).

## Goals

- Let a user capture a photo of a shelf/cupboard and get a reviewable,
  editable proposal of what's on it, in 3D-located detail.
- Track inventory quantity, location, and expiration per physical batch of
  a product, not just a single running total per product.
- Reconcile shopping lists (text or photo) against existing inventory and
  help resolve new/ambiguous items with visual suggestions.
- Support multiple independent households ("storages"), each with its own
  users, locations, and inventory, administered by a single admin.
- Run entirely from Docker Compose, with external AI services (vision LLM,
  image search) as the only non-self-hosted dependencies.

## Non-goals

- Not a multi-tenant SaaS product with public sign-up, billing, or
  per-tenant role hierarchies. It is a household tool: one admin, and
  otherwise flat per-storage membership (see
  [`03-auth-and-multi-tenancy.md`](03-auth-and-multi-tenancy.md)).
- Not a barcode/UPC-scanning system. Item identification is exclusively via
  vision-LLM photo analysis and text/semantic matching, not barcode
  databases.
- Not a recipe planner, meal planner, or budgeting tool. Inventory tracking
  and reordering only.
- No offline-first / conflict-resolution sync engine. The PWA capability
  described in [`05-frontend-pwa-foundations.md`](05-frontend-pwa-foundations.md)
  is for installability and camera access, not offline data editing.

## Glossary

| Term | Meaning |
|---|---|
| **Storage** | An independent household/inventory tenant (e.g. "Smith Family Home"). All locations, products, and inventory belong to exactly one storage. Users are granted access to one or more storages. See `02-data-model.md`. |
| **User** | A person who can log in. Created only by the admin — no self-registration. See `03-auth-and-multi-tenancy.md`. |
| **Admin** | The single operator account (or small set of accounts) with access to the admin view: create users, create storages, grant/revoke storage access. Not a per-storage role — see `03-auth-and-multi-tenancy.md`. |
| **Location** | A node in a storage's 3D spatial hierarchy tree, e.g. `Basement → Right Shelf → Layer 2 → Front-Right`. See `02-data-model.md`. |
| **Category** | A node in a storage's category tree, e.g. `Food → Dairy → Cheese`. Categories carry the default shelf-life rules used for expiry estimation. See `02-data-model.md` and `08-expiration-and-classification.md`. |
| **Product** | A distinct item type (e.g. "Barilla Penne 500g"), independent of how many units exist or where they are. |
| **Catalog product** | An entry in the global, storage-agnostic product catalog: a reusable description of what a product *is* (name, category path, type, image, shelf life), with no owner, quantity, or storage reference. Lets a newly added product reuse known data instead of re-running vision/image-search APIs. See `02-data-model.md`. |
| **Inventory batch** | A concrete quantity of a product sitting at a specific location, with its own expiration date. A product can have many batches across many locations. |
| **Inventory log** | An immutable append-only record of a quantity change (purchase, consumption, audit correction) for a product. Powers reporting. |
| **Vision ingestion** | The flow of uploading a shelf photo and getting back a structured, reviewable proposal of items/quantities/locations. See `06-vision-shelf-ingestion.md`. |

## Feature map

| # | Feature (from original PRD) | Spec file |
|---|---|---|
| — | Foundations: architecture, deployment, data model, auth | `01`, `02`, `03`, `04`, `05` |
| 1 | Spatial 3D location hierarchy | `02-data-model.md` (schema) + `06-vision-shelf-ingestion.md` (UI) |
| 2 | High-resolution shelf photo ingestion | `06-vision-shelf-ingestion.md` |
| 3 | Smart shopping list reconciliation | `07-shopping-list-reconciliation.md` |
| 4 | Expiration & item type classification | `08-expiration-and-classification.md` |
| 5 | Quick consumption logging | `09-consumption-logging.md` |
| 6 | Reorder & minimum stock management | `10-reorder-and-shopping-export.md` |
| 7 | Reporting & analytics | `11-reporting-and-analytics.md` |

## Scale expectations

Household scale: a handful of storages, a handful of users per storage,
low thousands of products/batches per storage, infrequent concurrent
writes. Do not over-engineer for high concurrency, horizontal scaling, or
multi-region deployment — optimize for correctness and simplicity over
throughput.
