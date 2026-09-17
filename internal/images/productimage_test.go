package images_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
)

// quadrantImage is a w×h PNG-safe image whose four quadrants are four distinct
// colours, so a crop can be identified by the colour it comes back as.
func quadrantImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.RGBA
			switch {
			case x < w/2 && y < h/2:
				c = color.RGBA{R: 255, A: 255} // top-left: red
			case x >= w/2 && y < h/2:
				c = color.RGBA{G: 255, A: 255} // top-right: green
			case x < w/2:
				c = color.RGBA{B: 255, A: 255} // bottom-left: blue
			default:
				c = color.RGBA{R: 255, G: 255, A: 255} // bottom-right: yellow
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	return img
}

func TestProductImageWithNoBoxKeepsTheWholePhoto(t *testing.T) {
	t.Parallel()

	result, err := images.ProductImage(encodePNG(t, quadrantImage(40, 20)), nil)
	require.NoError(t, err)

	assert.Equal(t, images.FormatPNG, result.Format, "the source format is kept")
	img := decode(t, result.Data)
	assert.Equal(t, 40, img.Bounds().Dx())
	assert.Equal(t, 20, img.Bounds().Dy())
}

func TestProductImageCropsTheRegionTheBoxSelects(t *testing.T) {
	t.Parallel()

	// The bottom-right quadrant of a 40×20 image, in the normalized form the
	// vision model reports.
	box := &images.Box{X: 0.5, Y: 0.5, Width: 0.5, Height: 0.5}

	result, err := images.ProductImage(encodePNG(t, quadrantImage(40, 20)), box)
	require.NoError(t, err)

	img := decode(t, result.Data)
	require.Equal(t, 20, img.Bounds().Dx())
	require.Equal(t, 10, img.Bounds().Dy())

	// Every pixel is the bottom-right quadrant's yellow, not a neighbour's
	// colour: the crop took the right region, not merely the right size.
	for _, p := range []image.Point{{0, 0}, {19, 0}, {0, 9}, {19, 9}, {10, 5}} {
		r, g, b, _ := img.At(img.Bounds().Min.X+p.X, img.Bounds().Min.Y+p.Y).RGBA()
		assert.Equal(t, [3]uint32{0xffff, 0xffff, 0}, [3]uint32{r, g, b}, "pixel %v", p)
	}
}

func TestProductImageCropsAfterApplyingOrientation(t *testing.T) {
	t.Parallel()

	// A 4×2 landscape frame that EXIF orientation 6 says to rotate 90° into a
	// 2×4 portrait. The model looked at the portrait, so a box over its top
	// half must come back 2×2 — cropping the unrotated landscape pixels would
	// return a 2×1 strip of the wrong part of the photo.
	original := jpegWithOrientation(t, cornerImage(4, 2), 6)
	box := &images.Box{X: 0, Y: 0, Width: 1, Height: 0.5}

	result, err := images.ProductImage(original, box)
	require.NoError(t, err)

	cfg, err := jpeg.DecodeConfig(bytes.NewReader(result.Data))
	require.NoError(t, err)
	assert.Equal(t, 2, cfg.Width, "crop width is taken from the upright image")
	assert.Equal(t, 2, cfg.Height, "crop height is taken from the upright image")
	assert.False(t, containsEXIF(result.Data), "no metadata may survive into a product image")
}

func TestProductImageNeverCarriesMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"jpeg", jpegWithOrientation(t, cornerImage(8, 8), 1)},
		{"png", pngWithOrientation(t, cornerImage(8, 8), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.True(t, containsEXIF(tc.data), "the fixture must actually carry EXIF")

			for _, box := range []*images.Box{nil, {X: 0.25, Y: 0.25, Width: 0.5, Height: 0.5}} {
				result, err := images.ProductImage(tc.data, box)
				require.NoError(t, err)
				assert.False(t, containsEXIF(result.Data))
			}
		})
	}
}

func TestProductImageClampsABoxThatOverhangsTheFrame(t *testing.T) {
	t.Parallel()

	// Starts left of the image and runs past its right edge: what is inside
	// the frame is the whole width, top half.
	box := &images.Box{X: -0.1, Y: 0, Width: 1.3, Height: 0.5}

	result, err := images.ProductImage(encodePNG(t, quadrantImage(40, 20)), box)
	require.NoError(t, err)

	img := decode(t, result.Data)
	assert.Equal(t, 40, img.Bounds().Dx())
	assert.Equal(t, 10, img.Bounds().Dy())
}

func TestProductImageRefusesABoxThatSelectsNothing(t *testing.T) {
	t.Parallel()

	photo := encodePNG(t, quadrantImage(40, 20))
	for name, box := range map[string]images.Box{
		"entirely outside": {X: 1.5, Y: 0, Width: 0.2, Height: 0.2},
		"zero width":       {X: 0.2, Y: 0.2, Width: 0, Height: 0.5},
		"negative size":    {X: 0.5, Y: 0.5, Width: -0.3, Height: -0.3},
		"not a number":     {X: math.NaN(), Y: 0, Width: 0.5, Height: 0.5},
		"infinite":         {X: 0, Y: 0, Width: math.Inf(1), Height: 0.5},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := images.ProductImage(photo, &box)
			assert.ErrorIs(t, err, images.ErrEmptyCrop)
		})
	}
}

func TestProductImageScalesALargePhotoDown(t *testing.T) {
	t.Parallel()

	result, err := images.ProductImage(encodePNG(t, quadrantImage(2048, 1024)), nil)
	require.NoError(t, err)

	img := decode(t, result.Data)
	assert.Equal(t, images.MaxProductImageSide, img.Bounds().Dx(), "longer side capped")
	assert.Equal(t, images.MaxProductImageSide/2, img.Bounds().Dy(), "aspect ratio kept")
}

func TestProductImageNeverScalesUp(t *testing.T) {
	t.Parallel()

	result, err := images.ProductImage(encodePNG(t, quadrantImage(16, 8)), nil)
	require.NoError(t, err)

	img := decode(t, result.Data)
	assert.Equal(t, 16, img.Bounds().Dx())
	assert.Equal(t, 8, img.Bounds().Dy())
}

func TestProductImageRejectsUnsupportedFormats(t *testing.T) {
	t.Parallel()

	_, err := images.ProductImage([]byte("GIF89a not really"), nil)
	assert.ErrorIs(t, err, images.ErrUnsupportedFormat)
}
