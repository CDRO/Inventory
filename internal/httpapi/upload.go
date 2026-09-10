package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/CDRO/Inventory/internal/images"
)

// Upload limits, from docs/specs/04-backend-api-conventions.md.
const (
	// MaxUploadBytes is a hard cap on the compressed file, independent of
	// pixel count. Shelf photos are large; a 25MB ceiling admits any real
	// phone photo while bounding what one request can cost.
	MaxUploadBytes = 25 << 20

	// maxMultipartMemory is how much of a parsed form is held in RAM before
	// the rest spills to disk. Small on purpose: this runs on a NAS beside
	// PostgreSQL, and buffering a 25MB upload per concurrent request is how a
	// household server starts swapping.
	maxMultipartMemory = 8 << 20

	// MaxImagePixels bounds the decoded dimensions. A small file can claim
	// enormous dimensions — the decompression-bomb shape — and the cost of
	// decoding is in pixels, not bytes.
	MaxImagePixels = 50_000_000
)

// UploadedImage is a stripped image ready to be written to disk.
type UploadedImage struct {
	// Data is the stripped image. It is the only copy that exists: the
	// original bytes are never persisted, so there is no file left holding the
	// GPS coordinates.
	Data []byte
	// Filename is generated — a UUIDv7 plus the extension implied by the
	// file's own magic bytes. It is never derived from what the client called
	// the file.
	Filename string
	Format   images.Format
	// Orientation is what the original claimed, kept for logging. The pixels
	// in Data are already upright.
	Orientation images.Orientation
}

// ReadImageUpload reads one image out of a multipart form, strips its
// metadata, and returns it with a generated filename.
//
// Every image entering the system goes through here, which is the point: the
// stripping rule in docs/specs/04-backend-api-conventions.md applies to shelf
// photos, product photos, consumption photos and custom uploads alike, and a
// rule applied at each call site is a rule that gets missed at one of them.
//
// The returned *Failure is ready for the error serializer; the caller does not
// decide status codes.
func ReadImageUpload(w http.ResponseWriter, r *http.Request, field string) (*UploadedImage, *Failure) {
	// Wrapped before parsing, so an oversized body is cut off rather than read
	// into memory and then rejected.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)

	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, PayloadTooLarge()
		}
		return nil, ValidationFailed(
			map[string][]string{field: {"The upload could not be read."}},
			fmt.Errorf("parse multipart form: %w", err),
		)
	}

	file, _, err := r.FormFile(field)
	if err != nil {
		return nil, ValidationFailed(
			map[string][]string{field: {"An image file is required."}},
			fmt.Errorf("read form file %q: %w", field, err),
		)
	}
	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(io.LimitReader(file, MaxUploadBytes+1))
	if err != nil {
		return nil, Internal(fmt.Errorf("read upload: %w", err))
	}
	if len(raw) > MaxUploadBytes {
		return nil, PayloadTooLarge()
	}

	// The format comes from the bytes, never from the client's declared
	// content type or filename: both are attacker-controlled, and a path built
	// from a supplied name is how an upload becomes a directory traversal.
	if _, err := images.DetectFormat(raw); err != nil {
		return nil, ValidationFailed(
			map[string][]string{field: {"Only JPEG and PNG images are supported."}},
			err,
		)
	}

	// Header only. A full decode here would cost the very 50MP decode that the
	// upright segment-surgery path exists to avoid, and then discard the
	// pixels.
	if err := images.CheckDimensions(raw, MaxImagePixels); err != nil {
		return nil, ValidationFailed(
			map[string][]string{field: {"The image is too large or could not be read."}},
			err,
		)
	}

	stripped, err := images.Strip(raw)
	if err != nil {
		return nil, ValidationFailed(
			map[string][]string{field: {"The image could not be processed."}},
			fmt.Errorf("strip metadata: %w", err),
		)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, Internal(fmt.Errorf("generate upload id: %w", err))
	}

	return &UploadedImage{
		Data:        stripped.Data,
		Filename:    id.String() + stripped.Format.Extension(),
		Format:      stripped.Format,
		Orientation: stripped.Orientation,
	}, nil
}
