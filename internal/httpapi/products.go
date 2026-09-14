package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// ProductStore is the slice of the store the product routes read and write.
type ProductStore interface {
	ListProducts(ctx context.Context, storageID uuid.UUID) ([]store.Product, error)
	ListProductBatches(ctx context.Context, storageID, productID uuid.UUID) ([]store.Batch, error)
	SetProductCategoryAsUser(ctx context.Context, storageID, id uuid.UUID, categoryID *uuid.UUID, userID uuid.UUID) error
	SetProductImageAsUser(ctx context.Context, storageID, id uuid.UUID, imageURL, iconName *string, userID uuid.UUID) error
}

// ProductHandler serves the two read-only product routes
// docs/specs/09-consumption-logging.md needs — naming an existing product to
// correct a row to, and listing the batches a decrement can be split across —
// plus the two narrow write routes docs/specs/52-gamification-quests-and-ui.md
// needs so its "uncategorized" and "imageless" quests have something a user
// can actually do to close them. General product browsing or creation stays
// out of scope for this handler.
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

// SetCategory serves PATCH /api/storages/{storage_id}/products/{product_id}/category
// — the write that lets a person close the "uncategorized" quest
// (docs/specs/52-gamification-quests-and-ui.md) on an existing product.
func (h *ProductHandler) SetCategory(w http.ResponseWriter, r *http.Request) {
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
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		CategoryID *string `json:"category_id"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var categoryID *uuid.UUID
	if body.CategoryID != nil {
		parsed, err := uuid.Parse(*body.CategoryID)
		if err != nil {
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"category_id": {"Must be a UUID or null."},
			}, nil))
			return
		}
		categoryID = &parsed
	}

	if err := h.store.SetProductCategoryAsUser(r.Context(), storageID, productID, categoryID, user.ID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product or category not found in storage"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"category_id": body.CategoryID})
}

// SetImage serves PATCH /api/storages/{storage_id}/products/{product_id}/image
// — the write that lets a person close the "imageless" quest
// (docs/specs/52-gamification-quests-and-ui.md) on an existing product.
// Exactly one of image_url and icon_name is expected; the frontend already
// resolves a chosen image_url through the existing image-suggestions flow
// (docs/specs/07-shopping-list-reconciliation.md) before this call, so this
// route only ever persists a URL or icon name already on this origin.
func (h *ProductHandler) SetImage(w http.ResponseWriter, r *http.Request) {
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
	productID, failure := idFromPath(r, "product_id", "malformed product id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body struct {
		ImageURL *string `json:"image_url"`
		IconName *string `json:"icon_name"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	if err := h.store.SetProductImageAsUser(r.Context(), storageID, productID, body.ImageURL, body.IconName, user.ID); err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"image_url": body.ImageURL, "icon_name": body.IconName})
}
