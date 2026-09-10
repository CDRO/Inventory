package images

import "image"

// applyOrientation rewrites the pixels so the image displays upright without
// any metadata telling a viewer to rotate it.
//
// This is the half of stripping that cannot be skipped. Once the EXIF is gone
// nothing remains to say the photo was taken sideways, so the rotation has to
// be baked into the pixels first or it is lost — every portrait phone photo in
// the system ends up on its side, permanently.
func applyOrientation(src image.Image, o Orientation) image.Image {
	if !o.NeedsTransform() {
		return src
	}

	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Orientations 5–8 exchange the axes, so the output is transposed.
	swapped := o == OrientationTranspose || o == OrientationRotate90 ||
		o == OrientationTransverse || o == OrientationRotate270

	outW, outH := w, h
	if swapped {
		outW, outH = h, w
	}

	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := mapPixel(o, x, y, w, h)
			dst.Set(dx, dy, src.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return dst
}

// mapPixel returns where the source pixel (x, y) lands in the output.
//
// The cases are written out rather than composed from flip-and-rotate helpers:
// orientation 5 and 7 are each a mirror *and* a rotation, and expressing them
// as compositions is where sign errors hide. Each line here can be checked
// against the EXIF specification on its own.
func mapPixel(o Orientation, x, y, w, h int) (int, int) {
	switch o {
	case OrientationFlipH:
		return w - 1 - x, y
	case OrientationRotate180:
		return w - 1 - x, h - 1 - y
	case OrientationFlipV:
		return x, h - 1 - y
	case OrientationTranspose:
		return y, x
	case OrientationRotate90:
		return h - 1 - y, x
	case OrientationTransverse:
		return h - 1 - y, w - 1 - x
	case OrientationRotate270:
		return y, w - 1 - x
	default:
		return x, y
	}
}
