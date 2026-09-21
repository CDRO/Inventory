package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// maxBatchQuantity bounds a manually corrected count, the same order of
// magnitude as maxIngestQuantity and maxConsumeQuantity: far above anything a
// household shelf holds, far below a number that would make one PATCH an
// expensive transaction.
const maxBatchQuantity = 100_000

// BatchStore is the slice of the store the batch handlers use.
type BatchStore interface {
	SplitBatch(ctx context.Context, storageID, batchID uuid.UUID, quantity int, targetLocationID uuid.UUID, userID *uuid.UUID) (*store.Batch, error)
	UpdateBatch(ctx context.Context, storageID, batchID uuid.UUID, patch store.BatchPatch, userID *uuid.UUID) (*store.Batch, error)
}

// BatchHandler serves the batch operations in
// docs/specs/06-vision-shelf-ingestion.md: splitting a batch across two
// locations, and moving one whole.
//
// Both are mounted behind RequireStorageMember, and both resolve the batch and
// the target location against the storage in the request context. A batch id or
// a location id from another storage is therefore ErrNotFound in the store and
// a 404 on the wire, indistinguishable from an id that never existed.
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
type batchResponse struct {
	ID               uuid.UUID `json:"id"`
	ProductID        uuid.UUID `json:"product_id"`
	LocationID       uuid.UUID `json:"location_id"`
	Quantity         int       `json:"quantity"`
	ExpirationDate   *string   `json:"expiration_date"`
	ExpirationSource string    `json:"expiration_source"`
	CreatedAt        time.Time `json:"created_at"`
}

// Split serves POST /api/storages/{storage_id}/inventory-batches/{id}/split.
//
// Three jars in the cellar and one carried to the kitchen is two batches, not
// one batch in two places. Both halves keep the original expiry and its source,
// because they are the same jars.
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
		Quantity         int    `json:"quantity"`
		TargetLocationID string `json:"target_location_id"`
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
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	created, err := h.store.SplitBatch(r.Context(), storageID, batchID, body.Quantity, targetID, actingUser(r))
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
// answers 204 rather than a 200 carrying a row that no longer exists.
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

	var body struct {
		LocationID *string `json:"location_id"`
		Quantity   *int    `json:"quantity"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if body.LocationID == nil && body.Quantity == nil {
		// A PATCH naming no field is a request the server cannot carry out, and
		// answering 200 would tell the caller their change landed when nothing
		// changed.
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"location_id": {"A location_id, a quantity, or both are required."},
			"quantity":    {"A location_id, a quantity, or both are required."},
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
