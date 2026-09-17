package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

// ErrUnusableMask is a segmentation mask that cannot be applied: not an image,
// or one that keeps no pixel of the subject at all. Background removal is
// optional, so either way the caller keeps the original picture
// (docs/specs/09-consumption-logging.md).
var ErrUnusableMask = errors.New("images: segmentation mask is unusable")

// maxMaskPixels bounds a mask's decode. A mask is a probability map the model
// draws at a few hundred pixels a side; anything near this size is not one.
const maxMaskPixels = 16_000_000

// Mask values between these two are the model's uncertain edge. Below
// maskTransparent a pixel is background, above maskOpaque it is subject, and in
// between it fades — a soft edge rather than the staircase a single threshold
// leaves around a round jar.
const (
	maskTransparent = 96
	maskOpaque      = 160
)

// Cutout cuts the subject out of a picture: the region box selects, with every
// pixel the mask marks as background made transparent. The result is always a
// PNG, since JPEG has no transparency.
//
// box and mask come from the model (vision.Segmentation); every pixel comes
// from picture. Nothing is redrawn, so a label on the product reads exactly as
// it did in the photo — which is the point of masking rather than asking an
// image model to regenerate the picture.
//
// Like ProductImage, the picture is stripped first, so a rotation still waiting
// in its metadata is applied before the box — drawn on upright pixels — is.
func Cutout(picture []byte, box Box, mask []byte) (*Result, error) {
	stripped, err := Strip(picture)
	if err != nil {
		return nil, err
	}
	img, err := Decode(stripped.Data, MaxPixels)
	if err != nil {
		return nil, err
	}
	rect, err := cropRect(img.Bounds(), box)
	if err != nil {
		return nil, err
	}

	probability, err := Decode(mask, maxMaskPixels)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnusableMask, err)
	}
	// The model draws the mask at a size of its own choosing; stretched over
	// the box it covers the same region the box does.
	w, h := rect.Dx(), rect.Dy()
	scaled := image.NewGray(image.Rect(0, 0, w, h))
	xdraw.BiLinear.Scale(scaled, scaled.Bounds(), probability, probability.Bounds(), xdraw.Src, nil)

	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	kept := false
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			alpha := maskAlpha(scaled.GrayAt(x, y).Y)
			if alpha == 0 {
				continue // NewNRGBA is already transparent
			}
			c := color.NRGBAModel.Convert(img.At(rect.Min.X+x, rect.Min.Y+y)).(color.NRGBA)
			c.A = uint8(uint32(c.A) * uint32(alpha) / 255)
			out.SetNRGBA(x, y, c)
			kept = true
		}
	}
	if !kept {
		return nil, fmt.Errorf("%w: it keeps nothing of the subject", ErrUnusableMask)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, fitWithin(out, MaxProductImageSide)); err != nil {
		return nil, fmt.Errorf("images: encode cutout: %w", err)
	}
	return &Result{Data: buf.Bytes(), Format: FormatPNG, Orientation: stripped.Orientation, ReEncoded: true}, nil
}

// maskAlpha maps a mask probability to an alpha value: transparent at or below
// maskTransparent, opaque at or above maskOpaque, linear in between.
func maskAlpha(v uint8) uint8 {
	switch {
	case v <= maskTransparent:
		return 0
	case v >= maskOpaque:
		return 255
	default:
		return uint8((uint32(v) - maskTransparent) * 255 / (maskOpaque - maskTransparent))
	}
}
