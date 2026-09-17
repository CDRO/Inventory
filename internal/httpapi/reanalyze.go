package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/store"
)

// Reanalyzer runs a finished job's vision call again on the photo it already
// has. internal/ingest does it for shelf and product photos, internal/consume
// for consumption photos; Available is the same model check their uploads
// make.
type Reanalyzer interface {
	Available(ctx context.Context) (model string, ok bool)
	Reanalyze(ctx context.Context, job *store.Job) error
}

// reanalyzers maps each kind of job to the service that analyses its photo.
// A kind whose service is not wired — the upload volume was unusable — is
// absent, and such a job cannot be analysed again. Shopping-list photos have no
// analysis to repeat yet (docs/specs/07-shopping-list-reconciliation.md).
func reanalyzers(d Deps) map[store.JobKind]Reanalyzer {
	out := map[store.JobKind]Reanalyzer{}
	if d.Ingester != nil {
		out[store.JobShelfIngestion] = d.Ingester
		out[store.JobProductPhoto] = d.Ingester
	}
	if d.Consumer != nil {
		out[store.JobConsumptionPhoto] = d.Consumer
	}
	return out
}

// Reanalyze serves POST /api/storages/{storage_id}/jobs/{id}/reanalyze —
// "Analyze again" (docs/specs/09-consumption-logging.md).
//
// It is the whole job: the proposal, with every correction a reviewer made to
// it, is replaced by a new one. Manual correction is never retried on its own,
// so this is the only way a job is ever analysed twice, and only because
// someone asked. The job reads as pending again until the new analysis
// finishes, and the review screen polls it exactly as it did after upload.
//
// Any member of the storage may ask, like any other review action. The answer
// is 202 with the job's own id: nothing new is created.
func (h *JobHandler) Reanalyze(w http.ResponseWriter, r *http.Request) {
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

	// Advisory only: these checks read the job before any lock and exist to
	// give a precise message. The guard is store.RequeueJob, which re-checks
	// the same conditions under FOR UPDATE and refuses a job a confirm got to
	// first.
	reanalyzer, ok := h.reanalyzers[job.Kind]
	switch {
	case !ok:
		h.errors.WriteError(w, r, Conflict("This proposal cannot be analysed again.", nil))
		return
	case job.Status != store.JobDone && job.Status != store.JobFailed:
		h.errors.WriteError(w, r, Conflict("This photo is still being analysed, or its proposal has already been applied.", nil))
		return
	case job.ImageFilename == nil:
		h.errors.WriteError(w, r, Conflict("This proposal has no photo left to analyse.", nil))
		return
	}

	// After the checks that are about the job itself, so a job that could
	// never be analysed again says so rather than blaming the model.
	if model, available := reanalyzer.Available(r.Context()); !available {
		h.errors.WriteError(w, r, ModelUnavailable(model))
		return
	}

	err = reanalyzer.Reanalyze(r.Context(), job)
	switch {
	case errors.Is(err, store.ErrConflict):
		// Lost a race since the read above: someone else confirmed it, or
		// asked for the same re-analysis a moment earlier.
		h.errors.WriteError(w, r, conflictAs(err, "", "This photo is already being analysed again, or its proposal has been applied."))
		return
	case err != nil:
		h.errors.WriteError(w, r, FromStoreError(err, "job not found in storage"))
		return
	}

	// Cutouts were made from the old proposal's rows, which are gone.
	removeJobCutouts(r.Context(), h.cutouts, h.errors, job.ID)

	writeJSON(w, http.StatusAccepted, struct {
		JobID uuid.UUID `json:"job_id"`
	}{JobID: job.ID})
}
