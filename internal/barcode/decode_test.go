package barcode_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/barcode"
)

// render draws an encoded barcode as a real image, so the decoder under test
// is given pixels rather than a bit matrix.
//
// The encoder is gozxing's, which makes this a round trip through one library.
// That is deliberate and it is not a test of the library: what it exercises is
// this package's own wiring — which formats are attempted, that the hints are
// accepted, that JPEG and PNG both reach the decoder, and that a failure comes
// back as ErrNotDecodable rather than as a library error type. The cases below
// that matter most — a blank photo, a truncated file, an empty body — need no
// encoder at all.
func render(t *testing.T, writer gozxing.Writer, format gozxing.BarcodeFormat, content string) image.Image {
	t.Helper()

	matrix, err := writer.Encode(content, format, 600, 240, nil)
	require.NoError(t, err)

	img := image.NewGray(image.Rect(0, 0, matrix.GetWidth(), matrix.GetHeight()))
	for y := 0; y < matrix.GetHeight(); y++ {
		for x := 0; x < matrix.GetWidth(); x++ {
			shade := color.Gray{Y: 255}
			if matrix.Get(x, y) {
				shade = color.Gray{Y: 0}
			}
			img.SetGray(x, y, shade)
		}
	}
	return img
}

func asPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func asJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}))
	return buf.Bytes()
}

// TestDecodeReadsTheFormatsTheSpecNames — the server fallback has to agree
// with the client-side BarcodeDetector about what counts as a barcode
// (docs/specs/20-barcode-recall.md).
func TestDecodeReadsTheFormatsTheSpecNames(t *testing.T) {
	cases := []struct {
		name    string
		writer  gozxing.Writer
		format  gozxing.BarcodeFormat
		content string
	}{
		{"ean_13", oned.NewEAN13Writer(), gozxing.BarcodeFormat_EAN_13, "4006381333931"},
		{"ean_8", oned.NewEAN8Writer(), gozxing.BarcodeFormat_EAN_8, "96385074"},
		{"upc_a", oned.NewUPCAWriter(), gozxing.BarcodeFormat_UPC_A, "036000291452"},
		{"code_128", oned.NewCode128Writer(), gozxing.BarcodeFormat_CODE_128, "ABC-12345"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := render(t, tc.writer, tc.format, tc.content)

			got, err := barcode.Decode(asPNG(t, img))
			require.NoError(t, err)
			assert.Equal(t, tc.content, got)

			got, err = barcode.Decode(asJPEG(t, img))
			require.NoError(t, err, "a phone uploads JPEG, so that path has to work too")
			assert.Equal(t, tc.content, got)
		})
	}
}

// TestDecodeRefusesAPhotoWithNoBarcode — "never a guessed or empty-string
// result" (docs/specs/20-barcode-recall.md).
func TestDecodeRefusesAPhotoWithNoBarcode(t *testing.T) {
	blank := image.NewGray(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			blank.SetGray(x, y, color.Gray{Y: 240})
		}
	}

	got, err := barcode.Decode(asPNG(t, blank))
	assert.ErrorIs(t, err, barcode.ErrNotDecodable)
	assert.Empty(t, got)
}

func TestDecodeRefusesAnythingThatIsNotAnImage(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     {},
		"text":      []byte("this is not an image"),
		"truncated": asPNG(t, image.NewGray(image.Rect(0, 0, 8, 8)))[:20],
	} {
		t.Run(name, func(t *testing.T) {
			got, err := barcode.Decode(data)
			assert.ErrorIs(t, err, barcode.ErrNotDecodable)
			assert.Empty(t, got)
		})
	}
}

// TestDecodeIsNotFooledByAQRCode — the readers are the four 1D families the
// spec names, and nothing else. A QR code in frame is not a product barcode,
// and returning its payload would put arbitrary text into product_barcodes.
func TestDecodeIgnoresFormatsOutsideTheSpecsList(t *testing.T) {
	// ITF is a 1D format gozxing can write and this package deliberately does
	// not read: it is used for shipping cartons, not retail units.
	img := render(t, oned.NewITFWriter(), gozxing.BarcodeFormat_ITF, "1234567890")

	got, err := barcode.Decode(asPNG(t, img))
	assert.ErrorIs(t, err, barcode.ErrNotDecodable)
	assert.Empty(t, got)
}
