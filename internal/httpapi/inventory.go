package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// InventoryStore is the slice of the store the whole-inventory read needs.
type InventoryStore interface {
	ListInventoryBatches(ctx context.Context, storageID uuid.UUID, filter store.InventoryBatchFilter, after *uuid.UUID, limit int) ([]store.InventoryBatchRow, error)
}

// InventoryHandler serves the read partner of spec 13's POST on the same
// collection (docs/specs/33-inventory-overview-table.md): every batch in a
// storage, with the names inventory.html needs already resolved, so the page
// does not have to join products, categories and locations client-side.
type InventoryHandler struct {
	store  InventoryStore
	errors *ErrorWriter
}

// NewInventoryHandler wires the handler to a store and the one error writer.
func NewInventoryHandler(s InventoryStore, errs *ErrorWriter) *InventoryHandler {
	return &InventoryHandler{store: s, errors: errs}
}

// inventoryBatchResponse is one row on the wire.
type inventoryBatchResponse struct {
	ID               uuid.UUID  `json:"id"`
	ProductID        uuid.UUID  `json:"product_id"`
	ProductName      string     `json:"product_name"`
	ImageURL         *string    `json:"image_url"`
	CategoryID       *uuid.UUID `json:"category_id"`
	CategoryName     *string    `json:"category_name"`
	LocationID       uuid.UUID  `json:"location_id"`
	LocationPath     []string   `json:"location_path"`
	Quantity         int        `json:"quantity"`
	ExpirationDate   *string    `json:"expiration_date"`
	ExpirationSource string     `json:"expiration_source"`
	CreatedAt        time.Time  `json:"created_at"`
}

// List serves GET /api/storages/{storage_id}/inventory-batches
// (docs/specs/33-inventory-overview-table.md): every batch of the storage,
// paginated per docs/specs/04-backend-api-conventions.md.
//
// ?location_id= and ?category_id= match the node or any descendant, and each
// is validated against the URL's storage like every other id in the system —
// a foreign id and an unknown one both answer the identical 404. ?q= is a
// substring match on the product name, for native clients (12); this page's
// own filtering happens client-side against the whole loaded set.
func (h *InventoryHandler) List(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	page, failure := readPage(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	filter, failure := readInventoryFilter(r)
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	rows, err := h.store.ListInventoryBatches(r.Context(), storageID, filter, page.After, page.Limit+1)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "location or category not in this storage or nonexistent"))
		return
	}

	out := make([]inventoryBatchResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, newInventoryBatchResponse(row))
	}
	writeJSON(w, http.StatusOK, pageOf(out, page.Limit, func(b inventoryBatchResponse) uuid.UUID { return b.ID }))
}

// readInventoryFilter parses the three optional query filters. Malformed
// location_id/category_id are a 422 naming the field; a filter id that
// merely does not resolve against this storage is left to the store, which
// answers the identical 404 every other foreign or unknown id gets.
func readInventoryFilter(r *http.Request) (store.InventoryBatchFilter, *Failure) {
	q := r.URL.Query()
	filter := store.InventoryBatchFilter{Q: q.Get("q")}

	fields := map[string][]string{}
	if raw := q.Get("location_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["location_id"] = append(fields["location_id"], "Must be a UUID.")
		} else {
			filter.LocationID = &id
		}
	}
	if raw := q.Get("category_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["category_id"] = append(fields["category_id"], "Must be a UUID.")
		} else {
			filter.CategoryID = &id
		}
	}
	if len(fields) > 0 {
		return filter, ValidationFailed(fields, nil)
	}
	return filter, nil
}

func newInventoryBatchResponse(row store.InventoryBatchRow) inventoryBatchResponse {
	out := inventoryBatchResponse{
		ID:               row.ID,
		ProductID:        row.ProductID,
		ProductName:      row.ProductName,
		ImageURL:         row.ImageURL,
		CategoryID:       row.CategoryID,
		CategoryName:     row.CategoryName,
		LocationID:       row.LocationID,
		LocationPath:     row.LocationPath,
		Quantity:         row.Quantity,
		ExpirationSource: string(row.ExpirationSource),
		CreatedAt:        row.CreatedAt,
	}
	if out.LocationPath == nil {
		out.LocationPath = []string{}
	}
	if row.ExpirationDate != nil {
		// A DATE column is a calendar day, not an instant — the same reasoning
		// as newBatchResponse.
		day := row.ExpirationDate.Format(time.DateOnly)
		out.ExpirationDate = &day
	}
	return out
}
