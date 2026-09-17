package ingest

import (
	"context"
	"errors"

	"github.com/CDRO/Inventory/internal/images"
	"github.com/CDRO/Inventory/internal/vision"
)

// Segmenter outlines a picture's main subject (internal/vision).
type Segmenter interface {
	Segment(ctx context.Context, model string, image []byte, mimeType string) (*vision.Segmentation, error)
}

// ImageModels reports whether the provider offers a model other than the
// analysis one (internal/vision).
type ImageModels interface {
	Offers(ctx context.Context, model string) bool
	Invalidate()
}

// Backgrounds removes the background from a product picture taken from a
// user's own photo (docs/specs/09-consumption-logging.md, "Optional:
// background removal on a user photo").
//
// It is two steps, split between the model and this code on purpose: the
// model configured as GEMINI_IMAGE_MODEL finds the subject and outlines it
// with a mask, and images.Cutout cuts along that outline. The pixels kept are
// the photo's own, so nothing about the product — least of all the text on its
// label — is ever redrawn by a model.
type Backgrounds struct {
	segmenter Segmenter
	models    ImageModels
	model     string
}

// NewBackgrounds wires background removal to model. An empty model is a
// deployment that never wanted the feature: Available is then always false.
func NewBackgrounds(segmenter Segmenter, models ImageModels, model string) *Backgrounds {
	return &Backgrounds{segmenter: segmenter, models: models, model: model}
}

// Available reports whether background removal can be offered right now, and
// names the model it checked. The model must be configured and listed by the
// provider; a deprecated one is simply not offered, and nothing else in the
// system is affected (docs/specs/01-architecture-and-deployment.md).
func (b *Backgrounds) Available(ctx context.Context) (model string, ok bool) {
	return b.model, b.model != "" && b.models.Offers(ctx, b.model)
}

// Remove cuts the main subject of picture out of its background and returns it
// as a PNG.
//
// A model the provider no longer knows is vision.ErrModelNotFound, and drops
// the cached model list so the next Available says so. A reply that cannot be
// turned into a cutout is vision.ErrMalformedResponse or an images error.
// Every failure means the same thing to the reviewer — the original picture is
// kept — so none of them is worth more than that to the caller.
func (b *Backgrounds) Remove(ctx context.Context, picture []byte, format images.Format) (*images.Result, error) {
	segmentation, err := b.segmenter.Segment(ctx, b.model, picture, format.ContentType())
	if errors.Is(err, vision.ErrModelNotFound) {
		b.models.Invalidate()
	}
	if err != nil {
		return nil, err
	}
	return images.Cutout(picture, images.Box(segmentation.Box), segmentation.Mask)
}
