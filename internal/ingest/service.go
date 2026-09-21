package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// ImageRetention is how long a reviewed photo is kept after its job was
// confirmed (docs/specs/04-backend-api-conventions.md).
const ImageRetention = 30 * 24 * time.Hour

// Messages a reviewer reads in the inbox when a job fails. Each one says what
// to do next, because it is read with no other context.
const (
	msgModelUnavailable = "The configured vision model is unavailable. Ask an admin to choose another one, then upload the photo again."
	msgUnreadable       = "The photo could not be analysed. Try uploading it again."
	msgPhotoMissing     = "The photo for this job is missing. Upload it again."
)

// Runner starts background jobs (internal/jobs).
type Runner interface {
	Submit(ctx context.Context, in store.NewJob, work jobs.Work) (*store.Job, error)
	Resubmit(ctx context.Context, storageID, id uuid.UUID, work jobs.Work) error
}

// Analyzer sends a photo to the vision model (internal/vision).
type Analyzer interface {
	Analyze(ctx context.Context, model string, mode vision.Mode, image []byte, mimeType string) (*vision.Analysis, error)
}

// Models is the model-availability checker (internal/vision).
type Models interface {
	EffectiveModel(ctx context.Context) (string, error)
	Status(ctx context.Context) string
	Invalidate()
}

// Photos is the ingest upload area (internal/uploads).
type Photos interface {
	Save(name string, data []byte) error
	Read(name string) ([]byte, error)
	Remove(name string) error
}

// Store is what the service reads and sweeps.
type Store interface {
	LocationTree(ctx context.Context, storageID uuid.UUID) ([]store.Location, error)
	ExpiredJobImages(ctx context.Context, cutoff time.Time, limit int) (map[uuid.UUID]string, error)
	ClearJobImage(ctx context.Context, id uuid.UUID) error
}

// Service starts ingestion jobs and does their work.
type Service struct {
	runner   Runner
	analyzer Analyzer
	models   Models
	matcher  Matcher
	store    Store
	photos   Photos
	log      *slog.Logger
}

// NewService wires the ingestion flow.
func NewService(runner Runner, analyzer Analyzer, models Models, matcher Matcher, s Store, photos Photos, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{runner: runner, analyzer: analyzer, models: models, matcher: matcher, store: s, photos: photos, log: log}
}

// Available reports whether photos can be analysed right now, and names the
// model it checked, for the 503 model_unavailable the upload answers with
// otherwise (docs/specs/01-architecture-and-deployment.md).
func (s *Service) Available(ctx context.Context) (model string, ok bool) {
	model, _ = s.models.EffectiveModel(ctx)
	return model, s.models.Status(ctx) == vision.StatusOK
}

// Upload is one photo to ingest.
type Upload struct {
	StorageID      uuid.UUID
	Kind           store.JobKind
	CreatedBy      uuid.UUID
	LocationHintID *uuid.UUID
	// Filename is the generated name the stripped image is stored under.
	Filename string
	Image    []byte
}

// ErrUnsupportedKind is a job kind this service does not ingest.
var ErrUnsupportedKind = errors.New("ingest: unsupported job kind")

// Start saves the photo and submits its job.
//
// The photo is written before the job exists, so a vision call that fails —
// or a server that restarts mid-call — never costs the user their photo
// (docs/specs/04-backend-api-conventions.md). If the job cannot be created the
// photo is removed again: nothing would ever reference it.
func (s *Service) Start(ctx context.Context, u Upload) (*store.Job, error) {
	mode, ok := modes[u.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, u.Kind)
	}

	if err := s.photos.Save(u.Filename, u.Image); err != nil {
		return nil, err
	}

	createdBy := u.CreatedBy
	job, err := s.runner.Submit(ctx, store.NewJob{
		StorageID:      u.StorageID,
		Kind:           u.Kind,
		CreatedBy:      &createdBy,
		ImageFilename:  &u.Filename,
		LocationHintID: u.LocationHintID,
	}, s.work(u.StorageID, mode, u.Filename, u.LocationHintID))
	if err != nil {
		if rmErr := s.photos.Remove(u.Filename); rmErr != nil {
			// WarnContext, not Warn: Start runs inside the upload request, so the
			// context carries the request id that ties this line to it
			// (docs/specs/18-operations-and-observability.md).
			s.log.WarnContext(ctx, "removing the photo of a job that was never created failed", slog.Any("err", rmErr))
		}
		return nil, err
	}
	return job, nil
}

// Reanalyze runs a finished job's vision call again, on the photo it already
// has — "Analyze again" (docs/specs/09-consumption-logging.md).
//
// It is the whole job or nothing: the old proposal, with any correction a
// reviewer made to it, is replaced by the new one. That is why it only ever
// happens when someone asks for it. The job reads as pending until the new call
// finishes, exactly as it did after upload, and keeps its location hint.
//
// A job the store will not requeue — still pending, already applied, without a
// photo — is its ErrConflict, and nothing runs.
func (s *Service) Reanalyze(ctx context.Context, job *store.Job) error {
	mode, ok := modes[job.Kind]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnsupportedKind, job.Kind)
	}
	if job.ImageFilename == nil {
		return fmt.Errorf("%w: job has no photo", store.ErrConflict)
	}
	return s.runner.Resubmit(ctx, job.StorageID, job.ID,
		s.work(job.StorageID, mode, *job.ImageFilename, job.LocationHintID))
}

var modes = map[store.JobKind]vision.Mode{
	store.JobShelfIngestion: vision.ModeShelf,
	store.JobProductPhoto:   vision.ModeProduct,
}

// work is one job: read the photo, ask the model, build the proposal.
//
// Failures that a reviewer can act on come back as jobs.UserError with a
// message written for them. Anything else becomes the runner's generic
// message, and the detail goes to the log.
func (s *Service) work(storageID uuid.UUID, mode vision.Mode, filename string, hint *uuid.UUID) jobs.Work {
	return func(ctx context.Context) (json.RawMessage, error) {
		image, err := s.photos.Read(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &jobs.UserError{Message: msgPhotoMissing, Err: err}
		}
		if err != nil {
			return nil, err
		}

		model, err := s.models.EffectiveModel(ctx)
		if err != nil {
			return nil, err
		}

		analysis, err := s.analyzer.Analyze(ctx, model, mode, image, mimeTypeOf(filename))
		switch {
		case errors.Is(err, vision.ErrModelNotFound):
			// The provider has withdrawn the model since the upload was
			// accepted. Dropping the cached model list is what flips /healthz
			// and the next upload to model_unavailable, rather than letting
			// every photo fail the same way one at a time.
			s.models.Invalidate()
			return nil, &jobs.UserError{Message: msgModelUnavailable, Err: err}
		case errors.Is(err, vision.ErrMalformedResponse):
			return nil, &jobs.UserError{Message: msgUnreadable, Err: err}
		case err != nil:
			return nil, err
		}

		tree, err := s.store.LocationTree(ctx, storageID)
		if err != nil {
			return nil, err
		}

		proposal, err := buildProposal(ctx, s.matcher, storageID, mode, hint, tree, analysis)
		if err != nil {
			return nil, err
		}
		return json.Marshal(proposal)
	}
}

func mimeTypeOf(filename string) string {
	if strings.HasSuffix(filename, ".png") {
		return "image/png"
	}
	return "image/jpeg"
}

// SweepImages deletes the photos of jobs confirmed more than ImageRetention
// ago. A photo still waiting for review is never touched, however old.
func (s *Service) SweepImages(ctx context.Context, now time.Time) (int, error) {
	expired, err := s.store.ExpiredJobImages(ctx, now.Add(-ImageRetention), 500)
	if err != nil {
		return 0, err
	}
	removed := 0
	for id, name := range expired {
		if err := s.photos.Remove(name); err != nil {
			s.log.Warn("removing an expired job photo failed", slog.String("job_id", id.String()), slog.Any("err", err))
			continue
		}
		if err := s.store.ClearJobImage(ctx, id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
