// Package derive makes and serves the derived forms of stored pictures — the
// thumbnails, previews and per-item crops of docs/specs/43-image-derivatives.md
// — so that no screen ever downloads a picture larger than it shows.
//
// One rule, two paths. Every write of a source schedules its derivatives
// (the eager path: inside a photo job, or after a product picture is saved),
// and every read of a missing derivative makes it (the lazy path, behind the
// serving handlers). Eager is the normal case; lazy is what makes a restored
// backup, a wiped cache, a crash between the two, or a deployment onto
// existing pictures invisible — the first request simply pays for the decode.
//
// The unit of work is a whole set, not one file: a decode of a 50 MP photo is
// the cost, so every variant it can serve is written from that one decode,
// and a singleflight group per source means twenty first-time requests for
// one job's crops decode it once. Two lanes of one slot each — eager and lazy
// — bound the work to two decodes at a time and keep a request from ever
// waiting behind a background job.
package derive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/uploads"
)

// Lane is which of the two single-slot workers a derivation runs on.
type Lane int

// The lanes.
const (
	// LaneEager is background work: photo jobs, the start-up warm-up, a saved
	// product picture.
	LaneEager Lane = iota
	// LaneLazy is a request that found its variant missing. Its own lane, so
	// a 70 ms product thumbnail never queues behind a 2 s shelf photo.
	LaneLazy
)

// lazyTimeout bounds a derivation a request started. Detached from the
// request's own context: a client that gives up should not waste the decode
// it caused, since the next request would only start it again.
const lazyTimeout = 30 * time.Second

// scheduledTimeout bounds a derivation started by Schedule.
const scheduledTimeout = 2 * time.Minute

// sweepInterval is how often orphaned derivatives are cleared.
const sweepInterval = time.Hour

// Loader supplies a source's bytes when a derivation actually needs them —
// so a job that has them in hand passes them along, and a handler reads them
// only on a miss.
type Loader func() ([]byte, error)

// Bytes is a Loader for bytes already in hand.
func Bytes(data []byte) Loader {
	return func() ([]byte, error) { return data, nil }
}

// Service makes and serves derivatives.
type Service struct {
	store *uploads.Derived
	lanes [2]chan struct{}
	photo singleflight.Group
	rows  singleflight.Group
	log   *slog.Logger
	wg    sync.WaitGroup
}

// New wires the service to its cache.
func New(store *uploads.Derived, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: store,
		lanes: [2]chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)},
		log:   log,
	}
}

// fileName is the file a whole-picture variant is kept as.
func fileName(v images.Variant, f images.Format) string {
	return string(v) + f.Extension()
}

// rowFileName is the file a row crop is kept as.
func rowFileName(rowID string, v images.Variant, f images.Format) string {
	return "rows-" + rowID + "-" + string(v) + f.Extension()
}

// read returns a derivative stored under either encoding, and its content
// type. Missing in both is os.ErrNotExist.
func (s *Service) read(area uploads.Area, stem, base string) ([]byte, string, error) {
	for _, f := range []images.Format{images.FormatJPEG, images.FormatPNG} {
		data, err := s.store.Read(area, stem, base+f.Extension())
		if err == nil {
			return data, f.ContentType(), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	return nil, "", os.ErrNotExist
}

func (s *Service) exists(area uploads.Area, stem, base string) bool {
	for _, f := range []images.Format{images.FormatJPEG, images.FormatPNG} {
		if s.store.Exists(area, stem, base+f.Extension()) {
			return true
		}
	}
	return false
}

// hasPhotoSet reports whether every whole-picture variant of a source is on
// disk. Any one missing means the set is made again as a whole — cheap, and
// simpler than reasoning about a partial set.
func (s *Service) hasPhotoSet(area uploads.Area, stem string) bool {
	for _, v := range images.PhotoVariants {
		if !s.exists(area, stem, string(v)) {
			return false
		}
	}
	return true
}

// hasRowSet reports whether every crop of every box is on disk. A box that
// selects nothing yields no file and is not counted as missing — otherwise
// one degenerate box would make every request for the set decode the photo
// again.
func (s *Service) hasRowSet(area uploads.Area, stem string, boxes map[string]images.Box) bool {
	for id, box := range boxes {
		if images.BoxSelectsNothing(box) {
			continue
		}
		for _, v := range images.RowVariants {
			if !s.exists(area, stem, "rows-"+id+"-"+string(v)) {
				return false
			}
		}
	}
	return true
}

// acquire takes a lane's single slot, or gives up with the context.
func (s *Service) acquire(ctx context.Context, lane Lane) (release func(), err error) {
	select {
	case s.lanes[lane] <- struct{}{}:
		return func() { <-s.lanes[lane] }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// EnsurePhotoSet makes every whole-picture variant of a source unless all
// exist. Concurrent calls for one source share a single run.
func (s *Service) EnsurePhotoSet(ctx context.Context, lane Lane, area uploads.Area, source string, load Loader) error {
	stem := uploads.Stem(source)
	key := string(area) + "/" + stem
	ch := s.photo.DoChan(key, func() (any, error) {
		return nil, s.makePhotoSet(ctx, lane, area, stem, load)
	})
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) makePhotoSet(ctx context.Context, lane Lane, area uploads.Area, stem string, load Loader) error {
	if s.hasPhotoSet(area, stem) {
		return nil
	}
	release, err := s.acquire(ctx, lane)
	if err != nil {
		return err
	}
	defer release()
	// The slot may have been waited for; the set may have been made meanwhile.
	if s.hasPhotoSet(area, stem) {
		return nil
	}

	data, err := load()
	if err != nil {
		return err
	}
	set, err := images.PhotoSet(data)
	if err != nil {
		return fmt.Errorf("derive: %s/%s: %w", area, stem, err)
	}
	for v, d := range set {
		if err := s.store.Write(area, stem, fileName(v, d.Format), d.Data); err != nil {
			return err
		}
	}
	return nil
}

// boxesKey identifies one proposal's boxes, so two analyses of the same photo
// never share a run: a singleflight keyed on the source alone would hand a
// re-analysis the crops of the proposal it replaced.
func boxesKey(boxes map[string]images.Box) string {
	ids := make([]string, 0, len(boxes))
	for id := range boxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		b, _ := json.Marshal(boxes[id])
		h.Write([]byte(id))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// EnsureRowSet makes the crops of every box unless all exist.
func (s *Service) EnsureRowSet(ctx context.Context, lane Lane, area uploads.Area, source string, load Loader, boxes map[string]images.Box) error {
	if len(boxes) == 0 {
		return nil
	}
	stem := uploads.Stem(source)
	key := string(area) + "/" + stem + "/" + boxesKey(boxes)
	ch := s.rows.DoChan(key, func() (any, error) {
		return nil, s.makeRowSet(ctx, lane, area, stem, load, boxes)
	})
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) makeRowSet(ctx context.Context, lane Lane, area uploads.Area, stem string, load Loader, boxes map[string]images.Box) error {
	if s.hasRowSet(area, stem, boxes) {
		return nil
	}
	release, err := s.acquire(ctx, lane)
	if err != nil {
		return err
	}
	defer release()
	if s.hasRowSet(area, stem, boxes) {
		return nil
	}

	data, err := load()
	if err != nil {
		return err
	}
	sets, err := images.RowSets(data, boxes)
	if err != nil {
		return fmt.Errorf("derive: %s/%s rows: %w", area, stem, err)
	}
	for id, set := range sets {
		for v, d := range set {
			if err := s.store.Write(area, stem, rowFileName(id, v, d.Format), d.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReplaceRowSet drops a source's row crops and makes the ones boxes asks for:
// what a job does after its photo was analysed again, so no crop of the old
// proposal survives beside the new one.
func (s *Service) ReplaceRowSet(ctx context.Context, lane Lane, area uploads.Area, source string, load Loader, boxes map[string]images.Box) error {
	if err := s.store.RemoveRows(area, uploads.Stem(source)); err != nil {
		return err
	}
	return s.EnsureRowSet(ctx, lane, area, source, load, boxes)
}

// detached is the context a lazy derivation runs under: the request's values
// (its request id, for the log) without its cancellation, bounded by
// lazyTimeout.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), lazyTimeout)
}

// Open returns a whole-picture variant of a source, making the set first if
// the variant is missing. The caller has already decided the caller may see
// the source; nothing here checks that again.
//
// A derivation runs under a detached context, so a request that is cancelled
// while waiting neither stops the work nor abandons it: the result still
// arrives, the file is kept, and the next request finds it.
func (s *Service) Open(ctx context.Context, area uploads.Area, source string, v images.Variant, load Loader) ([]byte, string, error) {
	stem := uploads.Stem(source)
	data, contentType, err := s.read(area, stem, string(v))
	if !errors.Is(err, os.ErrNotExist) {
		return data, contentType, err
	}

	work, cancel := detached(ctx)
	defer cancel()
	if err := s.EnsurePhotoSet(work, LaneLazy, area, source, load); err != nil {
		return nil, "", err
	}
	return s.read(area, stem, string(v))
}

// OpenRow returns one row's crop, making the row set first if it is missing.
// boxes is the job's current proposal; a row it does not name, or whose box
// selects nothing, is os.ErrNotExist — not a reason to decode anything.
func (s *Service) OpenRow(ctx context.Context, area uploads.Area, source, rowID string, v images.Variant, load Loader, boxes map[string]images.Box) ([]byte, string, error) {
	box, ok := boxes[rowID]
	if !ok || images.BoxSelectsNothing(box) {
		return nil, "", os.ErrNotExist
	}
	stem := uploads.Stem(source)
	base := "rows-" + rowID + "-" + string(v)
	data, contentType, err := s.read(area, stem, base)
	if !errors.Is(err, os.ErrNotExist) {
		return data, contentType, err
	}

	work, cancel := detached(ctx)
	defer cancel()
	if err := s.EnsureRowSet(work, LaneLazy, area, source, load, boxes); err != nil {
		return nil, "", err
	}
	return s.read(area, stem, base)
}

// Schedule makes a source's whole-picture set in the background, on the eager
// lane. For a picture just written to permanent storage: the first list that
// shows it should find its thumbnails ready. A failure is logged; the lazy
// path covers the request that finds the set missing.
func (s *Service) Schedule(area uploads.Area, source string, data []byte) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), scheduledTimeout)
		defer cancel()
		if err := s.EnsurePhotoSet(ctx, LaneEager, area, source, Bytes(data)); err != nil {
			s.log.Warn("deriving a saved picture's thumbnails failed",
				slog.String("area", string(area)), slog.String("source", source), slog.Any("err", err))
		}
	}()
}

// Wait blocks until every Schedule has finished. For shutdown and tests.
func (s *Service) Wait() {
	s.wg.Wait()
}

// Warm makes the whole-picture set of every source in dir that lacks one, on
// the eager lane, one after another, until ctx ends. It returns how many it
// made. Errors are logged per source and do not stop the rest.
func (s *Service) Warm(ctx context.Context, area uploads.Area, dir *uploads.Dir) (int, error) {
	names, err := dir.List()
	if err != nil {
		return 0, err
	}
	made := 0
	for _, name := range names {
		if ctx.Err() != nil {
			return made, ctx.Err()
		}
		// An SVG has no derivatives: it is served as it is at every size.
		if strings.HasSuffix(name, ".svg") || s.hasPhotoSet(area, uploads.Stem(name)) {
			continue
		}
		name := name
		err := s.EnsurePhotoSet(ctx, LaneEager, area, name, func() ([]byte, error) { return dir.Read(name) })
		if err != nil {
			if ctx.Err() != nil {
				return made, ctx.Err()
			}
			s.log.Warn("warming a picture's thumbnails failed",
				slog.String("area", string(area)), slog.String("source", name), slog.Any("err", err))
			continue
		}
		made++
	}
	return made, nil
}

// SweepOrphans removes the derivatives of every source that is no longer in
// dir: what a crash between removing a source and its derivatives leaves, or
// a restored backup whose uploads are older than the cache beside them.
func (s *Service) SweepOrphans(area uploads.Area, dir *uploads.Dir) (int, error) {
	stems, err := s.store.Stems(area)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, stem := range stems {
		if dir.Exists(stem+".jpg") || dir.Exists(stem+".png") || dir.Exists(stem+".svg") {
			continue
		}
		if err := s.store.RemoveSource(area, stem); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// RunMaintenance is the background loop: an orphan sweep of both areas at
// start and every sweepInterval, and once at start a warm-up of the product
// pictures, so the first inventory table after a deployment onto existing
// pictures does not trickle in. Ingest photos are not warmed: most on disk
// belong to consumed jobs nobody will open again, and a job makes its own.
// Either dir may be nil when its volume was unusable.
func (s *Service) RunMaintenance(ctx context.Context, products, ingest *uploads.Dir) {
	sweep := func() {
		for _, a := range []struct {
			area uploads.Area
			dir  *uploads.Dir
		}{{uploads.AreaProducts, products}, {uploads.AreaIngest, ingest}} {
			if a.dir == nil {
				continue
			}
			n, err := s.SweepOrphans(a.area, a.dir)
			if err != nil {
				s.log.Warn("sweep failed", slog.String("table", "derived pictures"), slog.String("area", string(a.area)), slog.Any("err", err))
			} else if n > 0 {
				s.log.Info("sweep", slog.String("table", "derived pictures"), slog.String("area", string(a.area)), slog.Int("removed", n))
			}
		}
	}

	sweep()
	if products != nil {
		if n, err := s.Warm(ctx, uploads.AreaProducts, products); err != nil && ctx.Err() == nil {
			s.log.Warn("warming product thumbnails failed", slog.Any("err", err))
		} else if n > 0 {
			s.log.Info("warmed product thumbnails", slog.Int("pictures", n))
		}
	}

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
