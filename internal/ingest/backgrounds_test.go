package ingest

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/vision"
)

type fakeSegmenter struct {
	segmentation *vision.Segmentation
	err          error
	gotModel     string
	gotMime      string
	gotImage     []byte
}

func (f *fakeSegmenter) Segment(_ context.Context, model string, image []byte, mime string) (*vision.Segmentation, error) {
	f.gotModel, f.gotImage, f.gotMime = model, image, mime
	return f.segmentation, f.err
}

type fakeImageModels struct {
	offered     map[string]bool
	invalidated int
}

func (f *fakeImageModels) Offers(_ context.Context, model string) bool { return f.offered[model] }
func (f *fakeImageModels) Invalidate()                                 { f.invalidated++ }

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestBackgroundsAreOfferedOnlyWithAListedModel(t *testing.T) {
	t.Parallel()

	models := &fakeImageModels{offered: map[string]bool{"gemini-2.5-flash": true}}

	model, ok := NewBackgrounds(&fakeSegmenter{}, models, "gemini-2.5-flash").Available(context.Background())
	assert.True(t, ok)
	assert.Equal(t, "gemini-2.5-flash", model)

	model, ok = NewBackgrounds(&fakeSegmenter{}, models, "gemini-1.0-pro-vision").Available(context.Background())
	assert.False(t, ok, "configured but deprecated away")
	assert.Equal(t, "gemini-1.0-pro-vision", model, "named, for the 503 and the admin banner")

	_, ok = NewBackgrounds(&fakeSegmenter{}, models, "").Available(context.Background())
	assert.False(t, ok, "unset is never offered")
}

// TestBackgroundsCutAlongTheModelsOutline — the picture goes to the image
// model, and the cutout is made here from the box and mask it returns.
func TestBackgroundsCutAlongTheModelsOutline(t *testing.T) {
	t.Parallel()

	segmenter := &fakeSegmenter{segmentation: &vision.Segmentation{
		Box:  vision.Box{X: 0, Y: 0, Width: 0.5, Height: 1},
		Mask: solidPNG(t, 4, 4, color.Gray{Y: 255}),
	}}
	b := NewBackgrounds(segmenter, &fakeImageModels{}, "gemini-2.5-flash")
	picture := solidPNG(t, 20, 10, color.RGBA{R: 200, A: 255})

	got, err := b.Remove(context.Background(), picture, images.FormatPNG)
	require.NoError(t, err)

	assert.Equal(t, "gemini-2.5-flash", segmenter.gotModel)
	assert.Equal(t, "image/png", segmenter.gotMime)
	assert.Equal(t, picture, segmenter.gotImage)

	assert.Equal(t, images.FormatPNG, got.Format)
	cut, _, err := image.Decode(bytes.NewReader(got.Data))
	require.NoError(t, err)
	assert.Equal(t, 10, cut.Bounds().Dx(), "cut to the model's box")
}

func TestBackgroundsFailWithoutAUsableOutline(t *testing.T) {
	t.Parallel()

	picture := solidPNG(t, 8, 8, color.White)

	t.Run("model withdrawn drops the cached list", func(t *testing.T) {
		t.Parallel()
		models := &fakeImageModels{}
		b := NewBackgrounds(&fakeSegmenter{err: vision.ErrModelNotFound}, models, "gemini-gone")
		_, err := b.Remove(context.Background(), picture, images.FormatPNG)
		assert.ErrorIs(t, err, vision.ErrModelNotFound)
		assert.Equal(t, 1, models.invalidated)
	})

	t.Run("outage", func(t *testing.T) {
		t.Parallel()
		models := &fakeImageModels{}
		b := NewBackgrounds(&fakeSegmenter{err: errors.New("generateContent returned 500")}, models, "m")
		_, err := b.Remove(context.Background(), picture, images.FormatPNG)
		assert.Error(t, err)
		assert.Zero(t, models.invalidated, "an outage is not a withdrawn model")
	})

	t.Run("unusable mask", func(t *testing.T) {
		t.Parallel()
		b := NewBackgrounds(&fakeSegmenter{segmentation: &vision.Segmentation{
			Box: vision.Box{Width: 1, Height: 1}, Mask: []byte("not a png"),
		}}, &fakeImageModels{}, "m")
		_, err := b.Remove(context.Background(), picture, images.FormatPNG)
		assert.ErrorIs(t, err, images.ErrUnusableMask)
	})
}
