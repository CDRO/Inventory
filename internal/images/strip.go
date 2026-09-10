// Package images enforces the rule that no uploaded image is ever written to
// disk with its embedded metadata intact
// (docs/specs/04-backend-api-conventions.md).
//
// A phone photo carries GPS coordinates — the user's home address — plus the
// device model, serial numbers and capture times. None of that belongs in an
// inventory system: it survives into backups, and it travels with any file
// that is later exported or shared.
//
// The order of operations is the whole point. The EXIF orientation tag is an
// instruction to rotate the stored pixels, so it must be read and applied
// *before* the metadata is dropped; deleting first loses it and leaves every
// portrait photo sideways with nothing left to correct it.
package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
)

// ErrUnsupportedFormat is returned for anything that is not JPEG or PNG.
var ErrUnsupportedFormat = errors.New("images: unsupported format")

// jpegQuality is used only when a rotation forces a re-encode. High enough
// that a re-encoded shelf photo is not visibly worse than the original.
const jpegQuality = 92

// MaxPixels bounds any decode this package performs.
//
// Strip enforces it itself rather than trusting the caller to have checked
// first. A small file can claim enormous dimensions, and the cost of decoding
// is in pixels rather than bytes; relying on every future caller — a
// reprocessing job, an admin backfill, a helper someone lifts into production
// code — to remember a guard is the same mistake the single error serializer
// exists to avoid.
const MaxPixels = 50_000_000

// Format names the container of a stripped image.
type Format string

const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
)

// Extension is the file extension for a format, used to build the generated
// filename. Filenames are never derived from what the client called the file.
func (f Format) Extension() string {
	switch f {
	case FormatJPEG:
		return ".jpg"
	case FormatPNG:
		return ".png"
	default:
		return ""
	}
}

// ContentType is the MIME type to serve a stripped image with.
func (f Format) ContentType() string {
	switch f {
	case FormatJPEG:
		return "image/jpeg"
	case FormatPNG:
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// Result is a stripped image and what happened to it.
type Result struct {
	Data   []byte
	Format Format
	// Orientation is what the original claimed. It is reported so a caller can
	// log or test the decision; the pixels in Data are already upright.
	Orientation Orientation
	// ReEncoded is true when the pixels had to be rebuilt. False means the
	// image was stripped by segment surgery: lossless, and no decode of a
	// 50MP file.
	ReEncoded bool
}

// Strip removes all embedded metadata from an image, applying the EXIF
// orientation to the pixels first.
//
// When the orientation is already normal the file is stripped by
// segment-level surgery — the metadata segments are dropped and everything
// else is copied verbatim. That is lossless and cheap, which matters because
// the alternative is a decode-encode cycle on an image of up to 50 megapixels,
// on a NAS.
//
// A rotation forces a re-encode, and that re-encode is what removes the
// metadata on that path: the encoder writes only what it is given.
func Strip(data []byte) (*Result, error) {
	switch {
	case isJPEG(data):
		return stripJPEG(data)
	case isPNG(data):
		return stripPNG(data)
	default:
		return nil, ErrUnsupportedFormat
	}
}

// DetectFormat reports the format from the file's own bytes.
//
// The client's declared content type and filename are not consulted: both are
// attacker-controlled, and a path built from a client-supplied name is how a
// upload becomes a directory traversal.
func DetectFormat(data []byte) (Format, error) {
	switch {
	case isJPEG(data):
		return FormatJPEG, nil
	case isPNG(data):
		return FormatPNG, nil
	default:
		return "", ErrUnsupportedFormat
	}
}

func isJPEG(data []byte) bool {
	return len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

func isPNG(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

// --- JPEG -------------------------------------------------------------------

// JPEG markers.
const (
	markerSOI  = 0xD8
	markerEOI  = 0xD9
	markerSOS  = 0xDA
	markerAPP0 = 0xE0
	markerAPP1 = 0xE1
	markerAPPF = 0xEF
	markerCOM  = 0xFE
	markerTEM  = 0x01
	markerRST0 = 0xD0
	markerRST7 = 0xD7
)

// isMetadataMarker reports whether a segment holds metadata rather than image
// data. APP0–APP15 covers JFIF, EXIF, XMP, IPTC and embedded thumbnails; COM
// is a free-text comment.
func isMetadataMarker(marker byte) bool {
	return (marker >= markerAPP0 && marker <= markerAPPF) || marker == markerCOM
}

// isStandaloneMarker reports whether a marker carries no length field.
func isStandaloneMarker(marker byte) bool {
	return marker == markerTEM || (marker >= markerRST0 && marker <= markerRST7)
}

func stripJPEG(data []byte) (*Result, error) {
	orientation, err := jpegOrientation(data)
	if err != nil {
		return nil, err
	}

	// Rotation first, when there is one. The re-encode drops the metadata by
	// construction — nothing is copied across but pixels.
	if orientation.NeedsTransform() {
		if err := CheckDimensions(data, MaxPixels); err != nil {
			return nil, err
		}
		src, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("images: decode jpeg for rotation: %w", err)
		}
		var out bytes.Buffer
		if err := jpeg.Encode(&out, applyOrientation(src, orientation), &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, fmt.Errorf("images: re-encode jpeg: %w", err)
		}
		return &Result{Data: out.Bytes(), Format: FormatJPEG, Orientation: orientation, ReEncoded: true}, nil
	}

	stripped, err := dropJPEGMetadataSegments(data)
	if err != nil {
		return nil, err
	}
	return &Result{Data: stripped, Format: FormatJPEG, Orientation: orientation, ReEncoded: false}, nil
}

// walkJPEGSegments calls visit for every marker segment until the scan begins.
//
// visit receives the marker byte and the segment payload (excluding the
// two-byte length). It returns false to stop the walk. The returned int is the
// offset at which entropy-coded scan data starts, or len(data) if the file
// ended first.
func walkJPEGSegments(data []byte, visit func(marker byte, payload []byte) bool) (int, error) {
	if !isJPEG(data) {
		return 0, ErrUnsupportedFormat
	}

	i := 2 // past SOI
	for i+1 < len(data) {
		if data[i] != 0xFF {
			return 0, fmt.Errorf("images: malformed jpeg at offset %d", i)
		}
		// Fill bytes: a run of 0xFF before the marker is legal padding.
		j := i
		for j < len(data) && data[j] == 0xFF {
			j++
		}
		if j >= len(data) {
			return len(data), nil
		}
		marker := data[j]
		i = j + 1

		if marker == markerEOI {
			return i, nil
		}
		if isStandaloneMarker(marker) || marker == markerSOI {
			continue
		}
		if i+2 > len(data) {
			return 0, errors.New("images: truncated jpeg segment length")
		}
		length := int(data[i])<<8 | int(data[i+1])
		if length < 2 || i+length > len(data) {
			return 0, errors.New("images: jpeg segment length out of range")
		}
		payload := data[i+2 : i+length]

		if visit != nil && !visit(marker, payload) {
			return i + length, nil
		}
		i += length

		if marker == markerSOS {
			// Everything after the scan header is entropy-coded data.
			return i, nil
		}
	}
	return len(data), nil
}

// jpegOrientation finds the orientation in the APP1 EXIF segment, if any.
func jpegOrientation(data []byte) (Orientation, error) {
	orientation := OrientationNormal

	_, err := walkJPEGSegments(data, func(marker byte, payload []byte) bool {
		if marker != markerAPP1 || len(payload) < len(exifHeader) {
			return true
		}
		if !bytes.Equal(payload[:len(exifHeader)], exifHeader) {
			return true // some other APP1, e.g. XMP
		}
		orientation = orientationFromEXIF(payload[len(exifHeader):])
		return false // the first EXIF segment is the one that counts
	})
	if err != nil {
		return OrientationNormal, err
	}
	return orientation, nil
}

// dropJPEGMetadataSegments copies the file without its APPn and COM segments.
//
// This is the lossless path: the entropy-coded image data is never touched, so
// a 50MP photo costs a copy rather than a decode.
func dropJPEGMetadataSegments(data []byte) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write([]byte{0xFF, markerSOI})

	scanStart, err := walkJPEGSegments(data, func(marker byte, payload []byte) bool {
		if isMetadataMarker(marker) {
			return true // skip: this is what we are removing
		}
		out.Write([]byte{0xFF, marker})
		length := len(payload) + 2
		out.Write([]byte{byte(length >> 8), byte(length)})
		out.Write(payload)
		return true
	})
	if err != nil {
		return nil, err
	}

	// Everything from the scan header onwards is copied verbatim.
	if scanStart < len(data) {
		out.Write(data[scanStart:])
	}
	return out.Bytes(), nil
}

// --- PNG --------------------------------------------------------------------

// pngMetadataChunks are the chunk types that carry metadata. eXIf is the PNG
// home of the same EXIF block a JPEG keeps in APP1; the text chunks carry
// arbitrary annotations, including whatever an editor decided to record.
var pngMetadataChunks = map[string]bool{
	"eXIf": true,
	"tEXt": true,
	"iTXt": true,
	"zTXt": true,
}

const pngChunkHeaderLen = 8 // 4-byte length + 4-byte type
const pngChunkCRCLen = 4

func stripPNG(data []byte) (*Result, error) {
	orientation, err := pngOrientation(data)
	if err != nil {
		return nil, err
	}

	if orientation.NeedsTransform() {
		if err := CheckDimensions(data, MaxPixels); err != nil {
			return nil, err
		}
		src, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("images: decode png for rotation: %w", err)
		}
		var out bytes.Buffer
		if err := png.Encode(&out, applyOrientation(src, orientation)); err != nil {
			return nil, fmt.Errorf("images: re-encode png: %w", err)
		}
		return &Result{Data: out.Bytes(), Format: FormatPNG, Orientation: orientation, ReEncoded: true}, nil
	}

	stripped, err := dropPNGMetadataChunks(data)
	if err != nil {
		return nil, err
	}
	return &Result{Data: stripped, Format: FormatPNG, Orientation: orientation, ReEncoded: false}, nil
}

// walkPNGChunks calls visit for each chunk. visit receives the type and the
// whole chunk including its length header and CRC, so a caller copying chunks
// verbatim keeps their CRCs valid without recomputing anything.
func walkPNGChunks(data []byte, visit func(chunkType string, whole []byte) error) error {
	if !isPNG(data) {
		return ErrUnsupportedFormat
	}

	i := len(pngSignature)
	for i+pngChunkHeaderLen <= len(data) {
		length := int(data[i])<<24 | int(data[i+1])<<16 | int(data[i+2])<<8 | int(data[i+3])
		if length < 0 {
			return errors.New("images: png chunk length out of range")
		}
		end := i + pngChunkHeaderLen + length + pngChunkCRCLen
		if end > len(data) {
			return errors.New("images: truncated png chunk")
		}
		chunkType := string(data[i+4 : i+8])

		if err := visit(chunkType, data[i:end]); err != nil {
			return err
		}

		i = end
		if chunkType == "IEND" {
			return nil
		}
	}
	return nil
}

func pngOrientation(data []byte) (Orientation, error) {
	orientation := OrientationNormal

	err := walkPNGChunks(data, func(chunkType string, whole []byte) error {
		if chunkType != "eXIf" {
			return nil
		}
		payload := whole[pngChunkHeaderLen : len(whole)-pngChunkCRCLen]
		// A PNG eXIf chunk holds the TIFF block directly, without the
		// "Exif\0\0" prefix a JPEG APP1 segment carries. Some writers include
		// it anyway, so tolerate both.
		if len(payload) >= len(exifHeader) && bytes.Equal(payload[:len(exifHeader)], exifHeader) {
			payload = payload[len(exifHeader):]
		}
		orientation = orientationFromEXIF(payload)
		return nil
	})
	if err != nil {
		return OrientationNormal, err
	}
	return orientation, nil
}

func dropPNGMetadataChunks(data []byte) ([]byte, error) {
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.Write(pngSignature)

	err := walkPNGChunks(data, func(chunkType string, whole []byte) error {
		if pngMetadataChunks[chunkType] {
			return nil
		}
		out.Write(whole)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// CheckDimensions rejects an image whose pixel count exceeds maxPixels,
// reading only the header.
//
// This is the guard to use for validation. It costs a header parse rather than
// a full decode, which matters because the upright path exists precisely to
// avoid decoding a 50MP shelf photo at all — validating with a full decode
// would pay that cost anyway and throw the pixels away.
func CheckDimensions(data []byte, maxPixels int) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("images: read dimensions: %w", err)
	}
	if maxPixels > 0 && cfg.Width*cfg.Height > maxPixels {
		return fmt.Errorf("images: %dx%d exceeds the %d pixel limit", cfg.Width, cfg.Height, maxPixels)
	}
	return nil
}

// Decode is a bounded decode, for callers that actually need the pixels.
func Decode(data []byte, maxPixels int) (image.Image, error) {
	if err := CheckDimensions(data, maxPixels); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("images: decode: %w", err)
	}
	return img, nil
}
