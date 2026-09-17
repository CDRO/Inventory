package httpapi

import (
	"context"
	"errors"
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
	store    ProductStore
	pictures productPictures
	errors   *ErrorWriter
}

// NewProductHandler wires the product routes. cache and productImages may be
// nil, in which case setting a picture is refused and setting an icon works.
func NewProductHandler(s ProductStore, cache ImageCache, productImages PhotoStore, errs *ErrorWriter) *ProductHandler {
	return &ProductHandler{store: s, errors: errs, pictures: productPictures{images: productImages, cache: cache}}
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
//
// The body is `{"image": "<suggestion hash>"}` or `{"icon_name": "…"}`; both
// null clears the picture. A picture is taken by the hash of a picked image
// suggestion and promoted into permanent storage, never by a URL: a
// suggestion-cache URL recorded on a product could be evicted from under it,
// and any other URL would let a caller point a household's product at a
// server of their choosing (docs/specs/07-shopping-list-reconciliation.md).
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
		Image    *string `json:"image"`
		IconName *string `json:"icon_name"`
	}
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var picture string
	var imageURL *string
	if body.Image != nil {
		name, url, _, err := h.pictures.promoteSuggestion(r.Context(), storageID, *body.Image)
		switch {
		case errors.Is(err, errPictureUnavailable):
			h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
				"image": {"That picture is no longer available. Pick another."},
			}, err))
			return
		case err != nil:
			h.errors.WriteError(w, r, Internal(err))
			return
		}
		picture, imageURL = name, &url
	}

	if err := h.store.SetProductImageAsUser(r.Context(), storageID, productID, imageURL, body.IconName, user.ID); err != nil {
		h.pictures.remove(r.Context(), h.errors, picture)
		h.errors.WriteError(w, r, FromStoreError(err, "product not found in storage"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"image_url": imageURL, "icon_name": body.IconName})
}
