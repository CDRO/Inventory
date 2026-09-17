package images_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
)

// halfMask is a w×h mask that marks the top half as subject (value top) and
// the bottom half as background (value bottom).
func halfMask(w, h int, top, bottom uint8) *image.Gray {
	m := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := bottom
			if y < h/2 {
				v = top
			}
			m.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return m
}

func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y)).(color.NRGBA)
}

// TestCutoutKeepsTheSubjectAndClearsTheBackground — the box selects the
// region, the mask (at its own, smaller size) is stretched over it, and what
// the mask calls background becomes transparent while the subject keeps its
// own pixels.
func TestCutoutKeepsTheSubjectAndClearsTheBackground(t *testing.T) {
	t.Parallel()

	// The right half of the quadrants: green on top, yellow below.
	box := images.Box{X: 0.5, Y: 0, Width: 0.5, Height: 1}
	result, err := images.Cutout(encodePNG(t, quadrantImage(40, 40)), box, encodePNG(t, halfMask(5, 10, 255, 0)))
	require.NoError(t, err)

	assert.Equal(t, images.FormatPNG, result.Format)
	img := decode(t, result.Data)
	require.Equal(t, 20, img.Bounds().Dx(), "cut to the box")
	require.Equal(t, 40, img.Bounds().Dy())

	top := nrgbaAt(img, 10, 5)
	assert.Equal(t, color.NRGBA{G: 255, A: 255}, top, "the subject keeps the photo's own pixels")
	bottom := nrgbaAt(img, 10, 35)
	assert.Equal(t, uint8(0), bottom.A, "the background is transparent")
}

// TestCutoutFromAJPEGIsATransparentPNG — a JPEG has no alpha channel, so the
// cutout of a JPEG photo must come back as a PNG or the removed background
// would turn black.
func TestCutoutFromAJPEGIsATransparentPNG(t *testing.T) {
	t.Parallel()

	var photo bytes.Buffer
	require.NoError(t, jpeg.Encode(&photo, quadrantImage(40, 40), &jpeg.Options{Quality: 95}))

	result, err := images.Cutout(photo.Bytes(), images.Box{Width: 1, Height: 1}, encodePNG(t, halfMask(8, 8, 255, 0)))
	require.NoError(t, err)

	assert.Equal(t, images.FormatPNG, result.Format)
	format, err := images.DetectFormat(result.Data)
	require.NoError(t, err)
	assert.Equal(t, images.FormatPNG, format)
	assert.Equal(t, uint8(0), nrgbaAt(decode(t, result.Data), 5, 35).A)
}

// TestCutoutFadesTheUncertainEdge — a mask value between clearly background
// and clearly subject is partly transparent, not snapped either way.
func TestCutoutFadesTheUncertainEdge(t *testing.T) {
	t.Parallel()

	result, err := images.Cutout(encodePNG(t, quadrantImage(20, 20)), images.Box{Width: 1, Height: 1},
		encodePNG(t, halfMask(20, 20, 128, 128)))
	require.NoError(t, err)

	alpha := nrgbaAt(decode(t, result.Data), 2, 2).A
	assert.Greater(t, alpha, uint8(0))
	assert.Less(t, alpha, uint8(255))
}

func TestCutoutRefusesAMaskItCannotUse(t *testing.T) {
	t.Parallel()

	photo := encodePNG(t, quadrantImage(20, 20))
	whole := images.Box{Width: 1, Height: 1}

	_, err := images.Cutout(photo, whole, []byte("not an image"))
	assert.ErrorIs(t, err, images.ErrUnusableMask)

	_, err = images.Cutout(photo, whole, encodePNG(t, halfMask(10, 10, 0, 0)))
	assert.ErrorIs(t, err, images.ErrUnusableMask, "a mask that keeps nothing is no picture at all")

	_, err = images.Cutout(photo, images.Box{X: 2, Y: 2, Width: 0.5, Height: 0.5}, encodePNG(t, halfMask(10, 10, 255, 255)))
	assert.ErrorIs(t, err, images.ErrEmptyCrop, "a box outside the picture")
}
