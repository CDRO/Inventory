package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// JobStore is the slice of the store the job routes read and delete.
type JobStore interface {
	Job(ctx context.Context, storageID, id uuid.UUID) (*store.Job, error)
	ListJobs(ctx context.Context, storageID uuid.UUID, statuses []store.JobStatus, after *uuid.UUID, limit int) ([]store.Job, error)
	DeleteJob(ctx context.Context, storageID, id uuid.UUID) (imageFilename *string, err error)
	DiscardJobs(ctx context.Context, storageID uuid.UUID, upTo time.Time) ([]store.DiscardedJob, error)
}

// PhotoStore holds the photos behind review jobs. *uploads.Dir satisfies it.
type PhotoStore interface {
	Save(name string, data []byte) error
	Read(name string) ([]byte, error)
	Remove(name string) error
}

// inboxStatuses are the jobs a review inbox shows when no status is asked for:
// everything not yet applied (docs/specs/06-vision-shelf-ingestion.md).
var inboxStatuses = []store.JobStatus{store.JobPending, store.JobDone, store.JobFailed}

// JobHandler serves the job routes of docs/specs/04-backend-api-conventions.md.
//
// Every route is storage-scoped, and any member of the storage may read or
// discard any of its jobs — not only the uploader. Rights within a storage are
// flat, and the person with time to review the pantry is often not the person
// who photographed it.
type JobHandler struct {
	store  JobStore
	photos PhotoStore
	// reanalyzers run a job's vision call again, by the kind of job each one
	// created. A kind with none cannot be analysed again.
	reanalyzers map[store.JobKind]Reanalyzer
	// cutouts and backgrounds are background removal
	// (docs/specs/09-consumption-logging.md): what a job's response says is on
	// offer, and what discarding or re-analysing a job clears away.
	cutouts     CutoutStore
	backgrounds BackgroundRemover
	errors      *ErrorWriter
}

// NewJobHandler wires the job routes. photos may be nil for a deployment with
// no photo jobs; the image route then answers 404 and discards remove no files.
// cutouts and backgrounds are nil for a deployment without background removal.
func NewJobHandler(s JobStore, photos PhotoStore, reanalyzers map[store.JobKind]Reanalyzer, cutouts CutoutStore, backgrounds BackgroundRemover, errs *ErrorWriter) *JobHandler {
	return &JobHandler{store: s, photos: photos, reanalyzers: reanalyzers, cutouts: cutouts, backgrounds: backgrounds, errors: errs}
}

// Image serves GET /api/storages/{storage_id}/jobs/{id}/image — the photo a
// review renders its crops from.
//
// Storage-scoped through the job lookup, so one storage's members cannot fetch
// another's photos by guessing a job id. The filename comes from the job row,
// never from the request.
func (h *JobHandler) Image(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	id, failure := idFromPath(r, "id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	job, err := h.store.Job(r.Context(), storageID, id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}
	if job.ImageFilename == nil || h.photos == nil {
		h.errors.WriteError(w, r, NotFound("job has no image"))
		return
	}

	data, err := h.photos.Read(*job.ImageFilename)
	if errors.Is(err, os.ErrNotExist) {
		h.errors.WriteError(w, r, NotFound("job image missing on disk"))
		return
	}
	if err != nil {
		h.errors.WriteError(w, r, Internal(err))
		return
	}

	header := w.Header()
	contentType := "image/jpeg"
	if strings.HasSuffix(*job.ImageFilename, ".png") {
		contentType = "image/png"
	}
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	// A photo taken inside someone's home: private to the browser, never a
	// shared cache, and never for longer than the job that holds it may live.
	header.Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// jobResponse is one job, with its proposal once it has one. This is the shape
// js/jobs.js polls for.
type jobResponse struct {
	ID      uuid.UUID       `json:"id"`
	Kind    store.JobKind   `json:"kind"`
	Status  store.JobStatus `json:"status"`
	Payload json.RawMessage `json:"payload"`
	Error   *string         `json:"error"`
	// HasImage says whether GET .../image will serve a photo, so a review
	// screen does not request one that is not there.
	HasImage bool `json:"has_image"`
	// BackgroundRemoval says whether a picture taken from this proposal's
	// photo can have its background removed right now, so a review screen
	// renders no such control when it cannot
	// (docs/specs/09-consumption-logging.md).
	BackgroundRemoval bool      `json:"background_removal"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// jobSummary is a job as the inbox lists it. No payload: a proposal can hold a
// whole shelf's worth of items, and the list needs none of them until the
// user opens one.
type jobSummary struct {
	ID     uuid.UUID       `json:"id"`
	Kind   store.JobKind   `json:"kind"`
	Status store.JobStatus `json:"status"`
	// ItemCount is how many rows the proposal holds, for the inbox
	// (docs/specs/06-vision-shelf-ingestion.md). Null until the job has a
	// proposal.
	ItemCount *int      `json:"item_count"`
	HasImage  bool      `json:"has_image"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Get serves GET /api/storages/{storage_id}/jobs/{id}.
func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	id, failure := idFromPath(r, "id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	job, err := h.store.Job(r.Context(), storageID, id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}

	writeJSON(w, http.StatusOK, jobResponse{
		ID: job.ID, Kind: job.Kind, Status: job.Status, Payload: job.Payload,
		Error: job.Error, HasImage: job.ImageFilename != nil,
		BackgroundRemoval: h.offersBackgroundRemoval(r.Context(), job),
		CreatedAt:         job.CreatedAt, UpdatedAt: job.UpdatedAt,
	})
}

// offersBackgroundRemoval reports whether a review of job may offer background
// removal: a proposal waiting for review, that can create products, with a
// photo to take pictures from, on a deployment whose image model is available.
//
// The model is checked last, and only for such a job, so a screen polling a
// pending job never asks about it. The check reads the cached model list either
// way.
func (h *JobHandler) offersBackgroundRemoval(ctx context.Context, job *store.Job) bool {
	if h.backgrounds == nil || h.cutouts == nil || job.Status != store.JobDone || job.ImageFilename == nil {
		return false
	}
	if job.Kind != store.JobShelfIngestion && job.Kind != store.JobProductPhoto {
		return false
	}
	_, ok := h.backgrounds.Available(ctx)
	return ok
}

// List serves GET /api/storages/{storage_id}/jobs?status=…&limit=…&cursor=…,
// newest first.
//
// status takes one value or a comma-separated set. Omitted, it means the
// review inbox: pending, done and failed — everything not yet applied.
func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
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

	statuses := inboxStatuses
	if raw := r.URL.Query().Get("status"); raw != "" {
		statuses = nil
		for _, part := range strings.Split(raw, ",") {
			status := store.JobStatus(strings.TrimSpace(part))
			if !status.Valid() {
				h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
					"status": {"Must be pending, done, failed or consumed."},
				}, nil))
				return
			}
			statuses = append(statuses, status)
		}
	}

	jobs, err := h.store.ListJobs(r.Context(), storageID, statuses, page.After, page.Limit+1)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "list jobs"))
		return
	}

	summaries := make([]jobSummary, 0, len(jobs))
	for _, j := range jobs {
		summaries = append(summaries, jobSummary{
			ID: j.ID, Kind: j.Kind, Status: j.Status, Error: j.Error,
			ItemCount: countRows(j.Payload), HasImage: j.ImageFilename != nil,
			CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, pageOf(summaries, page.Limit, func(s jobSummary) uuid.UUID { return s.ID }))
}

// countRows reads the size of a proposal without decoding the rows themselves.
// A payload with no rows array — no proposal yet, or a job kind whose payload
// has another shape — has no count.
func countRows(payload json.RawMessage) *int {
	if len(payload) == 0 {
		return nil
	}
	var p struct {
		Rows *[]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Rows == nil {
		return nil
	}
	n := len(*p.Rows)
	return &n
}

// Delete serves DELETE /api/storages/{storage_id}/jobs/{id}: discard a job.
//
// Discarding a job whose vision call is still running is allowed. The runner
// finds the row gone when the result arrives and drops it.
func (h *JobHandler) Delete(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}
	id, failure := idFromPath(r, "id", "malformed job id")
	if failure != nil {
		h.errors.WriteError(w, r, failure)
		return
	}

	image, err := h.store.DeleteJob(r.Context(), storageID, id)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}
	h.removeJobPhoto(r.Context(), image)
	removeJobCutouts(r.Context(), h.cutouts, h.errors, id)
	writeJSON(w, http.StatusNoContent, nil)
}

// removeJobPhoto is best effort, exactly like removeJobCutouts: the job's row
// is already gone, so a failure here leaves an unreferenced file on disk
// rather than a broken job — logged, and not a reason to tell the user their
// discard failed. Shared by Delete and DiscardAll so the one rule lives once.
func (h *JobHandler) removeJobPhoto(ctx context.Context, image *string) {
	if image == nil || h.photos == nil {
		return
	}
	if err := h.photos.Remove(*image); err != nil {
		h.errors.Log(ctx, "removing a discarded job's photo failed", err)
	}
}

// discardAllUpToParam names the query parameter DiscardAll requires
// (docs/specs/32-inbox-discard-all.md).
const discardAllUpToParam = "up_to"

// discardAllResponse carries only the count. The rows are already gone by the
// time this is written, so there are no ids left to report.
type discardAllResponse struct {
	Discarded int `json:"discarded"`
}

// DiscardAll serves DELETE /api/storages/{storage_id}/jobs?up_to=<RFC3339>:
// "Discard all" (docs/specs/32-inbox-discard-all.md).
//
// up_to is required, unlike readDeltaSince's optional updated_since — a bare
// DELETE that wiped a storage's whole inbox would be too easy to send by
// accident, so a missing or unparseable value is 422 and deletes nothing.
func (h *JobHandler) DiscardAll(w http.ResponseWriter, r *http.Request) {
	storageID, ok := StorageIDFrom(r.Context())
	if !ok {
		h.errors.WriteError(w, r, Internal(errNoStorageInContext))
		return
	}

	raw := r.URL.Query().Get(discardAllUpToParam)
	upTo, err := time.Parse(time.RFC3339, raw)
	if raw == "" || err != nil {
		h.errors.WriteError(w, r, ValidationFailed(map[string][]string{
			discardAllUpToParam: {"Required. Must be an RFC 3339 timestamp, e.g. 2026-09-21T14:30:00Z."},
		}, err))
		return
	}

	jobs, err := h.store.DiscardJobs(r.Context(), storageID, upTo)
	if err != nil {
		h.errors.WriteError(w, r, FromStoreError(err, "discard jobs"))
		return
	}
	for _, job := range jobs {
		h.removeJobPhoto(r.Context(), job.ImageFilename)
		removeJobCutouts(r.Context(), h.cutouts, h.errors, job.ID)
	}
	writeJSON(w, http.StatusOK, discardAllResponse{Discarded: len(jobs)})
}
