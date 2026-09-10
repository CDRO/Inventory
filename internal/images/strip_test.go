package images_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/images"
)

// tiffBlock builds a minimal little-endian TIFF block whose IFD0 contains one
// entry: the orientation tag.
func tiffBlock(orientation uint16) []byte {
	return []byte{
		'I', 'I', // little-endian byte order
		0x2A, 0x00, // TIFF magic
		0x08, 0x00, 0x00, 0x00, // IFD0 at offset 8
		0x01, 0x00, // one entry
		0x12, 0x01, // tag 0x0112, orientation
		0x03, 0x00, // type SHORT
		0x01, 0x00, 0x00, 0x00, // count 1
		byte(orientation), 0x00, 0x00, 0x00, // value, inline
		0x00, 0x00, 0x00, 0x00, // no next IFD
	}
}

// jpegWithOrientation encodes img and splices an APP1 EXIF segment carrying
// the given orientation in directly after the SOI marker — which is where a
// camera puts it.
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95}))
	data := encoded.Bytes()
	require.Equal(t, byte(0xD8), data[1], "encoded jpeg must start with SOI")

	payload := append([]byte("Exif\x00\x00"), tiffBlock(orientation)...)
	length := len(payload) + 2

	var out bytes.Buffer
	out.Write(data[:2]) // SOI
	out.Write([]byte{0xFF, 0xE1, byte(length >> 8), byte(length)})
	out.Write(payload)
	out.Write(data[2:])
	return out.Bytes()
}

// pngWithOrientation encodes img and inserts an eXIf chunk after the IHDR.
func pngWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	data := encoded.Bytes()

	// IHDR is always the first chunk: 8-byte signature, then 4 length + 4 type
	// + 13 data + 4 crc.
	const afterIHDR = 8 + 8 + 13 + 4
	require.Greater(t, len(data), afterIHDR)

	var out bytes.Buffer
	out.Write(data[:afterIHDR])
	out.Write(pngChunk("eXIf", tiffBlock(orientation)))
	out.Write(data[afterIHDR:])
	return out.Bytes()
}

// pngChunk assembles a PNG chunk with a valid CRC.
func pngChunk(chunkType string, payload []byte) []byte {
	var buf bytes.Buffer
	length := len(payload)
	buf.Write([]byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)})
	buf.WriteString(chunkType)
	buf.Write(payload)

	crc := crc32PNG(append([]byte(chunkType), payload...))
	buf.Write([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	return buf.Bytes()
}

func crc32PNG(data []byte) uint32 {
	// PNG uses the standard IEEE CRC-32.
	var table [256]uint32
	for i := range table {
		c := uint32(i)
		for k := 0; k < 8; k++ {
			if c&1 != 0 {
				c = 0xEDB88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		table[i] = c
	}
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = table[(crc^uint32(b))&0xFF] ^ (crc >> 8)
	}
	return crc ^ 0xFFFFFFFF
}

// cornerImage builds a w×h image with a distinctly coloured top-left pixel, so
// a transform can be identified by where that colour ends up.
func cornerImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	return img
}

// containsEXIF reports whether the marker bytes of an EXIF block survive
// anywhere in the file. Cruder than parsing, and deliberately so: it catches a
// stripper that removed the segment header but left the payload behind.
func containsEXIF(data []byte) bool {
	return bytes.Contains(data, []byte("Exif\x00\x00")) ||
		bytes.Contains(data, []byte("eXIf"))
}

// TestOrientationIsAppliedToPixelsBeforeMetadataIsDropped is the central test
// of this package.
//
// The EXIF orientation tag is an instruction to rotate the stored pixels. If
// the metadata is deleted first, that instruction is gone and nothing remains
// to say the photo was taken sideways — so every portrait phone photo ends up
// on its side, permanently, with no way for the user to correct it.
//
// The check is not "does an orientation field survive" but "did the pixels
// actually move": a 4×2 image must come back 2×4.
func TestOrientationIsAppliedToPixelsBeforeMetadataIsDropped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		orientation   uint16
		wantW, wantH  int
		wantTransform bool
	}{
		{name: "normal is left alone", orientation: 1, wantW: 4, wantH: 2},
		{name: "flip horizontal", orientation: 2, wantW: 4, wantH: 2, wantTransform: true},
		{name: "rotate 180", orientation: 3, wantW: 4, wantH: 2, wantTransform: true},
		{name: "flip vertical", orientation: 4, wantW: 4, wantH: 2, wantTransform: true},
		{name: "transpose swaps the axes", orientation: 5, wantW: 2, wantH: 4, wantTransform: true},
		{name: "rotate 90 swaps the axes", orientation: 6, wantW: 2, wantH: 4, wantTransform: true},
		{name: "transverse swaps the axes", orientation: 7, wantW: 2, wantH: 4, wantTransform: true},
		{name: "rotate 270 swaps the axes", orientation: 8, wantW: 2, wantH: 4, wantTransform: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := jpegWithOrientation(t, cornerImage(4, 2), tc.orientation)
			require.True(t, containsEXIF(original), "the fixture must actually carry EXIF")

			result, err := images.Strip(original)
			require.NoError(t, err)

			assert.Equal(t, images.FormatJPEG, result.Format)
			assert.Equal(t, tc.wantTransform, result.ReEncoded,
				"a rotation forces a re-encode; an upright image must not pay for one")

			cfg, err := jpeg.DecodeConfig(bytes.NewReader(result.Data))
			require.NoError(t, err)
			assert.Equal(t, tc.wantW, cfg.Width, "pixel width after the transform")
			assert.Equal(t, tc.wantH, cfg.Height, "pixel height after the transform")

			assert.False(t, containsEXIF(result.Data), "no metadata may survive")
		})
	}
}

// TestRotate90MovesTheCornerWhereItBelongs pins the direction of the rotation,
// which the dimension check above cannot see: 5, 6, 7 and 8 all transpose, and
// getting the sign wrong turns a photo upside down instead of upright.
//
// PNG rather than JPEG, because the check is an exact pixel colour and JPEG is
// lossy.
func TestRotate90MovesTheCornerWhereItBelongs(t *testing.T) {
	t.Parallel()

	// 4 wide, 2 tall, red at the top-left.
	original := pngWithOrientation(t, cornerImage(4, 2), 6)

	result, err := images.Strip(original)
	require.NoError(t, err)
	require.True(t, result.ReEncoded)

	img, err := png.Decode(bytes.NewReader(result.Data))
	require.NoError(t, err)

	bounds := img.Bounds()
	require.Equal(t, 2, bounds.Dx())
	require.Equal(t, 4, bounds.Dy())

	// Rotating 90° clockwise carries the top-left corner to the top-right.
	r, g, b, _ := img.At(1, 0).RGBA()
	assert.Equal(t, uint32(0xFFFF), r, "the red corner must land top-right after a 90° clockwise turn")
	assert.Equal(t, uint32(0), g)
	assert.Equal(t, uint32(0), b)
}

// TestUprightImageIsStrippedWithoutReEncoding covers the performance rule that
// is also a quality rule: an image needing no rotation is stripped by segment
// surgery, so it is neither degraded by a second lossy encode nor decoded at
// up to 50 megapixels on a NAS.
func TestUprightImageIsStrippedWithoutReEncoding(t *testing.T) {
	t.Parallel()

	original := jpegWithOrientation(t, cornerImage(8, 8), 1)

	result, err := images.Strip(original)
	require.NoError(t, err)

	assert.False(t, result.ReEncoded, "an upright image must take the lossless path")
	assert.False(t, containsEXIF(result.Data))
	assert.Less(t, len(result.Data), len(original), "the metadata segment is gone, so the file is smaller")

	// The entropy-coded image data is untouched: the scan bytes of the
	// original still appear verbatim in the output.
	scanStart := bytes.Index(original, []byte{0xFF, 0xDA})
	require.Positive(t, scanStart)
	assert.True(t, bytes.Contains(result.Data, original[scanStart:]),
		"the compressed pixel data must be copied, not re-encoded")
}

// TestCommentAndAppSegmentsAreRemoved covers the rest of rule 2: it is not
// only EXIF. APPn covers JFIF, XMP, IPTC and embedded thumbnails, and COM is a
// free-text comment an editor may have filled with anything.
func TestCommentAndAppSegmentsAreRemoved(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, cornerImage(4, 4), nil))
	data := encoded.Bytes()

	secret := []byte("GPS 47.3769 N 8.5417 E")
	comment := append([]byte{0xFF, 0xFE, 0x00, byte(len(secret) + 2)}, secret...)

	xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>private</x:xmpmeta>")
	xmpSeg := append([]byte{0xFF, 0xE1, byte((len(xmp) + 2) >> 8), byte(len(xmp) + 2)}, xmp...)

	var withMetadata bytes.Buffer
	withMetadata.Write(data[:2])
	withMetadata.Write(comment)
	withMetadata.Write(xmpSeg)
	withMetadata.Write(data[2:])

	result, err := images.Strip(withMetadata.Bytes())
	require.NoError(t, err)

	assert.NotContains(t, string(result.Data), string(secret), "a COM segment must not survive")
	assert.NotContains(t, string(result.Data), "xmpmeta", "an XMP APP1 segment must not survive")

	// Still a valid image afterwards.
	_, err = jpeg.Decode(bytes.NewReader(result.Data))
	assert.NoError(t, err)
}

func TestPNGMetadataChunksAreRemoved(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, cornerImage(4, 4)))
	data := encoded.Bytes()

	const afterIHDR = 8 + 8 + 13 + 4
	var withText bytes.Buffer
	withText.Write(data[:afterIHDR])
	withText.Write(pngChunk("tEXt", []byte("Comment\x00taken at home")))
	withText.Write(pngChunk("eXIf", tiffBlock(1)))
	withText.Write(data[afterIHDR:])

	result, err := images.Strip(withText.Bytes())
	require.NoError(t, err)

	assert.False(t, result.ReEncoded, "an upright PNG takes the chunk-surgery path")
	assert.NotContains(t, string(result.Data), "taken at home")
	assert.NotContains(t, string(result.Data), "eXIf")
	assert.NotContains(t, string(result.Data), "tEXt")

	// The image still decodes, and the pixels are unchanged.
	img, err := png.Decode(bytes.NewReader(result.Data))
	require.NoError(t, err)
	assert.Equal(t, 4, img.Bounds().Dx())
	r, _, _, _ := img.At(0, 0).RGBA()
	assert.Equal(t, uint32(0xFFFF), r)
}

// TestMalformedEXIFIsSurvivable — this parses a file someone uploaded. A
// truncated or hostile EXIF block must produce "no orientation", never a panic
// and never a read past the buffer.
func TestMalformedEXIFIsSurvivable(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, cornerImage(4, 4), nil))
	base := encoded.Bytes()

	blocks := map[string][]byte{
		"empty":               {},
		"byte order only":     {'I', 'I'},
		"truncated header":    {'I', 'I', 0x2A, 0x00, 0x08},
		"ifd offset past end": {'I', 'I', 0x2A, 0x00, 0xFF, 0xFF, 0xFF, 0x7F},
		"absurd entry count":  {'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0xFF, 0xFF},
		"bad byte order":      {'X', 'Y', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00},
		"entry runs past end": {'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00, 0x12, 0x01},
		"orientation out of range": append(
			[]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00, 0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00},
			[]byte{0x63, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}...),
	}

	for name, block := range blocks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			payload := append([]byte("Exif\x00\x00"), block...)
			length := len(payload) + 2

			var withBad bytes.Buffer
			withBad.Write(base[:2])
			withBad.Write([]byte{0xFF, 0xE1, byte(length >> 8), byte(length)})
			withBad.Write(payload)
			withBad.Write(base[2:])

			assert.NotPanics(t, func() {
				result, err := images.Strip(withBad.Bytes())
				require.NoError(t, err)
				assert.False(t, result.ReEncoded, "an unreadable orientation must mean no transform")
				assert.False(t, containsEXIF(result.Data))
			})
		})
	}
}

func TestStripRejectsUnsupportedFormats(t *testing.T) {
	t.Parallel()

	for name, data := range map[string][]byte{
		"empty":              {},
		"text":               []byte("this is not an image"),
		"gif":                []byte("GIF89a\x01\x00\x01\x00"),
		"pdf":                []byte("%PDF-1.7\n"),
		"partial jpeg magic": {0xFF},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := images.Strip(data)
			assert.ErrorIs(t, err, images.ErrUnsupportedFormat)
		})
	}
}

func TestDetectFormatUsesTheBytesNotTheName(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, cornerImage(2, 2)))

	format, err := images.DetectFormat(encoded.Bytes())
	require.NoError(t, err)
	assert.Equal(t, images.FormatPNG, format)
	assert.Equal(t, ".png", format.Extension())
	assert.Equal(t, "image/png", format.ContentType())
}

// TestDecodeRefusesOversizedImages guards the decode step against a
// decompression bomb: a small file that claims enormous dimensions.
func TestDecodeRefusesOversizedImages(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, cornerImage(50, 50)))

	_, err := images.Decode(encoded.Bytes(), 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")

	img, err := images.Decode(encoded.Bytes(), 10_000)
	require.NoError(t, err)
	assert.Equal(t, 50, img.Bounds().Dx())
}
