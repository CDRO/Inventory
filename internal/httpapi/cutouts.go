package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// cutoutTimeout bounds one background removal. The reviewer is waiting on the
// answer, and it is shorter than the server's write timeout (cmd/inventory), so
// a provider that never answers ends in a 502 the review screen shrugs off
// rather than in a dropped connection.
const cutoutTimeout = 60 * time.Second

// CutoutStore holds background-removed pictures while their job is under
// review, one set per job. *uploads.JobDirs satisfies it.
type CutoutStore interface {
	Save(job uuid.UUID, name string, data []byte) error
	Read(job uuid.UUID, name string) ([]byte, error)
	RemoveAll(job uuid.UUID) error
}

// BackgroundRemover cuts a picture's subject out of its background
// (internal/ingest). Available names the image model it checked.
type BackgroundRemover interface {
	Available(ctx context.Context) (model string, ok bool)
	Remove(ctx context.Context, picture []byte, format images.Format) (*images.Result, error)
}

// cutoutRequest is the body of POST .../ingest/{job_id}/cutouts.
type cutoutRequest struct {
	RowID string `json:"row_id"`
	// Source is the picture to remove the background from, as a confirm would
	// name it: "crop" or "photo".
	Source string `json:"source"`
}

// Cutout serves POST /api/storages/{storage_id}/ingest/{job_id}/cutouts:
// remove the background from a picture a new product could take from the
// reviewed photo (docs/specs/09-consumption-logging.md, "Optional: background
// removal on a user photo").
//
// The cutout is kept with the job, not with any product, and answered with its
// id and address. The review screen shows it next to the original; only a
// confirm naming it, as image "cutout", makes it a product picture. Every
// failure of the model — withdrawn, slow, unusable reply — is an error the
// screen answers by keeping the original, so nothing here retries.
func (h *IngestHandler) Cutout(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	jobID, failure := idFromPath(r, "job_id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	var body cutoutRequest
	if failure := decodeJSON(w, r, &body); failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	fields := map[string][]string{}
	if body.RowID == "" {
		fields["row_id"] = []string{"Required."}
	}
	if body.Source != productImageCrop && body.Source != productImagePhoto {
		fields["source"] = []string{"Must be crop or photo."}
	}
	if len(fields) > 0 {
		h.errors.WriteError(w, r, ValidationFailed(fields, nil))
		return
	}

	model, available := h.backgrounds.Available(r.Context())
	if !available {
		h.errors.WriteError(w, r, ModelUnavailable(model))
		return
	}

	job, err := h.store.Job(r.Context(), storageID, jobID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}
	// A consumption job is not an ingestion job, and never gives a product a
	// picture: it is answered like any other id this route does not name.
	if job.Kind != store.JobShelfIngestion && job.Kind != store.JobProductPhoto {
		h.errors.WriteError(w, r, NotFound("job is not an ingestion job"))
		return
	}
	if job.Status != store.JobDone {
		h.errors.WriteError(w, r, Conflict("This proposal is not waiting for review.", nil))
		return
	}

	found, box, err := proposalRow(job.Payload, body.RowID)
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if !found {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{"row_id": {"No such row in this proposal."}}, nil))
		return
	}
	if body.Source == productImageCrop && box == nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{"source": {"This item has no crop. Use the whole photo instead."}}, nil))
		return
	}
	if body.Source == productImagePhoto {
		box = nil
	}

	if h.photos == nil {
		h.errors.WriteError(w, r, Internal(errProductImagesUnavailable))
		return
	}
	noPhoto := ValidationFailed(map[string][]string{"source": {"This proposal has no photo to take a picture from."}}, nil)
	if job.ImageFilename == nil {
		h.errors.WriteError(w, r, noPhoto)
		return
	}
	photo, err := h.photos.Read(*job.ImageFilename)
	if errors.Is(err, os.ErrNotExist) {
		h.errors.WriteError(w, r, noPhoto)
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	// The same picture a confirm would cut, so what the reviewer compares is
	// exactly the original they would otherwise keep.
	picture, err := images.ProductImage(photo, box)
	if errors.Is(err, images.ErrEmptyCrop) {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{"source": {"This item's crop is empty. Use the whole photo instead."}}, nil))
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), cutoutTimeout)
	defer cancel()
	cutout, err := h.backgrounds.Remove(ctx, picture.Data, picture.Format)
	if errors.Is(err, vision.ErrModelNotFound) {
		h.errors.WriteError(w, r, ModelUnavailable(model))
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, UpstreamFailed(err))
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}
	if err := h.cutouts.Save(job.ID, cutoutName(id), cutout.Data); err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	writeJSON(w, http.StatusCreated, struct {
		CutoutID uuid.UUID `json:"cutout_id"`
		URL      string    `json:"url"`
	}{id, cutoutURL(storageID, job.ID, id)})
}

// CutoutImage serves GET
// /api/storages/{storage_id}/ingest/{job_id}/cutouts/{cutout_id}.
//
// Storage-scoped through the job lookup, like the photo it was cut from: a
// cutout is a picture taken inside someone's home. The file is found by the
// job's id and the parsed cutout id, never by a path from the request.
func (h *IngestHandler) CutoutImage(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	jobID, failure := idFromPath(r, "job_id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}
	cutoutID, failure := idFromPath(r, "cutout_id", "malformed cutout id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	job, err := h.store.Job(r.Context(), storageID, jobID)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}
	cutout, err := h.readCutout(job.ID, cutoutID)
	if errors.Is(err, os.ErrNotExist) {
		h.errors.WriteError(w, r, NotFound("cutout not found for job"))
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	header := w.Header()
	header.Set("Content-Type", cutout.Format.ContentType())
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cutout.Data)
}

// readCutout returns one of a job's cutouts. One that does not exist — never
// made, or removed with its job's review — is os.ErrNotExist, and so is every
// cutout of a deployment without background removal.
func (h *IngestHandler) readCutout(jobID, cutoutID uuid.UUID) (*images.Result, error) {
	if h.cutouts == nil {
		return nil, os.ErrNotExist
	}
	data, err := h.cutouts.Read(jobID, cutoutName(cutoutID))
	if err != nil {
		return nil, err
	}
	return &images.Result{Data: data, Format: images.FormatPNG}, nil
}

// removeCutouts drops every cutout made for a job, once its review is over.
func (h *IngestHandler) removeCutouts(ctx context.Context, jobID uuid.UUID) {
	removeJobCutouts(ctx, h.cutouts, h.errors, jobID)
}

// removeJobCutouts is best effort: the job's state has already changed, so a
// failure leaves unreachable files on disk — logged, and not a reason to tell
// the reviewer their confirm, discard or re-analysis failed.
func removeJobCutouts(ctx context.Context, cutouts CutoutStore, errs *ErrorWriter, jobID uuid.UUID) {
	if cutouts == nil {
		return
	}
	if err := cutouts.RemoveAll(jobID); err != nil {
		errs.Log(ctx, "removing a job's cutouts failed", err)
	}
}

// cutoutName is the file a cutout is stored under. Always a PNG: a cutout's
// whole point is its transparent background.
func cutoutName(id uuid.UUID) string {
	return id.String() + images.FormatPNG.Extension()
}

func cutoutURL(storageID, jobID, cutoutID uuid.UUID) string {
	return "/api/storages/" + storageID.String() + "/ingest/" + jobID.String() + "/cutouts/" + cutoutID.String()
}

// proposalRow finds one row of a proposal by row_id, and its bounding_box if
// it has one.
func proposalRow(payload json.RawMessage, rowID string) (found bool, box *images.Box, err error) {
	var p struct {
		Rows []struct {
			RowID       string      `json:"row_id"`
			BoundingBox *images.Box `json:"bounding_box"`
		} `json:"rows"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			return false, nil, fmt.Errorf("httpapi: read proposal row: %w", err)
		}
	}
	for _, row := range p.Rows {
		if row.RowID == rowID {
			return true, row.BoundingBox, nil
		}
	}
	return false, nil, nil
}
