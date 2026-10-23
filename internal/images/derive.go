package images

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"sort"

	xdraw "golang.org/x/image/draw"
)

// Variant names one derived form of a stored picture
// (docs/specs/43-image-derivatives.md). The set is fixed: a client asks for
// one of these names, never for a size, so no request can make the server
// render anything it did not already decide to.
type Variant string

// The whole-picture variants.
const (
	// VariantPreview is the whole picture, its longest edge at most
	// PreviewSide. Never scaled up: a smaller source keeps its size.
	VariantPreview Variant = "preview"
	// The square thumbnails: the picture's largest centred square at N×N —
	// what `object-fit: cover` would draw from the whole picture, rendered
	// once here instead of by every browser from the full file.
	VariantThumb96  Variant = "thumb-96"
	VariantThumb192 Variant = "thumb-192"
	VariantThumb384 Variant = "thumb-384"
	VariantThumb768 Variant = "thumb-768"
)

// PreviewSide bounds the preview's longest edge.
const PreviewSide = 1600

// derivedJPEGQuality is for derivatives, which are looked at small. Lower
// than jpegQuality, which is for a re-encoded original that may be cut from
// later; the suggestion cache uses the same figure for the same reason.
const derivedJPEGQuality = 82

// thumbSides is the square variants and their side. Also the allow-list.
var thumbSides = map[Variant]int{
	VariantThumb96:  96,
	VariantThumb192: 192,
	VariantThumb384: 384,
	VariantThumb768: 768,
}

// PhotoVariants is every variant of a whole picture, in a fixed order.
var PhotoVariants = []Variant{VariantPreview, VariantThumb96, VariantThumb192, VariantThumb384, VariantThumb768}

// RowVariants is every variant of a detected item's crop: the two sizes a
// 72 px review square needs at one to three device pixels per CSS pixel.
var RowVariants = []Variant{VariantThumb192, VariantThumb384}

// ParseVariant reads a whole-picture variant name. Anything else is false:
// the caller answers 404, exactly as for a picture that does not exist.
func ParseVariant(s string) (Variant, bool) {
	for _, v := range PhotoVariants {
		if string(v) == s {
			return v, true
		}
	}
	return "", false
}

// ParseRowVariant reads a row-crop variant name.
func ParseRowVariant(s string) (Variant, bool) {
	for _, v := range RowVariants {
		if string(v) == s {
			return v, true
		}
	}
	return "", false
}

// Side is a square variant's side, or 0 for the preview.
func (v Variant) Side() int {
	return thumbSides[v]
}

// Derived is one derived picture, encoded.
type Derived struct {
	Data   []byte
	Format Format
	Width  int
	Height int
}

// Set is every variant derived from one source.
type Set map[Variant]Derived

// PhotoSet derives every whole-picture variant of a source from one decode.
//
// Strip runs first for the same reason it does in ProductImage: the rule that
// a box or a crop is applied to upright pixels holds for any caller, not only
// the ones that know their file is already stripped. Every output is encoded
// from pixels, so it carries no metadata whatever the input did.
func PhotoSet(data []byte) (Set, error) {
	stripped, err := Strip(data)
	if err != nil {
		return nil, err
	}
	img, err := Decode(stripped.Data, MaxPixels)
	if err != nil {
		return nil, err
	}

	// A large JPEG is first shrunk by an integer factor with a box filter,
	// which is a plain average over the planes and costs a fraction of what
	// the kernel scaler does on 50 MP: half the time and, because the kernel
	// scaler's working buffer is proportional to the source height, less
	// than half the memory (measured in docs/specs/43-image-derivatives.md).
	// The kernel then does only the fractional remainder. This is the
	// two-stage pipeline libvips calls shrink-on-load.
	src := img
	if yc, ok := img.(*image.YCbCr); ok {
		if f := preShrinkFactor(yc.Bounds().Dx(), yc.Bounds().Dy()); f >= 2 {
			src = boxShrink(yc, f)
		}
	}

	set := make(Set, len(PhotoVariants))
	preview, err := encodeDerived(fitWithin(src, PreviewSide), stripped.Format)
	if err != nil {
		return nil, err
	}
	set[VariantPreview] = preview

	square := centredSquare(src)
	for v, side := range thumbSides {
		d, err := encodeDerived(scaleSquare(square, side), stripped.Format)
		if err != nil {
			return nil, err
		}
		set[v] = d
	}
	return set, nil
}

// RowSets derives the row-crop variants for every box, keyed as boxes is —
// by the proposal's row id. The crop is the box, then its largest centred
// square, then the size: the exact region the model drew, framed as
// `object-fit: cover` frames it in a square.
//
// A box that selects no pixels once clamped to the picture is skipped rather
// than failing the whole set: the row simply has no crop, and the review
// screen shows the whole picture for it, as it does for a row with no box.
func RowSets(data []byte, boxes map[string]Box) (map[string]Set, error) {
	stripped, err := Strip(data)
	if err != nil {
		return nil, err
	}
	img, err := Decode(stripped.Data, MaxPixels)
	if err != nil {
		return nil, err
	}

	// Sorted for a deterministic order of work; the map it returns is not.
	ids := make([]string, 0, len(boxes))
	for id := range boxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make(map[string]Set, len(boxes))
	for _, id := range ids {
		rect, err := cropRect(img.Bounds(), boxes[id])
		if err != nil {
			continue
		}
		square := centredSquare(subImage(img, rect))
		set := make(Set, len(RowVariants))
		for _, v := range RowVariants {
			d, err := encodeDerived(scaleSquare(square, v.Side()), stripped.Format)
			if err != nil {
				return nil, err
			}
			set[v] = d
		}
		out[id] = set
	}
	return out, nil
}

// preShrinkMargin is how much larger than its largest output the pre-shrunk
// picture is kept, so the kernel scaler always has real pixels to work from
// for the last step rather than the box filter's blockier result alone.
const preShrinkMargin = 1.5

// preShrinkFactor is the largest integer shrink that leaves every whole-
// picture variant still oversampled by preShrinkMargin: the preview along the
// longest edge, the largest thumbnail along the shortest. 1 means no
// pre-shrink is worth doing.
func preShrinkFactor(w, h int) int {
	long, short := max(w, h), min(w, h)
	byPreview := float64(long) / (preShrinkMargin * PreviewSide)
	byThumb := float64(short) / (preShrinkMargin * float64(thumbSides[VariantThumb768]))
	f := int(min(byPreview, byThumb))
	if f < 2 {
		return 1
	}
	return f
}

// boxShrink averages every f×f block of a YCbCr picture into one pixel, on
// the planes directly. The result is 4:4:4, whatever the source's subsampling:
// a subsampled chroma plane is simply read several times per block through
// COffset, which stays correct for every ratio the decoder produces.
func boxShrink(src *image.YCbCr, f int) *image.YCbCr {
	b := src.Bounds()
	w, h := b.Dx()/f, b.Dy()/f
	dst := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio444)
	area := f * f
	half := area / 2

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sum := 0
			for dy := 0; dy < f; dy++ {
				i := src.YOffset(b.Min.X+x*f, b.Min.Y+y*f+dy)
				for _, v := range src.Y[i : i+f] {
					sum += int(v)
				}
			}
			dst.Y[y*dst.YStride+x] = uint8((sum + half) / area)
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cb, cr := 0, 0
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					i := src.COffset(b.Min.X+x*f+dx, b.Min.Y+y*f+dy)
					cb += int(src.Cb[i])
					cr += int(src.Cr[i])
				}
			}
			o := y*dst.CStride + x
			dst.Cb[o] = uint8((cb + half) / area)
			dst.Cr[o] = uint8((cr + half) / area)
		}
	}
	return dst
}

// centredSquare is the picture's largest centred square, without copying.
func centredSquare(img image.Image) image.Image {
	b := img.Bounds()
	side := min(b.Dx(), b.Dy())
	if side == b.Dx() && side == b.Dy() {
		return img
	}
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	return subImage(img, image.Rect(x0, y0, x0+side, y0+side))
}

// scaleSquare scales a square picture down to side×side. A square already at
// or below side is returned as it is — never scaled up.
func scaleSquare(square image.Image, side int) image.Image {
	b := square.Bounds()
	if b.Dx() <= side {
		return square
	}
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), square, b, xdraw.Src, nil)
	return dst
}

// encodeDerived encodes a derivative: JPEG, unless the source was a PNG and
// the pixels are not all opaque, in which case the transparency is kept and
// the result is a PNG. An opaque PNG — a screenshot, a photo saved as PNG —
// would only be several times larger for nothing.
func encodeDerived(img image.Image, source Format) (Derived, error) {
	var buf bytes.Buffer
	format := FormatJPEG
	if source == FormatPNG && !isOpaque(img) {
		format = FormatPNG
		if err := png.Encode(&buf, img); err != nil {
			return Derived{}, fmt.Errorf("images: encode derived png: %w", err)
		}
	} else if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: derivedJPEGQuality}); err != nil {
		return Derived{}, fmt.Errorf("images: encode derived jpeg: %w", err)
	}
	b := img.Bounds()
	return Derived{Data: buf.Bytes(), Format: format, Width: b.Dx(), Height: b.Dy()}, nil
}

// isOpaque reports whether every pixel is fully opaque. The standard image
// types all answer this themselves; anything that cannot is taken as opaque,
// which only ever costs a JPEG where a PNG was possible, never the reverse.
func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return true
}
