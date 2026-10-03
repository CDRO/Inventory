package derive

import (
	"context"
	"strings"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/uploads"
)

// Jobs is the Service as a photo job uses it (internal/ingest,
// internal/consume): the ingest area, the eager lane, and the photo's bytes
// already in hand, so nothing is read from disk twice.
type Jobs struct {
	svc *Service
}

// ForJobs returns the job-facing view of a Service.
func ForJobs(s *Service) Jobs {
	return Jobs{svc: s}
}

// EnsurePhotoSet makes a photo's whole-picture variants unless they exist.
func (j Jobs) EnsurePhotoSet(ctx context.Context, filename string, photo []byte) error {
	return j.svc.EnsurePhotoSet(ctx, LaneEager, uploads.AreaIngest, filename, Bytes(photo))
}

// ReplaceRowSet drops a photo's row crops and makes the ones the proposal's
// boxes ask for.
func (j Jobs) ReplaceRowSet(ctx context.Context, filename string, photo []byte, boxes map[string]images.Box) error {
	return j.svc.ReplaceRowSet(ctx, LaneEager, uploads.AreaIngest, filename, Bytes(photo), boxes)
}

// Saving wraps the product picture directory so that every picture written
// to it gets its thumbnails made in the background — productPictures.save in
// internal/httpapi is the one path a picture takes into permanent storage,
// and it saves through this. Reads and removes pass straight through; the
// directory itself removes a picture's derivatives with it.
type Saving struct {
	Dir     *uploads.Dir
	Service *Service
}

// Save writes the picture and schedules its set. An SVG — a promoted icon —
// has no derivatives: it is served as it is at every size.
func (s Saving) Save(name string, data []byte) error {
	if err := s.Dir.Save(name, data); err != nil {
		return err
	}
	if !strings.HasSuffix(name, ".svg") {
		s.Service.Schedule(uploads.AreaProducts, name, data)
	}
	return nil
}

// Read returns a picture's bytes.
func (s Saving) Read(name string) ([]byte, error) {
	return s.Dir.Read(name)
}

// Remove deletes a picture, and its derivatives with it.
func (s Saving) Remove(name string) error {
	return s.Dir.Remove(name)
}
