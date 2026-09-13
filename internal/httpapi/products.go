package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// ProductStore is the slice of the store the product routes read.
type ProductStore interface {
	ListProducts(ctx context.Context, storageID uuid.UUID) ([]store.Product, error)
	ListProductBatches(ctx context.Context, storageID, productID uuid.UUID) ([]store.Batch, error)
}

// ProductHandler serves the two read-only product routes
// docs/specs/09-consumption-logging.md needs: naming an existing product to
// correct a row to, and listing the batches a decrement can be split across.
// Browsing, creating or managing products belongs to specs 10 and 11; nothing
// here does either.
type ProductHandler struct {
	store  ProductStore
	errors *ErrorWriter
}

// NewProductHandler wires the product routes.
func NewProductHandler(s ProductStore, errs *ErrorWriter) *ProductHandler {
	return &ProductHandler{store: s, errors: errs}
}

// productResponse is id and name only — everything the manual-correction
// picker needs and nothing else. category_id and catalog_id are
// deliberately absent, the same restraint batchResponse and every other
// handler in this package applies to its store type.
type productResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// List serves GET /api/storages/{storage_id}/products.
func (h *ProductHandler) List(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	products, err := h.store.ListProducts(r.Context(), storageID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list products"))
		return
	}

	out := make([]productResponse, 0, len(products))
	for _, p := range products {
		out = append(out, productResponse{ID: p.ID, Name: p.Name})
	}
	writeJSON(w, http.StatusOK, collection[productResponse]{Items: out})
}

// Batches serves GET /api/storages/{storage_id}/products/{product_id}/batches
// — the batch picker docs/specs/09-consumption-logging.md needs once a row's
// product is known, ordered nearest-expiry first, the default first-out
// batch.
func (h *ProductHandler) Batches(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	batches, err := h.store.ListProductBatches(r.Context(), storageID, productID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}

	out := make([]batchResponse, 0, len(batches))
	for _, b := range batches {
		out = append(out, newBatchResponse(b))
	}
	writeJSON(w, http.StatusOK, collection[batchResponse]{Items: out})
}
