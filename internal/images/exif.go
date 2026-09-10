package images

import "encoding/binary"

// Orientation is the EXIF orientation tag: how the camera was held, recorded
// as an instruction to rotate the stored pixels before display.
//
// It is the reason metadata cannot simply be deleted. The rotation lives *in*
// the data being dropped, so stripping without applying it first is the
// classic bug that leaves every phone photo sideways
// (docs/specs/04-backend-api-conventions.md).
type Orientation uint16

// The eight defined values. Anything else is treated as OrientationNormal.
const (
	OrientationNormal       Orientation = 1 // no transform
	OrientationFlipH        Orientation = 2 // mirrored left-to-right
	OrientationRotate180    Orientation = 3
	OrientationFlipV        Orientation = 4 // mirrored top-to-bottom
	OrientationTranspose    Orientation = 5 // mirrored, then rotated 90° clockwise
	OrientationRotate90     Orientation = 6 // rotated 90° clockwise
	OrientationTransverse   Orientation = 7 // mirrored, then rotated 270° clockwise
	OrientationRotate270    Orientation = 8 // rotated 270° clockwise
	orientationFirstInvalid Orientation = 9
)

// NeedsTransform reports whether the pixels have to be rewritten.
//
// When they do not, the file can be stripped by segment surgery instead of a
// decode-and-re-encode cycle — which on a 50MP shelf photo is the difference
// between copying bytes and rebuilding the image on a NAS.
func (o Orientation) NeedsTransform() bool {
	return o > OrientationNormal && o < orientationFirstInvalid
}

// tagOrientation is the EXIF tag id for orientation, in IFD0.
const tagOrientation = 0x0112

// exifHeader is the six bytes that begin a JPEG APP1 EXIF payload.
var exifHeader = []byte{'E', 'x', 'i', 'f', 0, 0}

// TIFF field type ids we can read an orientation out of. Orientation is
// specified as SHORT, but a few encoders write LONG, and both are unambiguous.
const (
	typeShort = 3
	typeLong  = 4
)

// tiffHeaderLen is the byte order mark, magic number and IFD0 offset.
const tiffHeaderLen = 8

// ifdEntryLen is the fixed size of one IFD entry: tag, type, count, value.
const ifdEntryLen = 12

// maxIFDEntries bounds the entry loop. Real IFD0s hold a few dozen tags; the
// count field is attacker-controlled, so it is not trusted as a loop bound.
const maxIFDEntries = 512

// orientationFromEXIF reads the orientation out of a bare EXIF payload — the
// bytes after the "Exif\0\0" header in a JPEG APP1 segment, or the whole
// contents of a PNG eXIf chunk.
//
// Every read is bounds-checked against the slice. This parses a file someone
// uploaded, so a truncated or hostile structure has to produce "no
// orientation", never a panic and never a read past the buffer.
func orientationFromEXIF(payload []byte) Orientation {
	if len(payload) < tiffHeaderLen {
		return OrientationNormal
	}

	var order binary.ByteOrder
	switch {
	case payload[0] == 'I' && payload[1] == 'I':
		order = binary.LittleEndian
	case payload[0] == 'M' && payload[1] == 'M':
		order = binary.BigEndian
	default:
		return OrientationNormal
	}

	if order.Uint16(payload[2:4]) != 0x002A {
		return OrientationNormal
	}

	ifdOffset := order.Uint32(payload[4:8])
	// The offset is measured from the start of the TIFF header, and must leave
	// room for at least the entry count.
	if ifdOffset > uint32(len(payload)) || uint64(ifdOffset)+2 > uint64(len(payload)) {
		return OrientationNormal
	}

	entries := order.Uint16(payload[ifdOffset : ifdOffset+2])
	if entries > maxIFDEntries {
		entries = maxIFDEntries
	}

	base := uint64(ifdOffset) + 2
	for i := uint64(0); i < uint64(entries); i++ {
		start := base + i*ifdEntryLen
		if start+ifdEntryLen > uint64(len(payload)) {
			return OrientationNormal
		}
		entry := payload[start : start+ifdEntryLen]

		if order.Uint16(entry[0:2]) != tagOrientation {
			continue
		}

		// Orientation is a single SHORT (or, from sloppy encoders, a LONG).
		// Either way it is small enough to live inline in the value field, so
		// there is no offset to follow and no second bounds problem.
		switch order.Uint16(entry[2:4]) {
		case typeShort:
			return normalizeOrientation(order.Uint16(entry[8:10]))
		case typeLong:
			return normalizeOrientation(uint16(order.Uint32(entry[8:12])))
		default:
			return OrientationNormal
		}
	}
	return OrientationNormal
}

// normalizeOrientation maps anything outside 1–8 onto "no transform".
//
// A file claiming orientation 0 or 99 is malformed; the safe reading is to
// leave the pixels alone rather than guess, because guessing wrong rotates
// somebody's photo for no reason and there is no way for them to correct it.
func normalizeOrientation(raw uint16) Orientation {
	o := Orientation(raw)
	if o < OrientationNormal || o >= orientationFirstInvalid {
		return OrientationNormal
	}
	return o
}
