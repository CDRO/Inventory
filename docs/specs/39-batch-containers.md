# 39 — Batch Containers

Depends on: [`02-data-model.md`](02-data-model.md) (`inventory_batches`),
[`06-vision-shelf-ingestion.md`](06-vision-shelf-ingestion.md) ("One batch,
one location — and how to split one" — the split/move endpoints this spec
extends), [`16-product-maintenance.md`](16-product-maintenance.md) (the
batch list on `products.html` this spec adds a field to).

## Why this spec exists

A batch already has an exact quantity and an exact location
(`02-data-model.md`), but nothing records what it is physically *held
in*. A 24-pack of beer, a bag of rice, a cooler bag someone packed for a
trip — these are real objects a person reasons about (`is this box
empty yet?`, `how many are left in the pack?`), and splitting two beers
out into the fridge today only ever answers "where," never "the pack now
has 22 left" or "I threw the empty pack away."

A container is **not** a location. A location is where a batch sits; a
container is what it sits *in*, and the two are independent — a batch's
container, if it has one, travels with whichever location the batch is
currently at. Introducing a second location-like hierarchy (a container
that itself has a location and can hold several batches) was considered
and rejected: the actual want is narrower — one batch, one optional
container, whose remaining count *is* the batch's own quantity, nothing
more.

**Several identical containers of the same product need nothing extra.**
Buying four 24-packs of beer for a party is already four independent
`inventory_batches` rows today — `02-data-model.md`'s existing model
never limited one product to one batch (the cucumber-jars-in-fridge-*and*-
cellar case is the same mechanism). Each pack can carry its own
container (`"Pack 1"`, `"Pack 2"`, …, or left unlabeled — nothing
requires naming every one), fully independent of the other three:
splitting two beers out of pack 1 never touches packs 2-4's quantities
or containers. There is no "batch of batches" concept, and this spec
does not add one — four packs are simply four rows, exactly as four
distinct purchases already are.

## Schema

Primary keys are UUIDv7, per `02-data-model.md`.

```sql
CREATE TABLE containers (
    id           UUID PRIMARY KEY,
    storage_id   UUID NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
    label        VARCHAR(255) NOT NULL,
    container_type TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    destroyed_at TIMESTAMPTZ
);

CREATE INDEX idx_containers_storage_id ON containers(storage_id);
```

```sql
ALTER TABLE inventory_batches
    ADD COLUMN container_id UUID REFERENCES containers(id) ON DELETE SET NULL;

CREATE INDEX idx_inventory_batches_container_id
    ON inventory_batches(container_id) WHERE container_id IS NOT NULL;
```

- `label` is free text ("24-pack box", "rice bag") — there is no catalog
  of container types, matching this project's general preference for
  typed-by-a-person text over a taxonomy nobody asked for.
- `container_type` is optional, free text, purely descriptive (e.g. "box",
  "bag") — not validated against an enum, and not currently used for any
  behavior. It exists so a future UI can group or icon containers by kind
  without a schema change; if unused a year from now, deleting the column
  is a two-line migration, not a design regret.
- **A container's current count is its batch's `quantity`.** There is no
  separate count on `containers` — that would be a second number that can
  drift from the truth. A container with no batch referencing it anymore
  (the batch hit `quantity = 0` and was deleted, per `02-data-model.md`'s
  existing rule) simply has no current count; its row is **deliberately
  left in place** (`destroyed_at` still `NULL`) as a harmless orphan, not
  auto-destroyed — emptying a container by consuming everything in it is
  not the same statement as "I threw the box away," and the distinction
  matters for anyone later asking "what happened to this container" via
  `inventory_logs`/its own audit trail below. Nothing in this spec sweeps
  or garbage-collects an orphaned container row; it stays queryable and
  `destroyed_at IS NULL` forever unless someone explicitly destroys it.
- `ON DELETE SET NULL` on `inventory_batches.container_id`: a container is
  never a required field, and nothing about deleting one (there is no
  delete endpoint, only destroy — below) should be able to fail or cascade
  into batch data.
- No `storage_id` cross-check lives in the FK; same-storage validation is
  application code's job, per every other cross-storage reference in this
  system (`CLAUDE.md`).

## Setting and clearing a container on a batch

`PATCH /api/storages/{storage_id}/inventory-batches/{id}` (already defined
by `06`) gains two more optional fields, alongside `location_id`:

- `container_label` — a non-empty string. If the batch has no container
  yet, creates one (`label = container_label`) and attaches it. If the
  batch already has a container, **renames that container** (the same row,
  same id) rather than creating a second one — a label is metadata about
  the physical object, not a new container each time someone fixes a typo.
- `container_label: null` — detaches the batch from its container without
  destroying the container row. This is for "I mislabeled this batch,"
  not for "the box is gone" (that is `destroy`, below); the container
  keeps existing, just referenced by nothing, in case another batch should
  have pointed at it instead.
- `container_type` — same upsert/rename semantics as `container_label`,
  and requires an existing or simultaneously-created container (sending
  `container_type` with no `container_label` and no existing container on
  the batch is `422`).

A batch can also be given a container at creation time, e.g. through
`16-product-maintenance.md`'s standalone add-product flow or the vision
review screen's confirm step (`06`) — same field names, same upsert rule,
on whichever endpoint creates the batch.

## Destroying a container

`POST /api/storages/{storage_id}/containers/{id}/destroy` — no body.
Unlike `container_label`'s rename path (which never takes a container id
directly — see "Acceptance criteria"), this endpoint's `{id}` *is* a
container id straight from the URL, so it is the one place in this spec
that needs a real runtime same-storage check, not a structural one.

1. `404` if the container does not belong to this storage (an id from a
   different storage gets the identical `404` an unknown id gets — no
   `403`, per the project's non-enumeration rule) or is already destroyed
   (destroying is not idempotent — a second call on an already-destroyed
   container is a `404`, the same "already gone" answer an unknown id
   gets).
2. In one transaction: set `destroyed_at = now()`, and clear
   `container_id` to `NULL` on every batch currently referencing it.
3. No `inventory_logs` row — nothing about quantity changed, only which
   physical object a batch is (was) held in. `containers.destroyed_at` is
   itself the audit trail for "when was this container destroyed."

A destroyed container's row is kept forever (never deleted), so a batch's
history can still say "this used to be in the 24-pack box, destroyed
2026-10-03" if that is ever surfaced — nothing in this spec requires
surfacing it, but nothing should make it impossible either.

## Splitting a container-holding batch

`POST /api/storages/{storage_id}/inventory-batches/{id}/split` (`06`)
gains one more optional field: `container_disposition`, one of
`"source"` (default), `"target"`, `"both"`, `"neither"`, or
`"destroy"`. A value outside this set is `422`, the same treatment every
other invalid-field-value case in this system already gets — this
endpoint does not introduce a new error shape.

- The source batch (what stays behind after the split) and the new target
  batch (what the split creates) each either keep the source's
  `container_id`, get `NULL`, or the source's container is destroyed
  outright (clearing it from both) — exactly one of five shapes, chosen by
  `container_disposition`:

  | `container_disposition` | Source batch's `container_id` after | Target batch's `container_id` |
  |---|---|---|
  | `source` (default) | unchanged | `NULL` |
  | `target` | `NULL` | the source's original container |
  | `both` | unchanged | the source's original container |
  | `neither` | `NULL` | `NULL` |
  | `destroy` | `NULL` (container destroyed) | `NULL` (container destroyed) |

- `container_disposition` on a source batch with no container is a no-op
  regardless of value (nothing to move or destroy) — never an error; a
  frontend that always sends the field for symmetry should not have to
  special-case container-less batches.
- `destroy` runs the same destroy semantics as the standalone endpoint
  above (`destroyed_at`, cleared on every referencing batch — which, at
  the moment of a split, is only ever the source, since the target batch
  is being created fresh in the same statement), in the same transaction
  as the split itself: a split-and-destroy is one atomic operation, not a
  split followed by a second call a client could fail to make.
- The default (`source`) is deliberately the common case the spec's own
  example describes: two beers split into the fridge, the 24-pack (now
  holding 22) is still the 24-pack; the two beers in the fridge are not
  "a container" unless the person explicitly says so with
  `container_disposition: "target"` or `"both"`.
- `PATCH .../inventory-batches/{id}` (whole-batch move, `06`) is
  unaffected — moving an entire batch to a new location does not touch
  its container, since the object physically travels with it either way.

## Product detail UI

`products.html`'s batch list (`16-product-maintenance.md`, "The product
edit surface") gains a container column/line per batch: the container's
label if set (e.g. "24-pack box"), or nothing if unset. A batch with a
container gets:

- An inline rename/clear affordance for `container_label`
  /`container_type`, calling the `PATCH` fields above.
- A "destroy container" action, calling the `destroy` endpoint, with a
  confirmation step (destroying is not idempotent, and clears the
  reference off every batch that had it — a batch-list surface should say
  which batches, if more than the one currently in view).

The split form (`06`, extended by
`28-batch-move-quick-create.md`'s quick-create trigger) gains a
`container_disposition` control, shown only when the source batch has a
container: a small choice between "keep with the remaining stock"
(`source`, pre-selected), "move with the split-out amount" (`target`),
"both" (`both`), "neither" (`neither`), and "destroy the container"
(`destroy`) — plain radio buttons, not a dropdown, since there are only
five and the person is mid-task.

## Acceptance criteria

- A batch's container is a `container_id` FK, never a quantity of its
  own — a container's current count is read from the batch, never
  written to the container.
- Setting `container_label` on a batch that already has a container
  renames the existing container row (same `id`); it does not create a
  second one.
- `container_label: null` detaches without destroying; the container row
  survives, referenced by nothing.
- Sending `container_type` with no `container_label` and no existing
  container already on the batch is `422` — there is nothing to attach
  the type to.
- `POST .../containers/{id}/destroy` sets `destroyed_at`, clears
  `container_id` on every batch that referenced it, in one transaction,
  is `404` on a container from a **different storage** (identical to the
  `404` an unknown id gets — never `403`), and is `404` on a
  **same-storage but already-destroyed** container too.
- A `container_disposition` value outside the five listed is `422`.
- Splitting a batch that has a container respects `container_disposition`
  exactly per the table above, defaulting to `source` when the field is
  omitted, and is a no-op on container fields when the source batch has
  no container regardless of the value sent.
- `container_disposition: "destroy"` on a split destroys the container in
  the same transaction as the split — no window where the split has
  happened but the container has not yet been destroyed, or vice versa.
- A batch's container becomes orphaned (referenced by nothing) when its
  last batch is consumed to `quantity = 0` and deleted — the container
  row is not destroyed or removed by that, and stays queryable exactly as
  before.
- Same-storage validation: `container_label`'s rename path never crosses
  storages structurally — there is no way to pass a container id
  directly through it, only a label to upsert against the batch's *own*
  container, and the batch itself is already storage-scoped by its own
  id in the URL. The **destroy** endpoint is different: its `{id}` *is* a
  container id taken directly from the URL, so it is checked against the
  URL's `storage_id` at runtime like any other directly-addressed
  resource — this is the one container operation that needs, and has, a
  real runtime check rather than relying on the structural argument
  above.
- E2E: `e2e/specs/products.spec.js` (or a new file) covers labeling a
  batch as a container, renaming it, splitting it with each
  `container_disposition` value, and destroying it — including the
  "22 left in the 24-pack, 2 in the fridge with no container" shape this
  spec exists for.
