package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// maxBatchQuantity bounds a manually corrected count, the same order of
// magnitude as maxIngestQuantity and maxConsumeQuantity: far above anything a
// household shelf holds, far below a number that would make one PATCH an
// expensive transaction.
const maxBatchQuantity = 100_000

// maxContainerLabelLength mirrors containers.label's VARCHAR(255)
// (migrations/00015_batch_containers.sql). Without the check a 256-character
// label reaches PostgreSQL and comes back as a 500 rather than the 422 it is.
//
// containers.container_type is TEXT and deliberately unbounded in the schema, so
// nothing bounds it here either beyond maxJSONBody, which caps every body on
// this API.
const maxContainerLabelLength = 255

// BatchStore is the slice of the store the batch handlers use.
type BatchStore interface {
	SplitBatch(ctx context.Context, storageID, batchID uuid.UUID, in store.SplitBatchInput, userID *uuid.UUID) (*store.Batch, error)
	UpdateBatch(ctx context.Context, storageID, batchID uuid.UUID, patch store.BatchPatch, userID *uuid.UUID) (*store.Batch, error)
	DestroyContainer(ctx context.Context, storageID, containerID uuid.UUID) error
}

// BatchHandler serves the batch operations in
// docs/specs/06-vision-shelf-ingestion.md: splitting a batch across two
// locations, and moving one whole.
//
// Both are mounted behind RequireStorageMember, and both resolve the batch and
// the target location against the storage in the request context. A batch id or
// a location id from another storage is therefore ErrNotFound in the store and
// a 404 on the wire, indistinguishable from an id that never existed.
//
// It also serves the container operations of
// docs/specs/39-batch-containers.md, which live here because a container is a
// batch attribute and not a resource of its own: it is set through the batch
// PATCH, disposed of through the split, and has exactly one endpoint addressing
// it directly — DestroyContainer, whose id comes straight from the URL and is
// therefore the one place in that spec needing a real runtime same-storage
// check rather than a structural one.
type BatchHandler struct {
	store  BatchStore
	errors *ErrorWriter
}

// NewBatchHandler wires the handlers to a store and the one error writer.
func NewBatchHandler(s BatchStore, errs *ErrorWriter) *BatchHandler {
	return &BatchHandler{store: s, errors: errs}
}

// batchResponse is one batch on the wire.
//
// expiration_source is included because it is the field a reviewer needs to
// see to trust a date: 'user' means a person typed it and the expiry cascade in
// docs/specs/08-expiration-and-classification.md must never overwrite it.
//
// container_id, container_label and container_type are all three serialized
// even when null (docs/specs/39-batch-containers.md): a batch's container is an
// optional field, and a client that has to tell "no container" from "this
// response does not carry containers" cannot do it from an absent key.
type batchResponse struct {
	ID               uuid.UUID  `json:"id"`
	ProductID        uuid.UUID  `json:"product_id"`
	LocationID       uuid.UUID  `json:"location_id"`
	Quantity         int        `json:"quantity"`
	ExpirationDate   *string    `json:"expiration_date"`
	ExpirationSource string     `json:"expiration_source"`
	CreatedAt        time.Time  `json:"created_at"`
	ContainerID      *uuid.UUID `json:"container_id"`
	ContainerLabel   *string    `json:"container_label"`
	ContainerType    *string    `json:"container_type"`
}

// Split serves POST /api/storages/{storage_id}/inventory-batches/{id}/split.
//
// Three jars in the cellar and one carried to the kitchen is two batches, not
// one batch in two places. Both halves keep the original expiry and its source,
// because they are the same jars.
//
// The optional container_disposition decides what happens to the source's
// container (docs/specs/39-batch-containers.md). It defaults to "source" — the
// 24-pack that now holds 22 is still the 24-pack — and is a no-op on a batch
// with no container whatever its value, so a client may always send it.
func (h *BatchHandler) Split(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	batchID, failure := batchIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		Quantity             int     `json:"quantity"`
		TargetLocationID     string  `json:"target_location_id"`
		ContainerDisposition *string `json:"container_disposition"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	fields := map[string][]string{}
	if body.Quantity < 1 {
		// The upper bound belongs to the store, which is the only thing holding
		// a lock on the row and therefore the only thing that knows the current
		// quantity. Checking the lower bound here just keeps an obviously
		// impossible request from reaching a transaction.
		fields["quantity"] = append(fields["quantity"], "Must be at least 1.")
	}
	targetID, err := uuid.Parse(body.TargetLocationID)
	if err != nil {
		fields["target_location_id"] = append(fields["target_location_id"], "Must be a UUID.")
	}

	// Checked here rather than in the store because it is a pure value check
	// needing no row: an unknown disposition never has to open a transaction to
	// be refused. The store validates it again for callers that do not come
	// through this handler.
	var disposition store.ContainerDisposition
	if body.ContainerDisposition != nil {
		disposition = store.ContainerDisposition(*body.ContainerDisposition)
		if !store.ValidContainerDisposition(disposition) {
			fields["container_disposition"] = append(fields["container_disposition"],
				`Must be "source", "target", "both", "neither" or "destroy".`)
		}
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	created, err := h.store.SplitBatch(r.Context(), storageID, batchID, store.SplitBatchInput{
		Quantity:             body.Quantity,
		TargetLocationID:     targetID,
		ContainerDisposition: disposition,
	}, actingUser(r))
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "batch or target location not in this storage or nonexistent"))
		return
	}

	writeJSON(w, http.StatusCreated, newBatchResponse(*created))
}

// Update serves PATCH /api/storages/{storage_id}/inventory-batches/{id}: a
// whole-batch move (docs/specs/06-vision-shelf-ingestion.md), a manual
// quantity correction (docs/specs/13-stocktake-and-audit.md), or both in one
// request — in which case the store applies them in one transaction, so there
// is no half-applied PATCH to detect afterwards.
//
// expiration_date and expiration_source stay on their own sub-route
// (.../expiry), which owns the derived-versus-user cascade of
// docs/specs/08-expiration-and-classification.md. Nothing is lost by them
// landing separately: a date writes no ledger row, so an expiry edit and a
// quantity correction have no shared state that could be left inconsistent.
//
// A quantity of 0 deletes the batch, which leaves nothing to return: that case
// answers 204 rather than a 200 carrying a row that no longer exists. Pairing
// that 0 with a location_id is the one combination this route refuses (422):
// the row is deleted either way, so "empty it" and "move it" contradict each
// other and applying them in either order gives a different ledger.
func (h *BatchHandler) Update(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	batchID, failure := batchIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	// container_label and container_type are RawMessage, not *string, because an
	// absent field and an explicit null mean different things here: absent is
	// "leave the container alone", null is "this batch is in nothing" (a detach
	// that keeps the container row, never a destroy). A plain pointer decodes
	// both to nil and silently turns the detach into a no-op. Same shape
	// products.go's Update uses for its own nullable fields.
	var body struct {
		LocationID     *string         `json:"location_id"`
		Quantity       *int            `json:"quantity"`
		ContainerLabel json.RawMessage `json:"container_label"`
		ContainerType  json.RawMessage `json:"container_type"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if body.LocationID == nil && body.Quantity == nil && body.ContainerLabel == nil && body.ContainerType == nil {
		// A PATCH naming no field is a request the server cannot carry out, and
		// answering 200 would tell the caller their change landed when nothing
		// changed. This is wrong with the request as a whole, not with any one
		// field, so it is one "body" entry rather than the same sentence repeated
		// under every field key — the same "body" key decodeJSON's own
		// malformed-body error uses, for the same reason.
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"body": {"Name at least one of location_id, quantity, container_label or container_type."},
		}, nil))
		return
	}

	fields := map[string][]string{}
	patch := store.BatchPatch{}

	if body.Quantity != nil {
		if *body.Quantity < 0 || *body.Quantity > maxBatchQuantity {
			fields["quantity"] = append(fields["quantity"], "Must be a whole number of zero or more.")
		} else {
			patch.Quantity = body.Quantity
		}
	}

	if body.LocationID != nil {
		targetID, err := uuid.Parse(*body.LocationID)
		if err != nil {
			fields["location_id"] = append(fields["location_id"], "Must be a UUID.")
		} else {
			patch.LocationID = &targetID
		}
	}

	if body.ContainerLabel != nil {
		patch.SetContainerLabel = true
		var raw *string
		if err := json.Unmarshal(body.ContainerLabel, &raw); err != nil {
			fields["container_label"] = append(fields["container_label"], "Must be a string or null.")
		} else if raw != nil {
			label := strings.TrimSpace(*raw)
			switch {
			case label == "":
				// Cleared by null, not by an empty string: two spellings of
				// "no container" would be two states a client has to reconcile,
				// the same reasoning icon_name uses in products.go.
				fields["container_label"] = append(fields["container_label"],
					"Send null to take this batch out of its container.")
			case utf8.RuneCountInString(label) > maxContainerLabelLength:
				fields["container_label"] = append(fields["container_label"],
					"Keep the container label under 255 characters.")
			default:
				patch.ContainerLabel = &label
			}
		}
	}

	if body.ContainerType != nil {
		patch.SetContainerType = true
		var raw *string
		if err := json.Unmarshal(body.ContainerType, &raw); err != nil {
			fields["container_type"] = append(fields["container_type"], "Must be a string or null.")
		} else if raw != nil {
			containerType := strings.TrimSpace(*raw)
			if containerType == "" {
				fields["container_type"] = append(fields["container_type"],
					"Send null to clear the container type.")
			}
			patch.ContainerType = &containerType
		}
	}

	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	updated, err := h.store.UpdateBatch(r.Context(), storageID, batchID, patch, actingUser(r))
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "batch or target location not in this storage or nonexistent"))
		return
	}

	if updated == nil {
		writeJSON(w, http.StatusNoContent, nil)
		return
	}

	writeJSON(w, http.StatusOK, newBatchResponse(*updated))
}

// DestroyContainer serves
// POST /api/storages/{storage_id}/containers/{id}/destroy — no body.
//
// It marks the container destroyed and clears it off every batch that referenced
// it, in one transaction (docs/specs/39-batch-containers.md, "Destroying a
// container"). 204: there is nothing left to describe, and the batches whose
// container_id it cleared are read back through their product, not from here.
//
// The three refusals are deliberately one answer. An unknown id, an id from
// another storage, and an already-destroyed id in this storage all come back as
// ErrNotFound and therefore the same 404 — never a 403, which would confirm the
// id names a real container somewhere, and never a 409, which would confirm it
// named one here. Destroying is not idempotent: a second call is "already gone",
// which is the same thing an id that never existed says.
//
// This is the one container operation whose id arrives straight from the URL,
// which is why the store's same-storage check is a runtime one rather than the
// structural argument that covers container_label.
func (h *BatchHandler) DestroyContainer(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	containerID, failure := containerIDFromPath(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.DestroyContainer(r.Context(), storageID, containerID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "container not in this storage, nonexistent, or already destroyed"))
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

// parseCreationContainerFields validates container_label/container_type on an
// endpoint that creates a batch — the stocktake CreateBatch and the ingest
// confirm step both take these two fields at creation time, same names and
// same upsert rule the PATCH above has
// (docs/specs/39-batch-containers.md, "Setting and clearing a container on a
// batch", closing paragraph).
//
// Unlike the PATCH's RawMessage handling, a batch being created has no
// existing container to detach from, so absent and explicit null are the same
// "no container" and a plain *string carries that with no ambiguity to lose.
// A container_type with no container_label is not rejected here — the store's
// own setBatchContainerType already refuses it with the same ErrValidation
// the PATCH's identical case reaches the wire as a 422 through, so the check
// only needs to exist once.
func parseCreationContainerFields(label, containerType *string, fields map[string][]string) (*string, *string) {
	var outLabel, outType *string
	if label != nil {
		trimmed := strings.TrimSpace(*label)
		switch {
		case trimmed == "":
			fields["container_label"] = append(fields["container_label"], "Must not be empty.")
		case utf8.RuneCountInString(trimmed) > maxContainerLabelLength:
			fields["container_label"] = append(fields["container_label"], "Keep the container label under 255 characters.")
		default:
			outLabel = &trimmed
		}
	}
	if containerType != nil {
		trimmed := strings.TrimSpace(*containerType)
		if trimmed == "" {
			fields["container_type"] = append(fields["container_type"], "Must not be empty.")
		} else {
			outType = &trimmed
		}
	}
	return outLabel, outType
}

// containerIDFromPath parses {id} on the container routes, answering 404 for
// anything unparseable — the same reasoning as batchIDFromPath, and the same
// answer a well-formed id from another storage gets.
func containerIDFromPath(r *http.Request) (uuid.UUID, *Failure) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, NotFound("malformed container id")
	}
	return id, nil
}

// batchIDFromPath parses {id}, answering 404 for anything unparseable — the
// same reasoning as locationIDFromPath.
func batchIDFromPath(r *http.Request) (uuid.UUID, *Failure) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, NotFound("malformed batch id")
	}
	return id, nil
}

// actingUser returns the id recorded on the ledger rows a request writes.
//
// inventory_logs.created_by is nullable and set to null when the user is
// deleted, so a nil here is a shape the column already allows rather than a
// special case. In practice these routes sit behind RequireSession and a user
// is always present.
func actingUser(r *http.Request) *uuid.UUID {
	user, ok := UserFrom(r.Context())
	if !ok {
		return nil
	}
	return &user.ID
}

func newBatchResponse(b store.Batch) batchResponse {
	out := batchResponse{
		ID:               b.ID,
		ProductID:        b.ProductID,
		LocationID:       b.LocationID,
		Quantity:         b.Quantity,
		ExpirationSource: string(b.ExpirationSource),
		CreatedAt:        b.CreatedAt,
		ContainerID:      b.ContainerID,
		ContainerLabel:   b.ContainerLabel,
		ContainerType:    b.ContainerType,
	}
	if b.ExpirationDate != nil {
		// A DATE column is a calendar day, not an instant. Serializing it as
		// RFC 3339 would attach a midnight-UTC time that a client in another
		// zone can render as the day before.
		day := b.ExpirationDate.Format(time.DateOnly)
		out.ExpirationDate = &day
	}
	return out
}
