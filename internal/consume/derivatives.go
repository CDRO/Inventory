package consume

import (
	"context"
	"log/slog"

	"github.com/CDRO/Inventory/internal/images"
)

// Derivatives makes the pictures a review screen shows from a consumption
// photo ahead of the first request (docs/specs/43-image-derivatives.md). The
// same shape internal/ingest asks for, so one derive.Jobs serves both.
type Derivatives interface {
	EnsurePhotoSet(ctx context.Context, filename string, photo []byte) error
	ReplaceRowSet(ctx context.Context, filename string, photo []byte, boxes map[string]images.Box) error
}

// WithDerivatives makes the service render a job's thumbnails and crops as
// part of its work.
func (s *Service) WithDerivatives(d Derivatives) *Service {
	s.derivatives = d
	return s
}

// startPhotoSet begins the whole-picture set in the background and returns
// what waits for it. Best effort, for the reason internal/ingest gives: a
// proposal must never be lost to a thumbnail, and the serving route makes a
// missing picture on request.
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
// crops of whatever proposal came before.
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
