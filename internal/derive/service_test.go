package derive_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/derive"
	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/uploads"
)

const (
	sourceA = "0192a5f0-1111-7000-8000-000000000001.png"
	sourceB = "0192a5f0-2222-7000-8000-000000000002.png"
)

// photoPNG is a 400×200 picture, red on the left and green on the right.
func photoPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			c := color.RGBA{R: 255, A: 255}
			if x >= 200 {
				c = color.RGBA{G: 255, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

type fixture struct {
	svc   *derive.Service
	store *uploads.Derived
	dir   *uploads.Dir
	loads atomic.Int32
	photo []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	store, err := uploads.NewDerived(filepath.Join(root, "derived"))
	require.NoError(t, err)
	dir, err := uploads.NewDir(filepath.Join(root, "ingest"))
	require.NoError(t, err)
	dir.RemoveDerivedWith(store, uploads.AreaIngest)
	return &fixture{svc: derive.New(store, nil), store: store, dir: dir, photo: photoPNG(t)}
}

// load counts how often the source is actually read — a decode follows every
// read, so this is the number of decodes.
func (f *fixture) load() ([]byte, error) {
	f.loads.Add(1)
	time.Sleep(5 * time.Millisecond) // long enough for concurrent callers to overlap
	return f.photo, nil
}

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	return cfg.Width, cfg.Height
}

// TestOpenMakesAMissingVariantOnceAndKeepsIt — the lazy path: the first
// request for any variant makes the whole set from one read; every later
// request, for any variant, is a file read.
func TestOpenMakesAMissingVariantOnceAndKeepsIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	data, contentType, err := f.svc.Open(ctx, uploads.AreaIngest, sourceA, images.VariantThumb96, f.load)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", contentType)
	w, h := decodeSize(t, data)
	assert.Equal(t, [2]int{96, 96}, [2]int{w, h})
	assert.Equal(t, int32(1), f.loads.Load())

	data, contentType, err = f.svc.Open(ctx, uploads.AreaIngest, sourceA, images.VariantPreview, f.load)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", contentType)
	w, h = decodeSize(t, data)
	assert.Equal(t, [2]int{400, 200}, [2]int{w, h}, "a preview is never scaled up")
	assert.Equal(t, int32(1), f.loads.Load(), "the second variant came from the set the first request made")

	for _, v := range images.PhotoVariants {
		assert.True(t, f.store.Exists(uploads.AreaIngest, uploads.Stem(sourceA), string(v)+".jpg"), v)
	}
}

// TestConcurrentFirstRequestsDecodeTheSourceOnce — twenty requests for one
// source at the same moment share one decode.
func TestConcurrentFirstRequestsDecodeTheSourceOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		v := images.PhotoVariants[i%len(images.PhotoVariants)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := f.svc.Open(context.Background(), uploads.AreaIngest, sourceA, v, f.load)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), f.loads.Load())
}

// TestOpenRowUsesTheProposalsBoxesAndRefusesOthers — a row crop exists only
// for a row the job's current proposal names with a box that selects pixels;
// anything else is not found, and costs no decode.
func TestOpenRowUsesTheProposalsBoxesAndRefusesOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	boxes := map[string]images.Box{
		"0": {X: 0.5, Y: 0, Width: 0.5, Height: 1}, // the green half
		"2": {X: 0.1, Y: 0.1, Width: 0, Height: 0}, // selects nothing
	}

	_, _, err := f.svc.OpenRow(ctx, uploads.AreaIngest, sourceA, "7", images.VariantThumb192, f.load, boxes)
	assert.ErrorIs(t, err, os.ErrNotExist, "not a row of the proposal")
	assert.Equal(t, int32(0), f.loads.Load())

	data, contentType, err := f.svc.OpenRow(ctx, uploads.AreaIngest, sourceA, "0", images.VariantThumb192, f.load, boxes)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", contentType)
	w, h := decodeSize(t, data)
	assert.Equal(t, [2]int{192, 192}, [2]int{w, h})
	img, _, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	r, g, _, _ := img.At(96, 96).RGBA()
	assert.Less(t, r>>8, uint32(60), "the crop is the green half")
	assert.Greater(t, g>>8, uint32(200))

	_, _, err = f.svc.OpenRow(ctx, uploads.AreaIngest, sourceA, "2", images.VariantThumb384, f.load, boxes)
	assert.ErrorIs(t, err, os.ErrNotExist, "a box that selects nothing has no crop")
	assert.Equal(t, int32(1), f.loads.Load(), "one decode made every row the proposal has")
}

// TestReplaceRowSetDropsTheOldProposalsCrops — after "Analyze again", no crop
// of the previous proposal survives.
func TestReplaceRowSetDropsTheOldProposalsCrops(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	stem := uploads.Stem(sourceA)
	whole := images.Box{Width: 1, Height: 1}

	require.NoError(t, f.svc.EnsureRowSet(ctx, derive.LaneEager, uploads.AreaIngest, sourceA, f.load,
		map[string]images.Box{"0": whole, "1": whole}))
	require.NoError(t, f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaIngest, sourceA, f.load))
	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "rows-1-thumb-192.jpg"))

	require.NoError(t, f.svc.ReplaceRowSet(ctx, derive.LaneEager, uploads.AreaIngest, sourceA, f.load,
		map[string]images.Box{"0": whole}))

	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "rows-0-thumb-192.jpg"))
	assert.False(t, f.store.Exists(uploads.AreaIngest, stem, "rows-1-thumb-192.jpg"))
	assert.False(t, f.store.Exists(uploads.AreaIngest, stem, "rows-1-thumb-384.jpg"))
	assert.True(t, f.store.Exists(uploads.AreaIngest, stem, "thumb-96.jpg"), "the whole-picture set stays")
}

// TestEnsurePhotoSetSkipsASetThatExists — the eager path on a source that
// already has its set reads nothing.
func TestEnsurePhotoSetSkipsASetThatExists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	require.NoError(t, f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaProducts, sourceA, f.load))
	require.NoError(t, f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaProducts, sourceA, f.load))
	assert.Equal(t, int32(1), f.loads.Load())

	// One missing file means the whole set is made again.
	require.NoError(t, f.store.RemoveSource(uploads.AreaProducts, uploads.Stem(sourceA)))
	require.NoError(t, f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaProducts, sourceA, f.load))
	assert.Equal(t, int32(2), f.loads.Load())
}

// TestLoaderAndDecodeFailuresAreReported — a source that cannot be read or is
// not a picture is an error to the caller, and nothing is written.
func TestLoaderAndDecodeFailuresAreReported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	boom := errors.New("disk gone")
	err := f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaIngest, sourceA, func() ([]byte, error) { return nil, boom })
	assert.ErrorIs(t, err, boom)

	_, _, err = f.svc.Open(ctx, uploads.AreaIngest, sourceB, images.VariantThumb96, derive.Bytes([]byte("not a picture")))
	assert.ErrorIs(t, err, images.ErrUnsupportedFormat)
	stems, err := f.store.Stems(uploads.AreaIngest)
	require.NoError(t, err)
	assert.Empty(t, stems)
}

// TestScheduleMakesTheSetInTheBackground — a saved product picture's set
// arrives without the saver waiting for it.
func TestScheduleMakesTheSetInTheBackground(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.svc.Schedule(uploads.AreaProducts, sourceA, f.photo)
	f.svc.Wait()

	assert.True(t, f.store.Exists(uploads.AreaProducts, uploads.Stem(sourceA), "thumb-768.jpg"))
}

// TestWarmMakesMissingSetsForEveryFileInADirectory — the start-up pass over
// existing product pictures, idempotent, skipping SVGs.
func TestWarmMakesMissingSetsForEveryFileInADirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	pictures, err := uploads.NewPictureDir(filepath.Join(t.TempDir(), "products"))
	require.NoError(t, err)
	require.NoError(t, pictures.Save(sourceA, f.photo))
	require.NoError(t, pictures.Save(sourceB, f.photo))
	svgName := "0192a5f0-3333-7000-8000-000000000003.svg"
	require.NoError(t, pictures.Save(svgName, []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")))

	made, err := f.svc.Warm(ctx, uploads.AreaProducts, pictures)
	require.NoError(t, err)
	assert.Equal(t, 2, made)
	assert.True(t, f.store.Exists(uploads.AreaProducts, uploads.Stem(sourceA), "thumb-96.jpg"))
	assert.True(t, f.store.Exists(uploads.AreaProducts, uploads.Stem(sourceB), "thumb-96.jpg"))
	assert.False(t, f.store.Exists(uploads.AreaProducts, uploads.Stem(svgName), "thumb-96.jpg"))

	made, err = f.svc.Warm(ctx, uploads.AreaProducts, pictures)
	require.NoError(t, err)
	assert.Equal(t, 0, made, "nothing left to make")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, f.store.RemoveSource(uploads.AreaProducts, uploads.Stem(sourceA)))
	_, err = f.svc.Warm(cancelled, uploads.AreaProducts, pictures)
	assert.ErrorIs(t, err, context.Canceled, "a warm-up stops with the process")
}

// TestSweepOrphansRemovesDerivativesWhoseSourceIsGone — and only those.
func TestSweepOrphansRemovesDerivativesWhoseSourceIsGone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	require.NoError(t, f.dir.Save(sourceA, f.photo))
	require.NoError(t, f.svc.EnsurePhotoSet(ctx, derive.LaneEager, uploads.AreaIngest, sourceA, f.load))
	// Derivatives of a source that is not in the directory at all — what a
	// restore of an older uploads tree leaves behind.
	require.NoError(t, f.store.Write(uploads.AreaIngest, uploads.Stem(sourceB), "thumb-96.jpg", []byte("x")))

	removed, err := f.svc.SweepOrphans(uploads.AreaIngest, f.dir)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.True(t, f.store.Exists(uploads.AreaIngest, uploads.Stem(sourceA), "thumb-96.jpg"))
	assert.False(t, f.store.Exists(uploads.AreaIngest, uploads.Stem(sourceB), "thumb-96.jpg"))
}

// TestALazyDerivationOutlivesTheRequestThatStartedIt — a client that gives up
// does not waste the decode: the work runs detached from the request, so the
// set is written and the variant is even still returned.
func TestALazyDerivationOutlivesTheRequestThatStartedIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data, _, err := f.svc.Open(ctx, uploads.AreaIngest, sourceA, images.VariantThumb96, f.load)
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.True(t, f.store.Exists(uploads.AreaIngest, uploads.Stem(sourceA), "thumb-96.jpg"))
}
