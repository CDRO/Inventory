package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/ingest"
	"github.com/CDRO/Inventory/internal/store"
)

// Confirm bounds. maxIngestRows is far above a real shelf and far below
// anything that would make one confirm an expensive transaction; the proposal
// the rows must match is bounded by what the model returned anyway.
const (
	maxIngestRows     = 500
	maxIngestQuantity = 100_000
	maxProductName    = 255
	maxLocationDepth  = 16
)

// Ingester starts photo ingestion (internal/ingest).
type Ingester interface {
	Available(ctx context.Context) (model string, ok bool)
	Start(ctx context.Context, u ingest.Upload) (*store.Job, error)
}

// IngestStore applies a reviewed proposal.
type IngestStore interface {
	ConfirmIngestion(ctx context.Context, storageID, jobID uuid.UUID, userID *uuid.UUID, decisions []store.IngestDecision) (*store.IngestResult, error)
}

// IngestHandler serves the ingestion routes of
// docs/specs/06-vision-shelf-ingestion.md.
type IngestHandler struct {
	ingester Ingester
	store    IngestStore
	errors   *ErrorWriter
}

// NewIngestHandler wires the ingestion routes.
func NewIngestHandler(i Ingester, s IngestStore, errs *ErrorWriter) *IngestHandler {
	return &IngestHandler{ingester: i, store: s, errors: errs}
}

// ShelfPhoto serves POST /api/storages/{storage_id}/ingest/shelf-photos.
func (h *IngestHandler) ShelfPhoto(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, store.JobShelfIngestion)
}

// ProductPhoto serves POST /api/storages/{storage_id}/ingest/product-photos.
func (h *IngestHandler) ProductPhoto(w http.ResponseWriter, r *http.Request) {
	h.upload(w, r, store.JobProductPhoto)
}

// upload accepts one photo and answers 202 with its job id at once.
//
// The model check comes before the body is read: a deployment whose model has
// been withdrawn says so immediately, as a configuration problem for the
// admin, instead of taking the user's 20MB photo and failing it later
// (docs/specs/01-architecture-and-deployment.md).
func (h *IngestHandler) upload(w http.ResponseWriter, r *http.Request, kind store.JobKind) {
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

	if model, available := h.ingester.Available(r.Context()); !available {
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

	var hint *uuid.UUID
	if raw := strings.TrimSpace(r.FormValue("location_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			// A malformed id names nothing, and is refused the same way as a
			// well-formed one from another storage.
			h.errors.WriteError(w, r, NotFound("malformed location hint"))
			return
		}
		hint = &id
	}

	job, err := h.ingester.Start(r.Context(), ingest.Upload{
		StorageID:      storageID,
		Kind:           kind,
		CreatedBy:      user.ID,
		LocationHintID: hint,
		Filename:       image.Filename,
		Image:          image.Data,
	})
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "location hint not found in storage"))
		return
	}

	writeJSON(w, http.StatusAccepted, struct {
		JobID uuid.UUID `json:"job_id"`
	}{JobID: job.ID})
}

// confirmRequest is the body of POST .../ingest/{job_id}/confirm.
type confirmRequest struct {
	Items *[]confirmItem `json:"items"`
}

type confirmItem struct {
	RowID    string `json:"row_id"`
	Decision string `json:"decision"`

	ProductID  *uuid.UUID `json:"product_id"`
	NewProduct *struct {
		Name       string     `json:"name"`
		CategoryID *uuid.UUID `json:"category_id"`
		ItemType   string     `json:"item_type"`
	} `json:"new_product"`

	Quantity int `json:"quantity"`

	LocationID *uuid.UUID `json:"location_id"`
	// NewLocation creates nodes the proposal marked as proposed: Names, root to
	// leaf, below ParentID (null for the storage's top level). Existing nodes
	// with the same name are reused.
	NewLocation *struct {
		ParentID *uuid.UUID `json:"parent_id"`
		Names    []string   `json:"names"`
	} `json:"new_location"`

	// ExpirationDate distinguishes absent from null. Absent accepts the
	// default and the batch gets a derived date; present — a date, or null for
	// "does not expire" — is the reviewer's own and recorded as a user date.
	ExpirationDate json.RawMessage `json:"expiration_date"`
}

// Confirm serves POST /api/storages/{storage_id}/ingest/{job_id}/confirm.
//
// The body decides every proposed row, accept or reject. Nothing is written to
// inventory before this call, and everything it writes lands in one
// transaction with the job marked consumed (docs/specs/06-vision-shelf-ingestion.md).
func (h *IngestHandler) Confirm(w http.ResponseWriter, r *http.Request) {
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

	var body confirmRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	decisions, failure := parseDecisions(body)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	userID := user.ID
	result, err := h.store.ConfirmIngestion(r.Context(), storageID, jobID, &userID, decisions)
	switch {
	case errors.Is(err, store.ErrValidation):
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			"items": {"Every proposed row needs exactly one decision, and only proposed rows may be decided."},
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
		BatchIDs         []uuid.UUID `json:"batch_ids"`
		ProductsCreated  int         `json:"products_created"`
		LocationsCreated int         `json:"locations_created"`
	}{result.BatchIDs, result.ProductsCreated, result.LocationsCreated})
}

// parseDecisions validates the body's shape. Whether the rows match the
// proposal, and whether every id belongs to this storage, is the store's to
// decide under the job's lock.
func parseDecisions(body confirmRequest) ([]store.IngestDecision, *Failure) {
	if body.Items == nil {
		return nil, ValidationFailed(map[string][]string{"items": {"A decision for every proposed row is required."}}, nil)
	}
	if len(*body.Items) > maxIngestRows {
		return nil, ValidationFailed(map[string][]string{"items": {"Too many rows."}}, nil)
	}

	fields := map[string][]string{}
	add := func(i int, field, msg string) {
		key := fmt.Sprintf("items[%d].%s", i, field)
		fields[key] = append(fields[key], msg)
	}

	out := make([]store.IngestDecision, 0, len(*body.Items))
	for i, item := range *body.Items {
		d := store.IngestDecision{RowID: item.RowID}
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

		switch {
		case (item.ProductID == nil) == (item.NewProduct == nil):
			add(i, "product_id", "Give exactly one of product_id and new_product.")
		case item.ProductID != nil:
			d.ProductID = item.ProductID
		default:
			name := strings.TrimSpace(item.NewProduct.Name)
			itemType := store.ItemType(item.NewProduct.ItemType)
			switch {
			case name == "":
				add(i, "new_product.name", "A name is required.")
			case utf8.RuneCountInString(name) > maxProductName:
				add(i, "new_product.name", "Must be at most 255 characters.")
			}
			switch itemType {
			case "":
				itemType = store.ItemLongShelfLife
			case store.ItemPerishable, store.ItemLongShelfLife, store.ItemNonPerishable:
			default:
				add(i, "new_product.item_type", "Must be perishable, long_shelf_life or non_perishable.")
			}
			d.NewProduct = &store.NewIngestProduct{Name: name, CategoryID: item.NewProduct.CategoryID, ItemType: itemType}
		}

		if item.Quantity < 1 || item.Quantity > maxIngestQuantity {
			add(i, "quantity", "Must be a whole number of at least 1.")
		}
		d.Quantity = item.Quantity

		switch {
		case (item.LocationID == nil) == (item.NewLocation == nil):
			add(i, "location_id", "Give exactly one of location_id and new_location.")
		case item.LocationID != nil:
			d.LocationID = item.LocationID
		default:
			names := make([]string, 0, len(item.NewLocation.Names))
			for _, n := range item.NewLocation.Names {
				n = strings.TrimSpace(n)
				if n == "" || utf8.RuneCountInString(n) > maxLocationName {
					add(i, "new_location.names", "Each name must be 1 to 255 characters.")
					break
				}
				names = append(names, n)
			}
			if len(item.NewLocation.Names) == 0 || len(item.NewLocation.Names) > maxLocationDepth {
				add(i, "new_location.names", "Give between 1 and 16 names.")
			}
			d.NewLocation = &store.NewIngestLocation{ParentID: item.NewLocation.ParentID, Names: names}
		}

		if len(item.ExpirationDate) > 0 {
			date, failure := parseExpirationDate(item.ExpirationDate)
			if failure != nil {
				add(i, "expiration_date", "Must be a YYYY-MM-DD date or null.")
			}
			d.ExpirationEdited, d.ExpirationDate = true, date
		}

		out = append(out, d)
	}

	if len(fields) > 0 {
		return nil, ValidationFailed(fields, nil)
	}
	return out, nil
}
