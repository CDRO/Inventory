// Package barcode decodes a printed 1D barcode out of a photograph.
//
// It exists for one narrow job: the server-side fallback of
// docs/specs/20-barcode-recall.md, for a browser with no `BarcodeDetector`.
// The PWA decodes in the browser where it can, and typing the digits by hand
// is always available; this is the third path, for the phone that has neither
// a detector nor a user willing to type thirteen digits correctly.
//
// # What this package is not
//
//   - **Not identification.** Nothing here asks what a product *is*. It reads
//     a printed number and stops. Identification remains vision-first, and
//     docs/specs/00-overview.md's non-goal — consulting an external UPC/EAN
//     database — is untouched by this package, which makes no network call of
//     any kind.
//   - **Not an AI call.** Deterministic pixel decoding: no Gemini, no cost, no
//     job queue. docs/specs/04-backend-api-conventions.md's job machinery is
//     for slow external calls, and this is neither.
//   - **Not a place bytes are stored.** Decode takes a slice and returns a
//     string. It opens no file, writes nothing, and has no filesystem
//     dependency at all — which is the structural half of the non-retention
//     guarantee its HTTP caller makes.
package barcode

import (
	"bytes"
	"errors"
	"image"

	// The decoders for the two formats an upload may be in. Registered for
	// image.Decode's side effect only, exactly as the images package does.
	_ "image/jpeg"
	_ "image/png"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/oned"
)

// MaxPixels bounds the decoded dimensions.
//
// A small file can claim enormous dimensions — the decompression-bomb shape —
// and the cost of decoding is in pixels, not bytes. The same ceiling the
// upload path uses (internal/images), for the same reason: this runs on a NAS
// beside PostgreSQL.
const MaxPixels = 50_000_000

// ErrNotDecodable means no barcode was found in the image.
//
// It is a distinct, checkable error rather than an empty string precisely
// because docs/specs/20-barcode-recall.md forbids "a guessed or empty-string
// result": a caller that ignored the error and used the value would otherwise
// associate the empty code with a product.
var ErrNotDecodable = errors.New("barcode: no readable barcode found")

// Decode reads the first barcode it finds in a JPEG or PNG image.
//
// Formats attempted are the ones docs/specs/20-barcode-recall.md names for the
// client-side `BarcodeDetector` path, so the two decoders agree about what
// counts as a barcode: EAN-13, EAN-8, UPC-A, UPC-E and Code 128.
//
// Anything unreadable — an unsupported format, a photo of a shelf, a blurred
// label — is ErrNotDecodable. There is no partial success.
func Decode(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrNotDecodable
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", ErrNotDecodable
	}
	// Checked from the header before the pixels are read, so an image that
	// claims 40000x40000 costs a header parse rather than 1.6 gigapixels of
	// allocation.
	if config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > MaxPixels {
		return "", ErrNotDecodable
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", ErrNotDecodable
	}

	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", ErrNotDecodable
	}

	// TRY_HARDER, because the input is a hand-held photograph rather than a
	// clean render: it costs extra scan passes, including the rotated one that
	// finds a code held sideways, and this route is already the slow fallback
	// somebody chose over typing.
	//
	// ALSO_INVERTED catches a light-on-dark label, which packaging does use.
	//
	// POSSIBLE_FORMATS is not decoration. A UPC-A barcode is bit-for-bit an
	// EAN-13 whose first digit is 0, so without UPC-A in this list the decoder
	// reports the 13-digit form while the browser's BarcodeDetector reports
	// the 12-digit one — and the same physical box would then be stored under
	// two different keys depending on which of the two paths read it, so a
	// product associated in the browser would not be recalled by a photo.
	// With the hint, both paths return the 12 digits printed on the box.
	hints := map[gozxing.DecodeHintType]any{
		gozxing.DecodeHintType_TRY_HARDER:    true,
		gozxing.DecodeHintType_ALSO_INVERTED: true,
		gozxing.DecodeHintType_POSSIBLE_FORMATS: []gozxing.BarcodeFormat{
			gozxing.BarcodeFormat_EAN_13,
			gozxing.BarcodeFormat_EAN_8,
			gozxing.BarcodeFormat_UPC_A,
			gozxing.BarcodeFormat_UPC_E,
			gozxing.BarcodeFormat_CODE_128,
		},
	}

	// Two readers rather than gozxing's MultiFormatReader: that one also drags
	// in QR, Data Matrix, Aztec and PDF417, none of which is a product
	// barcode, and each extra family is another way to return a string that
	// means something other than "the number printed under the bars".
	readers := []gozxing.Reader{
		oned.NewMultiFormatUPCEANReader(hints), // EAN-13, EAN-8, UPC-A, UPC-E
		oned.NewCode128Reader(),
	}

	for _, reader := range readers {
		// A fresh reader per call rather than a package-level one: gozxing's
		// readers carry per-image state, and Decode is reachable concurrently
		// from every request.
		result, err := reader.Decode(bitmap, hints)
		if err != nil {
			continue
		}
		text := result.GetText()
		if text == "" {
			// A decoder that reports success with nothing to show is a bug in
			// the decoder, not a barcode. Treating it as a miss is what keeps
			// the "never an empty-string result" guarantee true regardless of
			// what the library does.
			continue
		}
		return text, nil
	}
	return "", ErrNotDecodable
}
