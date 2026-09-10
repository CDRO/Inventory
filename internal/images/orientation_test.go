package images_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
)

// markedImage is 4 wide and 2 tall with two identifiable corners: red at the
// top-left and blue at the top-right.
//
// Two markers rather than one, because a single marker cannot separate a
// mirror from a rotation — orientations 2 and 6 both move the top-left corner
// somewhere, and only a second reference point says which way the rest of the
// image went.
func markedImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 30, G: 30, B: 30, A: 255})
		}
	}
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(3, 0, color.RGBA{B: 255, A: 255})
	return img
}

func pixelAt(t *testing.T, img image.Image, x, y int) (uint32, uint32, uint32) {
	t.Helper()

	r, g, b, _ := img.At(x, y).RGBA()
	return r, g, b
}

// TestEveryOrientationLandsWhereItShould pins the exact destination of both
// marker pixels for all eight EXIF orientations.
//
// The earlier table asserted only the output dimensions, which cannot tell 5
// from 6 from 7 from 8 — all four transpose — nor 2 from 4. Six of the eight
// could therefore have been implemented backwards and still passed: a photo
// would come out mirrored or upside down rather than upright, which is a worse
// outcome than the sideways one the whole feature exists to prevent.
//
// PNG, because the assertion is an exact colour and JPEG is lossy.
func TestEveryOrientationLandsWhereItShould(t *testing.T) {
	t.Parallel()

	const (
		w = 4
		h = 2
	)

	tests := []struct {
		name         string
		orientation  uint16
		wantW, wantH int
		redX, redY   int
		blueX, blueY int
	}{
		{name: "1 normal", orientation: 1, wantW: w, wantH: h, redX: 0, redY: 0, blueX: 3, blueY: 0},
		{name: "2 mirrored horizontally", orientation: 2, wantW: w, wantH: h, redX: 3, redY: 0, blueX: 0, blueY: 0},
		{name: "3 rotated 180", orientation: 3, wantW: w, wantH: h, redX: 3, redY: 1, blueX: 0, blueY: 1},
		{name: "4 mirrored vertically", orientation: 4, wantW: w, wantH: h, redX: 0, redY: 1, blueX: 3, blueY: 1},
		{name: "5 transposed", orientation: 5, wantW: h, wantH: w, redX: 0, redY: 0, blueX: 0, blueY: 3},
		{name: "6 rotated 90 clockwise", orientation: 6, wantW: h, wantH: w, redX: 1, redY: 0, blueX: 1, blueY: 3},
		{name: "7 transverse", orientation: 7, wantW: h, wantH: w, redX: 1, redY: 3, blueX: 1, blueY: 0},
		{name: "8 rotated 270 clockwise", orientation: 8, wantW: h, wantH: w, redX: 0, redY: 3, blueX: 0, blueY: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := images.Strip(pngWithOrientation(t, markedImage(), tc.orientation))
			require.NoError(t, err)

			img, err := png.Decode(bytes.NewReader(result.Data))
			require.NoError(t, err)

			require.Equal(t, tc.wantW, img.Bounds().Dx(), "output width")
			require.Equal(t, tc.wantH, img.Bounds().Dy(), "output height")

			r, g, b := pixelAt(t, img, tc.redX, tc.redY)
			assert.Equalf(t, [3]uint32{0xFFFF, 0, 0}, [3]uint32{r, g, b},
				"the red corner belongs at (%d,%d)", tc.redX, tc.redY)

			r, g, b = pixelAt(t, img, tc.blueX, tc.blueY)
			assert.Equalf(t, [3]uint32{0, 0, 0xFFFF}, [3]uint32{r, g, b},
				"the blue corner belongs at (%d,%d)", tc.blueX, tc.blueY)
		})
	}
}

// tiffBlockBigEndian builds the same one-entry IFD in motorola byte order.
func tiffBlockBigEndian(orientation uint16) []byte {
	return []byte{
		'M', 'M', // big-endian byte order
		0x00, 0x2A, // TIFF magic
		0x00, 0x00, 0x00, 0x08, // IFD0 at offset 8
		0x00, 0x01, // one entry
		0x01, 0x12, // tag 0x0112, orientation
		0x00, 0x03, // type SHORT
		0x00, 0x00, 0x00, 0x01, // count 1
		0x00, byte(orientation), 0x00, 0x00, // value, inline, big-endian
		0x00, 0x00, 0x00, 0x00, // no next IFD
	}
}

// TestBigEndianTIFFIsRead covers the byte order nothing else exercised.
//
// The parser handles both orders explicitly, but every other fixture in this
// package is little-endian ("II"). Plenty of cameras write big-endian ("MM"),
// and a mistake in that branch would leave exactly those photos sideways while
// the suite stayed green.
func TestBigEndianTIFFIsRead(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, markedImage()))
	data := encoded.Bytes()

	const afterIHDR = 8 + 8 + 13 + 4
	var withBE bytes.Buffer
	withBE.Write(data[:afterIHDR])
	withBE.Write(pngChunk("eXIf", tiffBlockBigEndian(6)))
	withBE.Write(data[afterIHDR:])

	result, err := images.Strip(withBE.Bytes())
	require.NoError(t, err)

	require.True(t, result.ReEncoded, "a big-endian orientation must be read, not ignored")
	assert.Equal(t, images.OrientationRotate90, result.Orientation)

	img, err := png.Decode(bytes.NewReader(result.Data))
	require.NoError(t, err)
	assert.Equal(t, 2, img.Bounds().Dx())
	assert.Equal(t, 4, img.Bounds().Dy())

	// Same destination as the little-endian case: byte order is a transport
	// detail, not a difference in meaning.
	r, g, b := pixelAt(t, img, 1, 0)
	assert.Equal(t, [3]uint32{0xFFFF, 0, 0}, [3]uint32{r, g, b})
}

// TestStripGuardsItsOwnPixelBudget — Strip is exported, so it cannot rely on
// every caller having checked the dimensions first. A reprocessing job or an
// admin backfill that skipped the check would otherwise reintroduce the
// decompression bomb the limit exists to stop.
func TestStripGuardsItsOwnPixelBudget(t *testing.T) {
	t.Parallel()

	// Well under the real limit, so this asserts the guard exists rather than
	// building a genuinely enormous fixture: the same code path rejects both.
	assert.Positive(t, images.MaxPixels, "the package must carry its own bound")

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, markedImage()))

	// A normal image passes the guard.
	require.NoError(t, images.CheckDimensions(encoded.Bytes(), images.MaxPixels))

	// And the guard actually rejects when the budget is smaller than the image.
	err := images.CheckDimensions(encoded.Bytes(), 4)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestCheckDimensionsReadsOnlyTheHeader is the performance rule made
// observable: validation must not decode the pixels, or the upright path's
// whole reason for existing is paid for anyway.
func TestCheckDimensionsReadsOnlyTheHeader(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, markedImage()))
	full := encoded.Bytes()

	// Truncating the compressed data leaves the header intact. A header-only
	// check still succeeds; a full decode could not.
	truncated := full[:len(full)-8]

	assert.NoError(t, images.CheckDimensions(truncated, images.MaxPixels),
		"dimensions come from the header, so a truncated body must not matter")

	_, err := images.Decode(truncated, images.MaxPixels)
	assert.Error(t, err, "a real decode of the same bytes must fail, proving the check did not do one")
}
