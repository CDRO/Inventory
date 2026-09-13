package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/consume"
	"github.com/CDRO/Inventory/internal/store"
)

// Confirm bounds. Mirrors maxIngestRows/maxIngestQuantity in ingest.go: far
// above a real photo's worth of items, far below anything that would make one
// confirm an expensive transaction.
const (
	maxConsumeRows             = 500
	maxConsumeDecrementsPerRow = 50
	maxConsumeQuantity         = 100_000
)

// Consumer starts consumption-photo ingestion (internal/consume).
type Consumer interface {
	Available(ctx context.Context) (model string, ok bool)
	Start(ctx context.Context, u consume.Upload) (*store.Job, error)
}

// ConsumeStore applies a reviewed consumption proposal.
type ConsumeStore interface {
	ConfirmConsumption(ctx context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []store.ConsumeDecision) (*store.ConsumeResult, error)
}

// ConsumeHandler serves the consumption-logging routes of
// docs/specs/09-consumption-logging.md.
type ConsumeHandler struct {
	consumer Consumer
	store    ConsumeStore
	errors   *ErrorWriter
}

// NewConsumeHandler wires the consumption-logging routes.
func NewConsumeHandler(c Consumer, s ConsumeStore, errs *ErrorWriter) *ConsumeHandler {
	return &ConsumeHandler{consumer: c, store: s, errors: errs}
}

// Upload serves POST /api/storages/{storage_id}/consume/photos.
//
// Unlike shelf and product ingestion there is no location hint: a
// consumption photo decrements batches that already have a location, so
// there is nothing here for one to disambiguate.
func (h *ConsumeHandler) Upload(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}

	if model, available := h.consumer.Available(r.Context()); !available {
		h.errors.WriteError(w, r, ModelUnavailable(model))
		return
	}

	// Metadata is stripped here, before the photo is ever written: the photo
	// on disk is already the clean one (docs/specs/04-backend-api-conventions.md).
	image, failure := ReadImageUpload(w, r, "image")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	job, err := h.consumer.Start(r.Context(), consume.Upload{
		StorageID: storageID,
		CreatedBy: user.ID,
		Filename:  image.Filename,
		Image:     image.Data,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "storage not found"))
		return
	}

	writeJSON(w, http.StatusAccepted, struct {
		JobID uuid.UUID `json:"job_id"`
	}{JobID: job.ID})
}

// consumeConfirmRequest is the body of POST .../consume/photos/{job_id}/confirm.
type consumeConfirmRequest struct {
	Items *[]consumeConfirmItem `json:"items"`
}

type consumeConfirmItem struct {
	RowID      string                    `json:"row_id"`
	Decision   string                    `json:"decision"`
	ProductID  *uuid.UUID                `json:"product_id"`
	Decrements []consumeConfirmDecrement `json:"decrements"`
}

type consumeConfirmDecrement struct {
	BatchID  uuid.UUID `json:"batch_id"`
	Quantity int       `json:"quantity"`
}

// Confirm serves POST /api/storages/{storage_id}/consume/photos/{job_id}/confirm.
//
// The body decides every proposed row, accept or reject. Nothing is written to
// inventory before this call, and everything it writes lands in one
// transaction with the job marked consumed
// (docs/specs/09-consumption-logging.md).
func (h *ConsumeHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Unauthorized(ReasonSessionMissing))
		return
	}
	jobID, failure := idFromPath(r, "job_id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body consumeConfirmRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	decisions, failure := parseConsumeDecisions(body)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	userID := user.ID
	result, err := h.store.ConfirmConsumption(r.Context(), storageID, jobID, &userID, decisions)
	switch {
	case errors.Is(err, store.ErrValidation):
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"items": {"Every proposed row needs exactly one decision; an accepted row needs a product and at least one valid batch decrement."},
		}, err))
		return
	case errors.Is(err, store.ErrConflict):
		h.errors.WriteError(w, r, conflictAs(err, "", "This proposal has already been confirmed, or is not ready for review."))
		return
	case err != nil:
		h.errors.WriteError(w, r, FromStoreError(err, "job or referenced id not found in storage"))
		return
	}

	writeJSON(w, http.StatusOK, struct {
		BatchIDs []uuid.UUID `json:"batch_ids"`
	}{result.BatchIDs})
}

// parseConsumeDecisions validates the body's shape. Whether the rows match
// the proposal, whether every id belongs to this storage, and whether a
// decrement fits its batch is the store's to decide under the job's lock.
func parseConsumeDecisions(body consumeConfirmRequest) ([]store.ConsumeDecision, *Failure) {
	if body.Items == nil {
		return nil, ValidationFailed(map[string][]string{"items": {"A decision for every proposed row is required."}}, nil)
	}
	if len(*body.Items) > maxConsumeRows {
		return nil, ValidationFailed(map[string][]string{"items": {"Too many rows."}}, nil)
	}

	fields := map[string][]string{}
	add := func(i int, field, msg string) {
		key := fmt.Sprintf("items[%d].%s", i, field)
		fields[key] = append(fields[key], msg)
	}

	out := make([]store.ConsumeDecision, 0, len(*body.Items))
	for i, item := range *body.Items {
		d := store.ConsumeDecision{RowID: item.RowID}
		if item.RowID == "" {
			add(i, "row_id", "Required.")
		}

		switch item.Decision {
		case "reject":
			out = append(out, d)
			continue
		case "accept":
			d.Accept = true
		default:
			add(i, "decision", "Must be accept or reject.")
			continue
		}

		if item.ProductID == nil {
			// Consumption never creates a product: an "unrecognized" row must
			// be corrected to an existing one, or rejected.
			add(i, "product_id", "Required.")
		} else {
			d.ProductID = item.ProductID
		}

		if len(item.Decrements) == 0 {
			add(i, "decrements", "At least one batch decrement is required.")
		} else if len(item.Decrements) > maxConsumeDecrementsPerRow {
			add(i, "decrements", "Too many batches.")
		}
		decrements := make([]store.ConsumeBatchDecrement, 0, len(item.Decrements))
		for _, dec := range item.Decrements {
			if dec.Quantity < 1 || dec.Quantity > maxConsumeQuantity {
				add(i, "decrements", "Each quantity must be a whole number of at least 1.")
				continue
			}
			decrements = append(decrements, store.ConsumeBatchDecrement{BatchID: dec.BatchID, Quantity: dec.Quantity})
		}
		d.Decrements = decrements

		out = append(out, d)
	}

	if len(fields) > 0 {
		return nil, ValidationFailed(fields, nil)
	}
	return out, nil
}
