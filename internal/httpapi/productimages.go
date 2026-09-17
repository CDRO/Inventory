package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/uploads"
)

// ProductImageStore answers whether a stored product picture belongs to a
// storage. *store.Store satisfies it.
type ProductImageStore interface {
	ProductImageInStorage(ctx context.Context, storageID uuid.UUID, imageURL string) (bool, error)
}

// productImageURL is the address a product picture in permanent storage is
// recorded under in products.image_url, and served from.
//
// The storage id is part of the URL on purpose. The file on disk has no owner;
// the URL a product records does, so the serving route can check the one it
// was asked for against the storage it was asked under.
func productImageURL(storageID uuid.UUID, name string) string {
	return "/api/storages/" + storageID.String() + "/product-images/" + name
}

// ProductImageHandler serves product pictures taken from a user's own photo.
type ProductImageHandler struct {
	store  ProductImageStore
	images PhotoStore
	errors *ErrorWriter
}

// NewProductImageHandler wires the product picture route. images may be nil
// when the upload volume is unusable, in which case every picture is a 404.
func NewProductImageHandler(s ProductImageStore, images PhotoStore, errs *ErrorWriter) *ProductImageHandler {
	return &ProductImageHandler{store: s, images: images, errors: errs}
}

// Serve serves GET /api/storages/{storage_id}/product-images/{name}.
//
// A picture is served only when a product in this storage uses it. Another
// storage's picture, a picture no product uses, a name the server would never
// generate and a name that was never written all answer the same 404
// (docs/specs/03-auth-and-multi-tenancy.md) — the name is a UUID, but being
// hard to guess is not the access check.
func (h *ProductImageHandler) Serve(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	name := chi.URLParam(r, "name")
	if h.images == nil {
		h.errors.WriteError(w, r, NotFound("product images unavailable"))
		return
	}

	used, err := h.store.ProductImageInStorage(r.Context(), storageID, productImageURL(storageID, name))
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if !used {
		h.errors.WriteError(w, r, NotFound("product image not used in storage"))
		return
	}

	data, err := h.images.Read(name)
	switch {
	case errors.Is(err, uploads.ErrInvalidName), errors.Is(err, os.ErrNotExist):
		h.errors.WriteError(w, r, NotFound("product image missing"))
		return
	case err != nil:
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	header := w.Header()
	header.Set("Content-Type", pictureContentType(name))
	header.Set("X-Content-Type-Options", "nosniff")
	if strings.HasSuffix(name, ".svg") {
		// A promoted icon is served from our own origin, so like a cached one
		// it may execute nothing if opened directly (docs/specs/07-shopping-list-reconciliation.md).
		header.Set("Content-Security-Policy", "default-src 'none'")
	}
	// Still a photo taken inside someone's home, so never a shared cache. The
	// name never changes content once written, so a day in the browser's own
	// cache is safe and spares a household's lists re-downloading every
	// picture on every visit.
	header.Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
