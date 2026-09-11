package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// ExpiryStore is the slice of the store the expiry handlers use.
type ExpiryStore interface {
	SetBatchExpiration(ctx context.Context, storageID, batchID uuid.UUID, date *time.Time) (*store.Batch, error)
	ResetBatchExpirationToDerived(ctx context.Context, storageID, batchID uuid.UUID) (*store.Batch, error)
	SetCategoryShelfLife(ctx context.Context, storageID, id uuid.UUID, days *int) error
	RecomputeDerivedExpiryForCategory(ctx context.Context, storageID, categoryID uuid.UUID) (int, error)
}

// ExpiryHandler serves the expiry surface of
// docs/specs/08-expiration-and-classification.md.
type ExpiryHandler struct {
	store  ExpiryStore
	errors *ErrorWriter
}

// NewExpiryHandler wires the handlers to a store and the one error writer.
func NewExpiryHandler(s ExpiryStore, errs *ErrorWriter) *ExpiryHandler {
	return &ExpiryHandler{store: s, errors: errs}
}

// PatchBatchExpiry serves PATCH
// /api/storages/{storage_id}/inventory-batches/{id}/expiry.
//
// Two shapes, and the difference between them is the whole feature:
//
//	{"expiration_date": "2026-03-01"}  — a person set this date
//	{"expiration_date": null}          — a person says this has no expiry
//	{"expiration_source": "derived"}   — put it back under the rules
//
// The first two both record expiration_source = 'user', because clearing a
// date is as much a decision as setting one, and a NULL date with a 'user'
// source is the sticky statement no later cascade may undo.
//
// The third exists because without a way back, one accidental edit would opt a
// batch out of every future rule change permanently — the spec calls it
// required rather than optional for exactly that reason.
func (h *ExpiryHandler) PatchBatchExpiry(w http.ResponseWriter, r *http.Request) {
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
		// RawMessage so an absent field is distinguishable from an explicit
		// null: omitting the date means "leave it", sending null means "this
		// has no expiry", and collapsing the two would make the deliberate
		// statement unexpressible.
		ExpirationDate   json.RawMessage `json:"expiration_date"`
		ExpirationSource *string         `json:"expiration_source"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	// The reset is checked first: a request asking for the automatic date back
	// has nothing to say about a specific date.
	if body.ExpirationSource != nil {
		if *body.ExpirationSource != string(store.ExpirationDerived) {
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"expiration_source": {`Only "derived" can be requested; a date you set is recorded as "user" automatically.`},
			}, nil))
			return
		}

		batch, err := h.store.ResetBatchExpirationToDerived(r.Context(), storageID, batchID)
		if err != nil {
			h.errors.WriteError(w, r, FromStoreError(err, "batch not in this storage or nonexistent"))
			return
		}
		writeJSON(w, http.StatusOK, newBatchResponse(*batch))
		return
	}

	if body.ExpirationDate == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"expiration_date": {`Provide a date, null to record "no expiry", or {"expiration_source":"derived"} to use the automatic date again.`},
		}, nil))
		return
	}

	date, failure := parseExpirationDate(body.ExpirationDate)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	batch, err := h.store.SetBatchExpiration(r.Context(), storageID, batchID, date)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "batch not in this storage or nonexistent"))
		return
	}

	writeJSON(w, http.StatusOK, newBatchResponse(*batch))
}

// parseExpirationDate reads the calendar-day form, or an explicit null.
//
// A DATE column is a day, not an instant. Accepting a timestamp here would let
// a client's timezone decide which day a jar expires on, so only YYYY-MM-DD is
// taken.
func parseExpirationDate(raw json.RawMessage) (*time.Time, *Failure) {
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, ValidationFailed(map[string][]string{
			"expiration_date": {"Must be a YYYY-MM-DD date or null."},
		}, err)
	}
	if text == nil {
		// An explicit null: "this has no expiry", recorded as a user decision.
		return nil, nil
	}

	parsed, err := time.Parse(time.DateOnly, *text)
	if err != nil {
		return nil, ValidationFailed(map[string][]string{
			"expiration_date": {"Must be a YYYY-MM-DD date or null."},
		}, err)
	}
	return &parsed, nil
}

// PatchCategoryShelfLife serves PATCH
// /api/storages/{storage_id}/categories/{id}/shelf-life.
//
// Changing a rule is a correction — it means the old number was wrong — so it
// applies to batches already in the database, not only to future ones. The
// cascade runs before the response so the count returned is the real one, and
// so a user who changes a rule and immediately looks at their inventory sees
// the new dates rather than the old ones.
//
// The spec asks for the *admin catalog* cascade to run as a background job.
// This is the storage-scoped version, which touches one household's rows
// rather than every storage's, and is small enough to do inline. The
// cross-storage catalog cascade waits for the job runner (#28).
func (h *ExpiryHandler) PatchCategoryShelfLife(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	categoryID, failure := idFromPath(r, "id", "malformed category id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		DefaultShelfLifeDays json.RawMessage `json:"default_shelf_life_days"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	if body.DefaultShelfLifeDays == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"default_shelf_life_days": {"Provide a number of days, or null to inherit from the parent category."},
		}, nil))
		return
	}

	days, failure := parseShelfLifeDays(body.DefaultShelfLifeDays)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.SetCategoryShelfLife(r.Context(), storageID, categoryID, days); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "category not in this storage or nonexistent"))
		return
	}

	affected, err := h.store.RecomputeDerivedExpiryForCategory(r.Context(), storageID, categoryID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusOK, struct {
		DefaultShelfLifeDays *int `json:"default_shelf_life_days"`
		RecomputedBatches    int  `json:"recomputed_batches"`
	}{DefaultShelfLifeDays: days, RecomputedBatches: affected})
}

// maxShelfLifeDays bounds a rule.
//
// A hundred years is past any food's shelf life and past the point where the
// number is a typo rather than an intention. The cap keeps a slip from
// producing a date the urgency colouring will treat as "fine" forever.
const maxShelfLifeDays = 36500

// parseShelfLifeDays reads a day count or an explicit null.
//
// Null means "inherit from the parent", which is a different statement from
// zero — zero would mean "expires the day it arrives".
func parseShelfLifeDays(raw json.RawMessage) (*int, *Failure) {
	var days *int
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, ValidationFailed(map[string][]string{
			"default_shelf_life_days": {"Must be a whole number of days, or null."},
		}, err)
	}
	if days == nil {
		return nil, nil
	}
	if *days < 0 {
		return nil, ValidationFailed(map[string][]string{
			"default_shelf_life_days": {"Cannot be negative."},
		}, nil)
	}
	if *days > maxShelfLifeDays {
		return nil, ValidationFailed(map[string][]string{
			"default_shelf_life_days": {"That is longer than any shelf life; check the number."},
		}, nil)
	}
	return days, nil
}
