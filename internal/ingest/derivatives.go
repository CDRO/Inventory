package ingest

import (
	"context"
	"log/slog"

	"github.com/CDRO/Inventory/internal/images"
)

// Derivatives makes the pictures a screen shows from a job's photo — the
// inbox thumbnail, the review preview, each detected item's crop — ahead of
// the first request (docs/specs/43-image-derivatives.md). derive.Jobs
// satisfies it.
type Derivatives interface {
	// EnsurePhotoSet makes the whole-picture variants unless they exist.
	EnsurePhotoSet(ctx context.Context, filename string, photo []byte) error
	// ReplaceRowSet drops the photo's row crops and makes the ones boxes asks
	// for, keyed by row id.
	ReplaceRowSet(ctx context.Context, filename string, photo []byte, boxes map[string]images.Box) error
}

// startPhotoSet begins the whole-picture set in the background and returns
// what waits for it. Best effort: a failure is logged and changes nothing
// about the job, because the serving route makes a missing picture on
// request, and a proposal must never be lost to a thumbnail.
//
// The wait is the point of returning a function rather than firing and
// forgetting: a job's work must not leave a goroutine decoding its photo
// after the job has been recorded as finished.
func (s *Service) startPhotoSet(ctx context.Context, filename string, photo []byte) (wait func()) {
	if s.derivatives == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.derivatives.EnsurePhotoSet(ctx, filename, photo); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "making a photo's thumbnails failed", slog.String("photo", filename), slog.Any("err", err))
		}
	}()
	return func() { <-done }
}

// replaceRowSet makes each row's crop from its bounding box, replacing the
// crops of whatever proposal came before. Best effort, like startPhotoSet.
func (s *Service) replaceRowSet(ctx context.Context, filename string, photo []byte, rows []Row) {
	if s.derivatives == nil {
		return
	}
	boxes := make(map[string]images.Box, len(rows))
	for _, row := range rows {
		if b := row.BoundingBox; b != nil {
			boxes[row.RowID] = images.Box{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height}
		}
	}
	if err := s.derivatives.ReplaceRowSet(ctx, filename, photo, boxes); err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "making a proposal's crops failed", slog.String("photo", filename), slog.Any("err", err))
	}
}
