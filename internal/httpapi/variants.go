package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/CDRO/Inventory/internal/derive"
	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/uploads"
)

// VariantStore serves the derived forms of a stored picture — thumbnails,
// previews, row crops — making a missing one on request
// (docs/specs/43-image-derivatives.md). *derive.Service satisfies it.
//
// It never decides who may see a picture: every handler below runs the same
// access check as the route that serves the original, and only then asks.
type VariantStore interface {
	Open(ctx context.Context, area uploads.Area, source string, v images.Variant, load derive.Loader) ([]byte, string, error)
	OpenRow(ctx context.Context, area uploads.Area, source, rowID string, v images.Variant, load derive.Loader, boxes map[string]images.Box) ([]byte, string, error)
}

// Cache lifetimes for pictures, as the original routes set them: a job's
// photo lives for as long as its job may, a product picture's name never
// changes content once written.
const (
	jobPictureCache     = "private, max-age=3600"
	productPictureCache = "private, max-age=86400"
)

// writePicture answers with picture bytes, or with the error the variant
// store reported: a derivative that does not exist — a row with no box, a
// source that is gone — is the same 404 the original route gives; a client
// that hung up while its picture was being made gets nothing at all, which
// is not an error worth a log line.
func (e *ErrorWriter) writePicture(w http.ResponseWriter, r *http.Request, data []byte, contentType, cacheControl string, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, os.ErrNotExist), errors.Is(err, uploads.ErrInvalidName):
		e.WriteError(w, r, NotFound("picture not found"))
		return
	case err != nil:
		e.WriteError(w, r, Internal(err))
		return
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// jobWithImage is the shared front of the job picture routes: the storage,
// the job, and that it has a photo this deployment can serve. It writes the
// error itself and reports false when the handler should stop.
func (h *JobHandler) jobWithImage(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return nil, false
	}
	id, failure := idFromPath(r, "id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return nil, false
	}
	job, err := h.store.Job(r.Context(), storageID, id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return nil, false
	}
	if job.ImageFilename == nil || h.photos == nil || h.variants == nil {
		h.errors.WriteError(w, r, NotFound("job has no image"))
		return nil, false
	}
	return job, true
}

// ImageVariant serves GET /api/storages/{storage_id}/jobs/{id}/image/{variant}:
// the photo at the size a screen shows it — the inbox card's thumbnail, the
// review's whole-photo preview.
func (h *JobHandler) ImageVariant(w http.ResponseWriter, r *http.Request) {
	job, ok := h.jobWithImage(w, r)
	if !ok {
		return
	}
	v, ok := images.ParseVariant(chi.URLParam(r, "variant"))
	if !ok {
		h.errors.WriteError(w, r, NotFound("no such picture size"))
		return
	}
	name := *job.ImageFilename
	data, contentType, err := h.variants.Open(r.Context(), uploads.AreaIngest, name, v,
		func() ([]byte, error) { return h.photos.Read(name) })
	h.errors.writePicture(w, r, data, contentType, jobPictureCache, err)
}

// ImageRowVariant serves
// GET /api/storages/{storage_id}/jobs/{id}/image/rows/{row_id}/{variant}: one
// detected item, cut from the photo along its bounding box. The row id is
// looked up in the job's own proposal, which is what supplies the box; a row
// the proposal does not name, or names without a box, is a picture that does
// not exist.
func (h *JobHandler) ImageRowVariant(w http.ResponseWriter, r *http.Request) {
	job, ok := h.jobWithImage(w, r)
	if !ok {
		return
	}
	v, ok := images.ParseRowVariant(chi.URLParam(r, "variant"))
	if !ok {
		h.errors.WriteError(w, r, NotFound("no such picture size"))
		return
	}
	boxes, err := proposalBoxes(job.Payload)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	byRow := make(map[string]images.Box, len(boxes))
	for id, box := range boxes {
		byRow[id] = *box
	}
	name := *job.ImageFilename
	data, contentType, err := h.variants.OpenRow(r.Context(), uploads.AreaIngest, name, chi.URLParam(r, "row_id"), v,
		func() ([]byte, error) { return h.photos.Read(name) }, byRow)
	h.errors.writePicture(w, r, data, contentType, jobPictureCache, err)
}

// ServeVariant serves GET /api/storages/{storage_id}/product-images/{name}/{variant}.
//
// The same check as Serve decides whether the caller may see the picture at
// all, before any file is touched. An SVG — a promoted icon — answers every
// size with the icon itself: a vector needs no derivative.
func (h *ProductImageHandler) ServeVariant(w http.ResponseWriter, r *http.Request) {
	storageID, name, ok := h.usedPicture(w, r)
	if !ok {
		return
	}
	v, ok := images.ParseVariant(chi.URLParam(r, "variant"))
	if !ok {
		h.errors.WriteError(w, r, NotFound("no such picture size"))
		return
	}
	if strings.HasSuffix(name, ".svg") || h.variants == nil {
		h.serveStored(w, r, name)
		return
	}
	_ = storageID
	data, contentType, err := h.variants.Open(r.Context(), uploads.AreaProducts, name, v,
		func() ([]byte, error) { return h.images.Read(name) })
	h.errors.writePicture(w, r, data, contentType, productPictureCache, err)
}
