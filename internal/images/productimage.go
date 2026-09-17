package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

// MaxProductImageSide bounds the longer edge of a product picture cut from a
// user's photo. A product image is a thumbnail next to a name in a list; a
// 12-megapixel phone photo stored as one would cost every list render several
// megabytes, on a NAS, over whatever connection the phone has.
const MaxProductImageSide = 1024

// ErrEmptyCrop is a box that selects no pixels once clamped to the image.
var ErrEmptyCrop = errors.New("images: crop box selects no pixels")

// Box is a region of an image in normalized coordinates: X and Width are
// fractions of the image's width, Y and Height of its height. It is the shape
// the vision model reports a detection's bounding_box in
// (docs/specs/06-vision-shelf-ingestion.md).
type Box struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ProductImage turns a user's photo into a product picture: the whole photo
// when box is nil, or the region box selects.
//
// Strip runs first, and that order is the point. The box was drawn by a model
// looking at upright pixels, so cropping pixels that still carry an
// unapplied EXIF rotation would cut out the wrong part of the photo — and
// dropping the metadata before applying it would lose the rotation for good.
// Photos from the upload path are already stripped, which makes this pass the
// cheap lossless one; it is here so the rule holds for any caller, not only
// the ones that happen to have stripped already.
//
// The result is re-encoded from pixels, in the source's format, so it carries
// no metadata of its own whatever the input did.
func ProductImage(data []byte, box *Box) (*Result, error) {
	stripped, err := Strip(data)
	if err != nil {
		return nil, err
	}
	img, err := Decode(stripped.Data, MaxPixels)
	if err != nil {
		return nil, err
	}

	if box != nil {
		rect, err := cropRect(img.Bounds(), *box)
		if err != nil {
			return nil, err
		}
		img = subImage(img, rect)
	}
	img = fitWithin(img, MaxProductImageSide)

	var out bytes.Buffer
	switch stripped.Format {
	case FormatPNG:
		err = png.Encode(&out, img)
	default:
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return nil, fmt.Errorf("images: encode product image: %w", err)
	}

	return &Result{
		Data:        out.Bytes(),
		Format:      stripped.Format,
		Orientation: stripped.Orientation,
		ReEncoded:   true,
	}, nil
}

// cropRect maps a normalized box onto pixel bounds, clamped to the image.
//
// Clamped rather than refused: a model reports boxes that overhang the frame
// by a fraction of a percent often enough, and the item is still plainly in
// the part that is inside it. A box that is entirely outside, has no area, or
// is not a number at all, selects nothing and is refused.
//
// A negative size is refused before any arithmetic, not left to the
// rectangle: image.Rect swaps inverted corners, so a box running backwards
// from (0.5, 0.5) would otherwise quietly select a region nobody drew.
func cropRect(bounds image.Rectangle, box Box) (image.Rectangle, error) {
	for _, v := range []float64{box.X, box.Y, box.Width, box.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return image.Rectangle{}, ErrEmptyCrop
		}
	}
	if box.Width <= 0 || box.Height <= 0 {
		return image.Rectangle{}, ErrEmptyCrop
	}

	w, h := float64(bounds.Dx()), float64(bounds.Dy())
	x0 := clamp(box.X, 0, 1) * w
	y0 := clamp(box.Y, 0, 1) * h
	x1 := clamp(box.X+box.Width, 0, 1) * w
	y1 := clamp(box.Y+box.Height, 0, 1) * h

	rect := image.Rect(
		bounds.Min.X+int(math.Floor(x0)),
		bounds.Min.Y+int(math.Floor(y0)),
		bounds.Min.X+int(math.Ceil(x1)),
		bounds.Min.Y+int(math.Ceil(y1)),
	).Intersect(bounds)

	if rect.Empty() {
		return image.Rectangle{}, ErrEmptyCrop
	}
	return rect, nil
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// subImage crops without copying where the decoded type allows it, which the
// standard decoders' types all do.
func subImage(img image.Image, rect image.Rectangle) image.Image {
	if s, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return s.SubImage(rect)
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	xdraw.Copy(dst, image.Point{}, img, rect, xdraw.Src, nil)
	return dst
}

// fitWithin scales img down so its longer side is at most maxSide. An image
// already within bounds is returned as it is — never scaled up.
func fitWithin(img image.Image, maxSide int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return img
	}

	scale := float64(maxSide) / float64(max(w, h))
	tw := max(1, int(math.Round(float64(w)*scale)))
	th := max(1, int(math.Round(float64(h)*scale)))

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}
