package images_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
)

var (
	red    = color.RGBA{R: 255, A: 255}
	green  = color.RGBA{G: 255, A: 255}
	blue   = color.RGBA{B: 255, A: 255}
	yellow = color.RGBA{R: 255, G: 255, A: 255}
)

// assertNear compares a pixel to a colour with JPEG tolerance: the derived
// pictures are lossy, and a quarter of the way into a flat quadrant the error
// is small but not zero.
func assertNear(t *testing.T, img image.Image, x, y int, want color.RGBA) {
	t.Helper()
	got := color.RGBAModel.Convert(img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y)).(color.RGBA)
	const tolerance = 40
	for name, pair := range map[string][2]uint8{"R": {got.R, want.R}, "G": {got.G, want.G}, "B": {got.B, want.B}} {
		diff := int(pair[0]) - int(pair[1])
		if diff < 0 {
			diff = -diff
		}
		assert.LessOrEqualf(t, diff, tolerance, "pixel (%d,%d) channel %s: got %v, want %v", x, y, name, got, want)
	}
}

func size(d images.Derived) [2]int { return [2]int{d.Width, d.Height} }

// TestPhotoSetMakesEveryVariantFromOneSource — the whole set from a 400×200
// picture: a preview that is not scaled up, square thumbnails cut from the
// centred square and scaled down, never up, every one a JPEG because the
// source is opaque.
func TestPhotoSetMakesEveryVariantFromOneSource(t *testing.T) {
	t.Parallel()

	set, err := images.PhotoSet(encodePNG(t, quadrantImage(400, 200)))
	require.NoError(t, err)
	require.Len(t, set, len(images.PhotoVariants))

	assert.Equal(t, [2]int{400, 200}, size(set[images.VariantPreview]), "a preview is never scaled up")
	assert.Equal(t, [2]int{96, 96}, size(set[images.VariantThumb96]))
	assert.Equal(t, [2]int{192, 192}, size(set[images.VariantThumb192]))
	assert.Equal(t, [2]int{200, 200}, size(set[images.VariantThumb384]), "the centred square is 200 wide; never scaled up")
	assert.Equal(t, [2]int{200, 200}, size(set[images.VariantThumb768]))

	for v, d := range set {
		assert.Equalf(t, images.FormatJPEG, d.Format, "%s of an opaque source is a JPEG", v)
		format, err := images.DetectFormat(d.Data)
		require.NoError(t, err)
		assert.Equal(t, images.FormatJPEG, format)
	}

	// The centred square of 400×200 is x 100..300: red/blue on its left,
	// green/yellow on its right.
	thumb := decode(t, set[images.VariantThumb96].Data)
	assertNear(t, thumb, 24, 24, red)
	assertNear(t, thumb, 72, 24, green)
	assertNear(t, thumb, 24, 72, blue)
	assertNear(t, thumb, 72, 72, yellow)
}

// TestPhotoSetKeepsTransparencyAsPNG — a PNG with transparent pixels keeps
// them, as PNG; JPEG would paint them black.
func TestPhotoSetKeepsTransparencyAsPNG(t *testing.T) {
	t.Parallel()

	src := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 100; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	set, err := images.PhotoSet(encodePNG(t, src))
	require.NoError(t, err)

	for v, d := range set {
		assert.Equalf(t, images.FormatPNG, d.Format, "%s keeps the transparency", v)
	}
	thumb := decode(t, set[images.VariantThumb96].Data)
	_, _, _, leftAlpha := thumb.At(24, 48).RGBA()
	_, _, _, rightAlpha := thumb.At(72, 48).RGBA()
	assert.Equal(t, uint32(0xffff), leftAlpha)
	assert.Equal(t, uint32(0), rightAlpha)
}

// TestPhotoSetPreShrinksALargeJPEG — a source large enough to take the
// box-filter path (4800×2400 shrinks by 2 first) still lands every variant at
// its size with the right pixels in the right place.
func TestPhotoSetPreShrinksALargeJPEG(t *testing.T) {
	t.Parallel()

	const w, h = 4800, 2400
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	// Row templates, copied per row: pixel-by-pixel Set on 11.5 MP is slow.
	top := make([]byte, w*4)
	bottom := make([]byte, w*4)
	for x := 0; x < w; x++ {
		tc, bc := red, blue
		if x >= w/2 {
			tc, bc = green, yellow
		}
		copy(top[x*4:], []byte{tc.R, tc.G, tc.B, 255})
		copy(bottom[x*4:], []byte{bc.R, bc.G, bc.B, 255})
	}
	for y := 0; y < h; y++ {
		row := top
		if y >= h/2 {
			row = bottom
		}
		copy(src.Pix[y*src.Stride:], row)
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}))

	set, err := images.PhotoSet(buf.Bytes())
	require.NoError(t, err)

	assert.Equal(t, [2]int{1600, 800}, size(set[images.VariantPreview]))
	assert.Equal(t, [2]int{768, 768}, size(set[images.VariantThumb768]))
	assert.Equal(t, [2]int{96, 96}, size(set[images.VariantThumb96]))

	preview := decode(t, set[images.VariantPreview].Data)
	assertNear(t, preview, 400, 200, red)
	assertNear(t, preview, 1200, 200, green)
	assertNear(t, preview, 400, 600, blue)
	assertNear(t, preview, 1200, 600, yellow)

	thumb := decode(t, set[images.VariantThumb768].Data)
	assertNear(t, thumb, 192, 192, red)
	assertNear(t, thumb, 576, 576, yellow)
}

// TestRowSetsCropTheBoxThenItsCentredSquare — a row crop is the box the
// model drew, framed as `object-fit: cover` frames it: its largest centred
// square, scaled down to the size and never up. A box that selects nothing
// yields no crop and fails nothing.
func TestRowSetsCropTheBoxThenItsCentredSquare(t *testing.T) {
	t.Parallel()

	sets, err := images.RowSets(encodePNG(t, quadrantImage(400, 200)), map[string]images.Box{
		"0": {X: 0.5, Y: 0, Width: 0.5, Height: 1},   // the right half: green over yellow, 200×200
		"1": {X: 0, Y: 0, Width: 1, Height: 0.5},     // the top strip: red then green, 400×100
		"2": {X: 0.1, Y: 0.1, Width: 0, Height: 0.2}, // no area
		"3": {X: 2, Y: 2, Width: 0.5, Height: 0.5},   // outside the picture
	})
	require.NoError(t, err)
	require.Len(t, sets, 2, "rows whose box selects nothing have no crop")
	require.Contains(t, sets, "0")
	require.Contains(t, sets, "1")

	right := sets["0"]
	require.Len(t, right, len(images.RowVariants))
	assert.Equal(t, [2]int{192, 192}, size(right[images.VariantThumb192]))
	assert.Equal(t, [2]int{200, 200}, size(right[images.VariantThumb384]), "never scaled up")
	crop := decode(t, right[images.VariantThumb192].Data)
	assertNear(t, crop, 96, 48, green)
	assertNear(t, crop, 96, 144, yellow)

	// The top strip's centred square is x 150..250: red on the left, green on
	// the right, 100 wide — below both sizes, so served at 100.
	top := sets["1"]
	assert.Equal(t, [2]int{100, 100}, size(top[images.VariantThumb192]))
	assert.Equal(t, [2]int{100, 100}, size(top[images.VariantThumb384]))
	strip := decode(t, top[images.VariantThumb192].Data)
	assertNear(t, strip, 25, 50, red)
	assertNear(t, strip, 75, 50, green)
}

func TestRowSetsAndPhotoSetRejectUnsupportedFormats(t *testing.T) {
	t.Parallel()

	_, err := images.PhotoSet([]byte("GIF89a not really"))
	assert.ErrorIs(t, err, images.ErrUnsupportedFormat)
	_, err = images.RowSets([]byte("<svg/>"), map[string]images.Box{"0": {Width: 1, Height: 1}})
	assert.ErrorIs(t, err, images.ErrUnsupportedFormat)
}

// TestVariantNamesAreAnAllowList — a variant comes from a URL, so only the
// exact names exist; anything else is not a smaller size, it is nothing.
func TestVariantNamesAreAnAllowList(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"preview", "thumb-96", "thumb-192", "thumb-384", "thumb-768"} {
		v, ok := images.ParseVariant(name)
		assert.True(t, ok, name)
		assert.Equal(t, images.Variant(name), v)
	}
	for _, name := range []string{"", "thumb-100", "thumb-96.jpg", "Preview", "../preview", "rows", "thumb-768 "} {
		_, ok := images.ParseVariant(name)
		assert.False(t, ok, name)
	}

	for _, name := range []string{"thumb-192", "thumb-384"} {
		_, ok := images.ParseRowVariant(name)
		assert.True(t, ok, name)
	}
	for _, name := range []string{"preview", "thumb-96", "thumb-768", ""} {
		_, ok := images.ParseRowVariant(name)
		assert.False(t, ok, "%q is not a row-crop size", name)
	}

	assert.Equal(t, 384, images.VariantThumb384.Side())
	assert.Equal(t, 0, images.VariantPreview.Side())
}

// TestBoxSelectsNothingAgreesWithTheCrop — the decode-free answer matches what
// cropping would do, at any picture size.
func TestBoxSelectsNothingAgreesWithTheCrop(t *testing.T) {
	t.Parallel()

	empty := []images.Box{
		{X: 0.1, Y: 0.1, Width: 0, Height: 0.2},
		{X: 0.1, Y: 0.1, Width: 0.2, Height: -1},
		{X: 2, Y: 2, Width: 0.5, Height: 0.5},
		{X: 1, Y: 0, Width: 0.5, Height: 1},
		{X: math.NaN(), Y: 0, Width: 0.5, Height: 0.5},
	}
	for _, box := range empty {
		assert.True(t, images.BoxSelectsNothing(box), "%+v", box)
		_, err := images.ProductImage(encodePNG(t, quadrantImage(40, 20)), &box)
		assert.ErrorIs(t, err, images.ErrEmptyCrop, "%+v", box)
	}
	full := []images.Box{
		{Width: 1, Height: 1},
		{X: 0.9995, Y: 0.9995, Width: 0.0001, Height: 0.0001},
		{X: -0.5, Y: -0.5, Width: 0.6, Height: 0.6},
	}
	for _, box := range full {
		assert.False(t, images.BoxSelectsNothing(box), "%+v", box)
		_, err := images.ProductImage(encodePNG(t, quadrantImage(40, 20)), &box)
		assert.NoError(t, err, "%+v", box)
	}
}
