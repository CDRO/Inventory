package httpapi_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/httpapi"
	"github.com/CDRO/Inventory/internal/images"
)

// jpegWithEXIF builds a JPEG carrying an APP1 EXIF segment with the given
// orientation, mirroring what a phone camera produces.
func jpegWithEXIF(t *testing.T, w, h int, orientation byte) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 90, G: 90, B: 90, A: 255})
		}
	}

	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}))
	data := encoded.Bytes()

	tiff := []byte{
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00,
		0x01, 0x00,
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00,
		orientation, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	length := len(payload) + 2

	var out bytes.Buffer
	out.Write(data[:2])
	out.Write([]byte{0xFF, 0xE1, byte(length >> 8), byte(length)})
	out.Write(payload)
	out.Write(data[2:])
	return out.Bytes()
}

// uploadRequest builds a multipart request carrying one file.
func uploadRequest(t *testing.T, field, filename string, content []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/storages/x/ingest/shelf-photos", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// TestUploadStripsMetadataOnTheWayIn is the rule at the point it actually
// matters. images.Strip being correct is worth nothing if the upload path does
// not call it, so this asserts on what a handler would receive.
func TestUploadStripsMetadataOnTheWayIn(t *testing.T) {
	t.Parallel()

	original := jpegWithEXIF(t, 8, 4, 1)
	require.Contains(t, string(original), "Exif\x00\x00", "the fixture must carry EXIF")

	req := uploadRequest(t, "photo", "IMG_2024.jpg", original)
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, failure)
	require.NotNil(t, uploaded)
	assert.NotContains(t, string(uploaded.Data), "Exif\x00\x00",
		"no image may reach a handler with its metadata intact")
	assert.Equal(t, images.FormatJPEG, uploaded.Format)
}

// TestUploadAppliesOrientationBeforeStripping — the same ordering rule, on the
// path a real photo takes. A portrait phone photo must arrive upright.
func TestUploadAppliesOrientationBeforeStripping(t *testing.T) {
	t.Parallel()

	// 8 wide, 4 tall, tagged "rotate 90".
	req := uploadRequest(t, "photo", "IMG_2024.jpg", jpegWithEXIF(t, 8, 4, 6))
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, failure)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(uploaded.Data))
	require.NoError(t, err)

	assert.Equal(t, 4, cfg.Width, "the rotation must be baked into the pixels")
	assert.Equal(t, 8, cfg.Height)
	assert.NotContains(t, string(uploaded.Data), "Exif\x00\x00",
		"and the metadata must be gone afterwards")
}

// TestUploadFilenameIsGeneratedNotSupplied — a path built from a client-chosen
// name is how an upload becomes a directory traversal.
func TestUploadFilenameIsGeneratedNotSupplied(t *testing.T) {
	t.Parallel()

	hostile := "../../../etc/passwd.jpg"

	req := uploadRequest(t, "photo", hostile, jpegWithEXIF(t, 4, 4, 1))
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, failure)
	assert.NotContains(t, uploaded.Filename, "..")
	assert.NotContains(t, uploaded.Filename, "/")
	assert.NotContains(t, uploaded.Filename, "passwd")
	assert.True(t, strings.HasSuffix(uploaded.Filename, ".jpg"))

	// The stem is a UUID, so two uploads of the same photo cannot collide.
	stem := strings.TrimSuffix(uploaded.Filename, ".jpg")
	_, err := uuid.Parse(stem)
	assert.NoError(t, err, "the filename stem must be a generated id")
}

// TestUploadExtensionComesFromTheBytes — a PNG named .jpg must be stored as a
// PNG. Trusting the name would file it under an extension that lies about its
// contents.
func TestUploadExtensionComesFromTheBytes(t *testing.T) {
	t.Parallel()

	var pngData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	require.NoError(t, png.Encode(&pngData, img))

	req := uploadRequest(t, "photo", "actually-a-png.jpg", pngData.Bytes())
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, failure)
	assert.Equal(t, images.FormatPNG, uploaded.Format)
	assert.True(t, strings.HasSuffix(uploaded.Filename, ".png"),
		"the extension follows the magic bytes, not the client's name")
}

func TestUploadRejectsNonImages(t *testing.T) {
	t.Parallel()

	req := uploadRequest(t, "photo", "notes.txt", []byte("this is not an image at all"))
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, uploaded)
	require.NotNil(t, failure)
	assert.Equal(t, http.StatusUnprocessableEntity, failure.Status)
	assert.Equal(t, httpapi.CodeValidationFailed, failure.Code)
}

func TestUploadRejectsAMissingField(t *testing.T) {
	t.Parallel()

	req := uploadRequest(t, "wrong_field", "x.jpg", jpegWithEXIF(t, 4, 4, 1))
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, uploaded)
	require.NotNil(t, failure)
	assert.Equal(t, http.StatusUnprocessableEntity, failure.Status)
}

// TestUploadRejectsOversizedBodyWith413 checks the cap is enforced by cutting
// the body off, not by reading it all and then complaining.
func TestUploadRejectsOversizedBodyWith413(t *testing.T) {
	t.Parallel()

	oversized := bytes.Repeat([]byte{0xAB}, httpapi.MaxUploadBytes+1024)
	req := uploadRequest(t, "photo", "huge.jpg", oversized)
	rec := httptest.NewRecorder()

	uploaded, failure := httpapi.ReadImageUpload(rec, req, "photo")

	require.Nil(t, uploaded)
	require.NotNil(t, failure)
	assert.Equal(t, http.StatusRequestEntityTooLarge, failure.Status)
	assert.Equal(t, httpapi.CodePayloadTooLarge, failure.Code)
}
