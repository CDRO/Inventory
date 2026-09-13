package consume

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/jobs"
	"github.com/CDRO/Inventory/internal/store"
	"github.com/CDRO/Inventory/internal/vision"
)

// Messages a reviewer reads in the inbox when a job fails. Each one says what
// to do next, because it is read with no other context — the same shape
// internal/ingest uses, since both share the runner and its failure modes.
const (
	msgModelUnavailable = "The configured vision model is unavailable. Ask an admin to choose another one, then upload the photo again."
	msgUnreadable       = "The photo could not be analysed. Try uploading it again."
	msgPhotoMissing     = "The photo for this job is missing. Upload it again."
)

// Runner starts background jobs (internal/jobs).
type Runner interface {
	Submit(ctx context.Context, in store.NewJob, work jobs.Work) (*store.Job, error)
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

// Photos is the ingest upload area (internal/uploads). Consumption photos
// share the same upload volume as shelf and product photos — one area of
// user photos awaiting review, not three — including its 30-day retention
// sweep, which is kind-agnostic already.
type Photos interface {
	Save(name string, data []byte) error
	Read(name string) ([]byte, error)
	Remove(name string) error
}

// Service starts consumption jobs and does their work.
type Service struct {
	runner   Runner
	analyzer Analyzer
	models   Models
	matcher  Matcher
	photos   Photos
	log      *slog.Logger
}

// NewService wires the consumption-logging flow.
func NewService(runner Runner, analyzer Analyzer, models Models, matcher Matcher, photos Photos, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{runner: runner, analyzer: analyzer, models: models, matcher: matcher, photos: photos, log: log}
}

// Available reports whether photos can be analysed right now, and names the
// model it checked — the same check, against the same model, as shelf and
// product ingestion (docs/specs/01-architecture-and-deployment.md).
func (s *Service) Available(ctx context.Context) (model string, ok bool) {
	model, _ = s.models.EffectiveModel(ctx)
	return model, s.models.Status(ctx) == vision.StatusOK
}

// Upload is one photo of something being used up.
type Upload struct {
	StorageID uuid.UUID
	CreatedBy uuid.UUID
	// Filename is the generated name the stripped image is stored under.
	Filename string
	Image    []byte
}

// Start saves the photo and submits its job.
//
// The photo is written before the job exists, so a vision call that fails —
// or a server that restarts mid-call — never costs the user their photo
// (docs/specs/04-backend-api-conventions.md). If the job cannot be created the
// photo is removed again: nothing would ever reference it.
func (s *Service) Start(ctx context.Context, u Upload) (*store.Job, error) {
	if err := s.photos.Save(u.Filename, u.Image); err != nil {
		return nil, err
	}

	createdBy := u.CreatedBy
	job, err := s.runner.Submit(ctx, store.NewJob{
		StorageID:     u.StorageID,
		Kind:          store.JobConsumptionPhoto,
		CreatedBy:     &createdBy,
		ImageFilename: &u.Filename,
	}, s.work(u.StorageID, u.Filename))
	if err != nil {
		if rmErr := s.photos.Remove(u.Filename); rmErr != nil {
			s.log.Warn("removing the photo of a job that was never created failed", slog.Any("err", rmErr))
		}
		return nil, err
	}
	return job, nil
}

// work is one job: read the photo, ask the model, build the proposal.
//
// Failures a reviewer can act on come back as jobs.UserError with a message
// written for them. Anything else becomes the runner's generic message, and
// the detail goes to the log.
func (s *Service) work(storageID uuid.UUID, filename string) jobs.Work {
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

		analysis, err := s.analyzer.Analyze(ctx, model, vision.ModeConsumption, image, mimeTypeOf(filename))
		switch {
		case errors.Is(err, vision.ErrModelNotFound):
			// The provider has withdrawn the model since the upload was
			// accepted. Dropping the cached model list is what flips
			// /healthz and the next upload to model_unavailable, rather
			// than letting every photo fail the same way one at a time.
			s.models.Invalidate()
			return nil, &jobs.UserError{Message: msgModelUnavailable, Err: err}
		case errors.Is(err, vision.ErrMalformedResponse):
			return nil, &jobs.UserError{Message: msgUnreadable, Err: err}
		case err != nil:
			return nil, err
		}

		proposal, err := buildProposal(ctx, s.matcher, storageID, analysis)
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
